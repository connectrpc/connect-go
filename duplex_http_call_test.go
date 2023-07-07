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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	//"golang.org/x/net/http2"
)

func TestDuplexHTTPCallGetBody(t *testing.T) {
	t.Parallel()

	var getBodyCount uint32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The "Connection: close" header is turned into a GOAWAY frame by the http2 server.
		if atomic.LoadUint32(&getBodyCount) == 0 {
			w.Header().Add("Connection", "close")
			w.(http.Flusher).Flush()
		}

		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		_, _ = w.Write(b)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	bufferPool := newBufferPool()
	serverURL, _ := url.Parse(server.URL)

	errGetBodyCalled := fmt.Errorf("getBodyCalled")
	caller := func(id int) error {
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
			t.Log("GOT GetBody()", id)
			atomic.AddUint32(&getBodyCount, 1)
			return getBody()
		}
		_, err := duplexCall.Write([]byte("hello"))
		if err != nil {
			return err
		}
		if err := duplexCall.CloseWrite(); err != nil {
			return err
		}
		_, err = io.ReadAll(duplexCall)
		if err != nil {
			return err
		}
		if getBodyCalled {
			return errGetBodyCalled
		}
		return nil
	}
	workChan := make(chan chan error)
	runner := func(id int) {
		for errChan := range workChan {
			errChan <- caller(id)
		}
	}
	go runner(1)
	go runner(2)

	errChan1 := make(chan error)
	errChan2 := make(chan error)
	for i, gotGetBody := 0, false; !gotGetBody; i++ {
		workChan <- errChan1
		workChan <- errChan2

		t.Log("waiting", i)
		for _, err := range []error{<-errChan1, <-errChan2} {
			if err == errGetBodyCalled {
				t.Log("success", err)
				gotGetBody = true
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}
	close(workChan)
	t.Log("done", atomic.LoadUint32(&getBodyCount))
}
