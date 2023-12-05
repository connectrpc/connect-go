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

//go:build !race
// +build !race

package connect_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
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

	// This run creates a new connection for each RPC to verify that timeouts during dialing
	// won't cause issues. This is historically easier to reproduce, so it uses a smaller
	// duration, no concurrency, and fewer iterations. This is important because if we used
	// a new connection for each RPC in the bigger test scenario below, we'd encounter other
	// issues related to overwhelming the loopback interface and exhausting ephemeral ports.
	t.Run("dial", func(t *testing.T) {
		t.Parallel()
		testClientDeadlineBruteForceLoop(t,
			5*time.Second, 5, 1,
			func(t *testing.T, ctx context.Context) error {
				transport := svr.Client().Transport.(*http.Transport)
				httpClient := &http.Client{
					Transport: transport.Clone(),
				}
				client := pingv1connect.NewPingServiceClient(httpClient, svr.URL)
				_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{Text: "foo"}))
				httpClient.CloseIdleConnections()
				// Make sure to give a little time for the OS to release socket resources
				// to prevent resource exhaustion.
				time.Sleep(time.Millisecond / 2)
				return err
			},
		)
	})

	// This run is much creates significantly more load than the above one, but re-uses
	// connections. It also uses all stream types to send messages, to make sure that all
	// stream implementations handle deadlines correctly. The I/O errors related to
	// deadlines are historically harder to reproduce, so it throws a lot more effort into
	// reproducing, particularly a longer duration for which it will run. It also uses
	// larger messages (by packing requests with unrecognized fields) and compression, to
	// make it more likely to encounter the deadline in the middle of read and write
	// operations.
	t.Run("read-write", func(t *testing.T) {
		t.Parallel()

		var extraField []byte
		extraField = protowire.AppendTag(extraField, 999, protowire.BytesType)
		extraData := make([]byte, 16*1024)
		_, err := rand.Read(extraData) // use good random data so it's not very compressible
		if err != nil {
			t.Fatalf("failed to generate extra payload: %v", err)
			return
		}
		extraField = protowire.AppendBytes(extraField, extraData)

		client := pingv1connect.NewPingServiceClient(svr.Client(), svr.URL, connect.WithSendGzip())
		var count atomic.Int32
		testClientDeadlineBruteForceLoop(t,
			20*time.Second, 200, runtime.GOMAXPROCS(0),
			func(t *testing.T, ctx context.Context) error {
				var err error
				switch count.Add(1) % 4 {
				case 0:
					_, err = client.Ping(ctx, connect.NewRequest(addUnrecognizedBytes(&pingv1.PingRequest{Text: "foo"}, extraField)))
				case 1:
					stream := client.Sum(ctx)
					for i := 0; i < 3; i++ {
						err = stream.Send(addUnrecognizedBytes(&pingv1.SumRequest{Number: 1}, extraField))
						if err != nil {
							break
						}
					}
					_, err = stream.CloseAndReceive()
				case 2:
					stream, err := client.CountUp(ctx, connect.NewRequest(addUnrecognizedBytes(&pingv1.CountUpRequest{Number: 3}, extraField)))
					if err == nil {
						for stream.Receive() {
						}
						err = stream.Err()
						_ = stream.Close()
					}
				case 3:
					stream := client.CumSum(ctx)
					for i := 0; i < 3; i++ {
						_ = stream.Send(addUnrecognizedBytes(&pingv1.CumSumRequest{Number: 1}, extraField))
						_, err = stream.Receive()
						if err != nil {
							break
						}
					}
					_ = stream.CloseRequest()
					_ = stream.CloseResponse()
				}
				return err
			},
		)
	})
}

func testClientDeadlineBruteForceLoop(
	t *testing.T,
	duration time.Duration,
	iterationsPerDeadline int,
	parallelism int,
	loopBody func(t *testing.T, ctx context.Context) error,
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
						err := loopBody(t, ctx)
						rpcCount.Add(1)
						cancel()
						if err == nil {
							// operation completed before timeout, try again
							continue
						}
						if !assert.Equal(t, connect.CodeOf(err), connect.CodeDeadlineExceeded) {
							var buf bytes.Buffer
							_, _ = fmt.Fprintf(&buf, "actual error: %v\n%#v", err, err)
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
				t.Logf("goroutine %d: repeating duration loop", goroutine)
			}
		}()
	}
	wg.Wait()
	t.Logf("Issued %d RPCs.", rpcCount.Load())
}

func addUnrecognizedBytes[M proto.Message](msg M, data []byte) M {
	msg.ProtoReflect().SetUnknown(data)
	return msg
}
