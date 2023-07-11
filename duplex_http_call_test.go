// Copyright 2021-2023 Buf Technologies, Inc.
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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDuplexHTTPCallGetBody(t *testing.T) {
	t.Parallel()

	var getBodyCount uint32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		// The "Connection: close" header is turned into a GOAWAY frame by the http2 server.
		if atomic.LoadUint32(&getBodyCount) == 0 {
			responseWriter.Header().Add("Connection", "close")
		}
		b, _ := io.ReadAll(request.Body)
		_ = request.Body.Close()
		_, _ = responseWriter.Write(b)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	bufferPool := newBufferPool()
	serverURL, _ := url.Parse(server.URL)

	errGetBodyCalled := fmt.Errorf("getBodyCalled")
	caller := func(size int) error {
		duplexCall := newDuplexHTTPCall(
			context.Background(),
			server.Client(),
			serverURL,
			Spec{StreamType: StreamTypeUnary},
			http.Header{},
			bufferPool,
		)
		duplexCall.SetValidateResponse(func(*http.Response) *Error {
			return nil
		})
		getBodyCalled := false
		getBody := duplexCall.request.GetBody
		duplexCall.request.GetBody = func() (io.ReadCloser, error) {
			getBodyCalled = true
			rdcloser, err := getBody()
			if err != nil {
				t.Log("getBody failed", err)
				return nil, err
			}
			t.Log("getBodyCalled")
			atomic.AddUint32(&getBodyCount, 1)
			return rdcloser, nil
		}
		_, err := duplexCall.Write(make([]byte, size))
		if err != nil {
			return err
		}
		if err := duplexCall.CloseWrite(); err != nil {
			return err
		}
		body, err := io.ReadAll(duplexCall)
		if err != nil {
			return err
		}
		if len(body) != size {
			return fmt.Errorf("expected %d bytes, got %d", size, len(body))
		}
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
	var wg sync.WaitGroup
	worker := func() {
		for work := range workChan {
			work.errs <- caller(work.size)
		}
		wg.Done()
	}
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go worker()
	}

	for _, size := range []int{512} {
		for i, gotGetBody := 0, false; !gotGetBody; i++ {
			errs := make([]chan error, numWorkers)
			for i := 0; i < numWorkers; i++ {
				errs[i] = make(chan error, 1)
				workChan <- work{size: size, errs: errs[i]}
			}

			t.Log("waiting", i)
			for _, errChan := range errs {
				err := <-errChan
				if errors.Is(err, errGetBodyCalled) {
					gotGetBody = true
				} else if err != nil {
					t.Fatal(err)
				}
			}
		}
		x := atomic.LoadUint32(&getBodyCount)
		if x == 0 {
			t.Fatal("expected getBody to be called at least once")
		}
		atomic.StoreUint32(&getBodyCount, 0)
	}
	close(workChan)
	wg.Wait()
}
