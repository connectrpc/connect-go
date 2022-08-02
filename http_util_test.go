// Copyright 2021-2022 Buf Technologies, Inc.
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
)

func Test_WriteError(t *testing.T) {
	t.Run("successfully encodes connect error", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		connectErr := NewError(CodeInvalidArgument, errors.New("something went wrong"))
		if err := WriteError(w, connectErr); err != nil {
			t.Fatalf(err.Error())
		}
		resp := w.Result()
		assert.Equal(t, resp.StatusCode, 400)
		respErr, readErr := readConnectErr(resp)
		if readErr != nil {
			t.Fatalf(readErr.Error())
		}
		assert.Equal(t, respErr.Code, CodeInvalidArgument)
		assert.Equal(t, respErr.Message, connectErr.Message())
	})
	t.Run("successfully encodes non-connect error", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		err := errors.New("something went wrong")
		if err := WriteError(w, err); err != nil {
			t.Fatalf(err.Error())
		}
		resp := w.Result()
		assert.Equal(t, resp.StatusCode, 500)
		respErr, readErr := readConnectErr(resp)
		if readErr != nil {
			t.Fatalf(readErr.Error())
		}
		assert.Equal(t, respErr.Code, CodeUnknown)
		assert.Equal(t, respErr.Message, err.Error())
	})
}

func readConnectErr(resp *http.Response) (*connectWireError, error) {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	respErr := &connectWireError{}
	if err = json.Unmarshal(data, respErr); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return respErr, nil
}
