// Copyright 2021-2024 The Connect Authors
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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

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
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	bufferPool := newBufferPool()
	serverURL, _ := url.Parse(server.URL)
	errGetBodyCalled := errors.New("getBodyCalled") // sentinel error
	caller := func(size int) error {
		call := newDuplexHTTPCall(
			context.Background(),
			server.Client(),
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
		for range numWorkers {
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
