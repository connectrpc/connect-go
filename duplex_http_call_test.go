// Copyright 2021-2026 The Connect Authors
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

package connect

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/internal/assert"
)

// TestHTTPCallGetBody tests that the client is able to retry requests on
// connection close errors. It will initialize a closing handler and ensure
// http.Request.GetBody is successfully called to replay the request.
func TestHTTPCallGetBody(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		// The "Connection: close" header is turned into a GOAWAY frame by the http2 server.
		responseWriter.Header().Add("Connection", "close")
		_, _ = io.Copy(responseWriter, request.Body)
		_ = request.Body.Close()
	})
	// Must use httptest for this test.
	server := httptest.NewUnstartedServer(handler)
	svrProtos := new(http.Protocols)
	svrProtos.SetHTTP1(true)
	svrProtos.SetUnencryptedHTTP2(true)
	server.Config.Protocols = svrProtos
	server.Start()
	t.Cleanup(server.Close)

	clientProtos := new(http.Protocols)
	clientProtos.SetUnencryptedHTTP2(true)
	client := server.Client()
	transport, ok := client.Transport.(*http.Transport)
	assert.True(t, ok)
	transport.Protocols = clientProtos

	bufferPool := newBufferPool()
	serverURL, _ := url.Parse(server.URL)
	errGetBodyCalled := errors.New("getBodyCalled") // sentinel error
	caller := func(size int) error {
		call := newDuplexHTTPCall(
			t.Context(),
			client,
			serverURL,
			Spec{StreamType: StreamTypeUnary},
			http.Header{},
		)
		getBodyCalled := false
		call.onRequestSend = func(*http.Request) {
			getBody := call.request.GetBody
			call.request.GetBody = func() (io.ReadCloser, error) {
				getBodyCalled = true
				rdcloser, err := getBody()
				assert.Nil(t, err)
				return rdcloser, err
			}
		}
		// SetValidateResponse must be set.
		call.SetValidateResponse(func(*http.Response) *Error {
			return nil
		})
		buf := bufferPool.Get()
		defer bufferPool.Put(buf)
		buf.Write(make([]byte, size))
		_, err := call.Send(bytes.NewReader(buf.Bytes()))
		assert.Nil(t, err)
		assert.Nil(t, call.CloseWrite())
		buf.Reset()
		_, err = io.Copy(buf, call)
		assert.Nil(t, err)
		assert.Equal(t, buf.Len(), size)
		if getBodyCalled {
			return errGetBodyCalled
		}
		return nil
	}
	type work struct {
		size int
		errs chan error
	}
	numWorkers := 2
	workChan := make(chan work)
	wg := sync.WaitGroup{}
	wg.Add(numWorkers)
	for range numWorkers {
		go func() {
			for work := range workChan {
				work.errs <- caller(work.size)
			}
			wg.Done()
		}()
	}
	for i, gotGetBody := 0, false; !gotGetBody; i++ {
		errs := make([]chan error, numWorkers)
		for i := range numWorkers {
			errs[i] = make(chan error, 1)
			workChan <- work{size: 512, errs: errs[i]}
		}
		t.Log("waiting", i)
		for _, errChan := range errs {
			if err := <-errChan; err != nil {
				if errors.Is(err, errGetBodyCalled) {
					gotGetBody = true
				} else {
					t.Fatal(err)
				}
			}
		}
	}
	close(workChan)
	wg.Wait()
}

// TestDuplexHTTPCallSendCloseWriteNoNilDeref is a regression test for a nil
// pointer dereference in duplexHTTPCall when Send and CloseWrite are invoked
// concurrently on a client-streaming call.
//
// The failure mode: Send previously initialised requestBodyWriter only in
// the branch that won the CompareAndSwap of requestSent. CloseWrite could
// win the same CAS first and return without ever initialising the pipe; a
// subsequent Send then observed isFirst=false, skipped the setup, and ran
// payload.WriteTo(d.requestBodyWriter) with requestBodyWriter == nil,
// crashing at io.(*PipeWriter).Write.
//
// Concurrent Send and CloseWrite are explicitly expected to be safe (see
// the "This runs concurrently with Write and CloseWrite" comment on
// duplexHTTPCall.makeRequest), so the nil-deref is a bug in duplexHTTPCall.
//
// With the fix, requestBodyWriter is initialised in newDuplexHTTPCall for
// client-streaming and bidi calls and is therefore never observable as nil.
// This test provokes the bad ordering deterministically by delaying the
// sender goroutine's first Send until after the main goroutine has called
// CloseWrite.
func TestDuplexHTTPCallSendCloseWriteNoNilDeref(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		// Drain the request body so Send on the client side doesn't block.
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
		responseWriter.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	assert.Nil(t, err)

	// A handful of iterations is enough: the bad ordering is forced
	// deterministically below.
	const iterations = 5
	for range iterations {
		call := newDuplexHTTPCall(
			t.Context(),
			server.Client(),
			serverURL,
			Spec{StreamType: StreamTypeClient},
			http.Header{},
		)
		call.SetValidateResponse(func(*http.Response) *Error { return nil })

		// Start a goroutine that issues a Send after a short delay. The delay
		// lets the main goroutine's CloseWrite win the CAS on requestSent
		// first. The late Send then observes isFirst=false and - in the buggy
		// implementation - reads requestBodyWriter==nil and nil-derefs.
		sendDone := make(chan struct{})
		go func() {
			defer close(sendDone)
			time.Sleep(50 * time.Millisecond)
			// Ignore the error: with the bug present this panics before
			// returning; with the fix it returns nil or io.EOF depending on
			// whether CloseWrite has already completed.
			_, _ = call.Send(bytes.NewReader([]byte{1}))
		}()

		assert.Nil(t, call.CloseWrite())

		// Wait for the sender goroutine to finish. With the bug present it
		// will have panicked already; with the fix it returns cleanly.
		<-sendDone
		_ = call.CloseRead()
	}
}
