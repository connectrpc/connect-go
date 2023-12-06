// Copyright 2021-2023 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// These tests are not able to reproduce issues with the race detector
// enabled. So they are only build with race detection off. This means
// these tests, for now, are run manually, since CI runs tests with the
// race detector enabled. Since these tests are slow (take 20s), it's
// probably okay that they don't run with every commit.

//go:build !race
// +build !race

package connect_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestClientDeadlineHandling(t *testing.T) {
	t.Parallel()

	_, handler := pingv1connect.NewPingServiceHandler(pingServer{})
	svr := httptest.NewUnstartedServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if req.Context().Err() != nil {
			return
		}
		handler.ServeHTTP(respWriter, req)
	}))
	svr.EnableHTTP2 = true
	svr.StartTLS()
	t.Cleanup(svr.Close)

	// This case creates a new connection for each RPC to verify that timeouts during dialing
	// won't cause issues. This is historically easier to reproduce, so it uses a smaller
	// duration, no concurrency, and fewer iterations. This is important because if we used
	// a new connection for each RPC in the bigger test scenario below, we'd encounter other
	// issues related to overwhelming the loopback interface and exhausting ephemeral ports.
	t.Run("dial", func(t *testing.T) {
		t.Parallel()
		transport, ok := svr.Client().Transport.(*http.Transport)
		if !assert.True(t, ok) {
			t.FailNow()
		}
		testClientDeadlineBruteForceLoop(t,
			5*time.Second, 5, 1,
			func(ctx context.Context) (string, rpcErrors) {
				httpClient := &http.Client{
					Transport: transport.Clone(),
				}
				client := pingv1connect.NewPingServiceClient(httpClient, svr.URL)
				_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{Text: "foo"}))
				// Close all connections and make sure to give a little time for the OS to
				// release socket resources to prevent resource exhaustion (such as running
				// out of ephemeral ports).
				httpClient.CloseIdleConnections()
				time.Sleep(time.Millisecond / 2)
				return pingv1connect.PingServicePingProcedure, rpcErrors{recvErr: err}
			},
		)
	})

	// This case creates significantly more load than the above one, but uses a normal
	// client so pools and re-uses connections. It also uses all stream types to send
	// messages, to make sure that all stream implementations handle deadlines correctly.
	// The I/O errors related to deadlines are historically harder to reproduce, so it
	// throws a lot more effort into reproducing, particularly a longer duration for
	// which it will run. It also uses larger messages (by packing requests with
	// unrecognized fields) and compression, to make it more likely to encounter the
	// deadline in the middle of read and write operations.
	t.Run("read-write", func(t *testing.T) {
		t.Parallel()

		var extraField []byte
		extraField = protowire.AppendTag(extraField, 999, protowire.BytesType)
		extraData := make([]byte, 16*1024)
		// use good random data so it's not very compressible
		if _, err := rand.Read(extraData); err != nil {
			t.Fatalf("failed to generate extra payload: %v", err)
			return
		}
		extraField = protowire.AppendBytes(extraField, extraData)

		clientConnect := pingv1connect.NewPingServiceClient(svr.Client(), svr.URL, connect.WithSendGzip())
		clientGRPC := pingv1connect.NewPingServiceClient(svr.Client(), svr.URL, connect.WithSendGzip(), connect.WithGRPCWeb())
		var count atomic.Int32
		testClientDeadlineBruteForceLoop(t,
			20*time.Second, 200, runtime.GOMAXPROCS(0),
			func(ctx context.Context) (string, rpcErrors) {
				var procedure string
				var errs rpcErrors
				rpcNum := count.Add(1)
				var client pingv1connect.PingServiceClient
				if rpcNum&4 == 0 {
					client = clientConnect
				} else {
					client = clientGRPC
				}
				switch rpcNum & 3 {
				case 0:
					procedure = pingv1connect.PingServicePingProcedure
					_, errs.recvErr = client.Ping(ctx, connect.NewRequest(addUnrecognizedBytes(&pingv1.PingRequest{Text: "foo"}, extraField)))
				case 1:
					procedure = pingv1connect.PingServiceSumProcedure
					stream := client.Sum(ctx)
					for i := 0; i < 3; i++ {
						errs.sendErr = stream.Send(addUnrecognizedBytes(&pingv1.SumRequest{Number: 1}, extraField))
						if errs.sendErr != nil {
							break
						}
					}
					_, errs.recvErr = stream.CloseAndReceive()
				case 2:
					procedure = pingv1connect.PingServiceCountUpProcedure
					var stream *connect.ServerStreamForClient[pingv1.CountUpResponse]
					stream, errs.recvErr = client.CountUp(ctx, connect.NewRequest(addUnrecognizedBytes(&pingv1.CountUpRequest{Number: 3}, extraField)))
					if errs.recvErr == nil {
						for stream.Receive() {
						}
						errs.recvErr = stream.Err()
						errs.closeRecvErr = stream.Close()
					}
				case 3:
					procedure = pingv1connect.PingServiceCumSumProcedure
					stream := client.CumSum(ctx)
					for i := 0; i < 3; i++ {
						errs.sendErr = stream.Send(addUnrecognizedBytes(&pingv1.CumSumRequest{Number: 1}, extraField))
						_, errs.recvErr = stream.Receive()
						if errs.recvErr != nil {
							break
						}
					}
					errs.closeSendErr = stream.CloseRequest()
					errs.closeRecvErr = stream.CloseResponse()
				}
				return procedure, errs
			},
		)
	})
}

func testClientDeadlineBruteForceLoop(
	t *testing.T,
	duration time.Duration,
	iterationsPerDeadline int,
	parallelism int,
	loopBody func(ctx context.Context) (string, rpcErrors),
) {
	t.Helper()
	testContext, testCancel := context.WithTimeout(context.Background(), duration)
	defer testCancel()
	var rpcCount atomic.Int64

	var wg sync.WaitGroup
	for goroutine := 0; goroutine < parallelism; goroutine++ {
		goroutine := goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			// We try a range of timeouts since the timing issue is sensitive
			// to execution environment (e.g. CPU, memory, and network speeds).
			// So the lower timeout values may be more likely to trigger an issue
			// in faster environments; higher timeouts for slower environments.
			const minTimeout = 10 * time.Microsecond
			const maxTimeout = 2 * time.Millisecond
			for {
				for timeout := minTimeout; timeout <= maxTimeout; timeout += 10 * time.Microsecond {
					for i := 0; i < iterationsPerDeadline; i++ {
						if testContext.Err() != nil {
							return
						}
						ctx, cancel := context.WithTimeout(context.Background(), timeout)
						// We are intentionally not inheriting from testContext, which signals when the
						// test loop should stop and return but need not influence the RPC deadline.
						proc, errs := loopBody(ctx) //nolint:contextcheck
						rpcCount.Add(1)
						cancel()
						type errCase struct {
							err      error
							name     string
							allowEOF bool
						}
						errCases := []errCase{
							{
								err:      errs.sendErr,
								name:     "send error",
								allowEOF: true,
							},
							{
								err:  errs.recvErr,
								name: "receive error",
							},
							{
								err:  errs.closeSendErr,
								name: "close-send error",
							},
							{
								err:  errs.closeRecvErr,
								name: "close-receive error",
							},
						}
						for _, errCase := range errCases {
							err := errCase.err
							if err == nil {
								// operation completed before timeout, try again
								continue
							}
							if errCase.allowEOF && errors.Is(err, io.EOF) {
								continue
							}

							if !assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded) {
								var buf bytes.Buffer
								_, _ = fmt.Fprintf(&buf, "actual %v from %s: %v\n%#v", errCase.name, proc, err, err)
								for {
									err = errors.Unwrap(err)
									if err == nil {
										break
									}
									_, _ = fmt.Fprintf(&buf, "\n  caused by: %#v", err)
								}
								t.Log(buf.String())
								testCancel()
							}
						}
					}
				}
				t.Logf("goroutine %d: repeating duration loop", goroutine)
			}
		}()
	}
	wg.Wait()
	t.Logf("Issued %d RPCs.", rpcCount.Load())
}

type rpcErrors struct {
	sendErr      error
	recvErr      error
	closeSendErr error
	closeRecvErr error
}

func addUnrecognizedBytes[M proto.Message](msg M, data []byte) M {
	msg.ProtoReflect().SetUnknown(data)
	return msg
}
