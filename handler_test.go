// Copyright 2021-2025 The Connect Authors
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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/internal/assert"
)

// fakeDeadlineSetter records deadlines passed to SetReadDeadline/SetWriteDeadline for testing.
type fakeDeadlineSetter struct {
	readDeadline  *time.Time
	writeDeadline *time.Time
	setReadErr    error
	setWriteErr   error
}

func (f *fakeDeadlineSetter) SetReadDeadline(t time.Time) error {
	f.readDeadline = &t
	return f.setReadErr
}

func (f *fakeDeadlineSetter) SetWriteDeadline(t time.Time) error {
	f.writeDeadline = &t
	return f.setWriteErr
}

// responseWriterWithFailingDeadlines implements http.ResponseWriter and the optional
// SetReadDeadline/SetWriteDeadline methods so that http.ResponseController will call them.
// Returning an error from those methods allows testing that ServeHTTP returns 500.
type responseWriterWithFailingDeadlines struct {
	http.ResponseWriter
	readErr  error
	writeErr error
}

func (w *responseWriterWithFailingDeadlines) SetReadDeadline(time.Time) error  { return w.readErr }
func (w *responseWriterWithFailingDeadlines) SetWriteDeadline(time.Time) error { return w.writeErr }

func TestServeHTTPReturns500WhenDeadlineFailsToSet(t *testing.T) {
	t.Parallel()
	deadlineErr := errors.New("set deadline failed")

	handler := NewUnaryHandler(
		"/test.Service/Method",
		func(context.Context, *Request[struct{}]) (*Response[struct{}], error) {
			return &Response[struct{}]{}, nil
		},
		WithReadTimeout(time.Second),
		WithWriteTimeout(time.Second),
	)

	tests := []struct {
		name     string
		readErr  error
		writeErr error
		wantCode int
	}{
		{"SetReadDeadline error returns 500", deadlineErr, nil, http.StatusInternalServerError},
		{"SetWriteDeadline error returns 500", nil, deadlineErr, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			w := &responseWriterWithFailingDeadlines{
				ResponseWriter: rec,
				readErr:        tt.readErr,
				writeErr:       tt.writeErr,
			}

			req := httptest.NewRequest(http.MethodPost, "http://test/", nil)
			handler.ServeHTTP(w, req)

			assert.Equal(t, rec.Code, tt.wantCode)
		})
	}
}

func TestGetDeadline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		timeout time.Duration
		// wantNil is true when the result should be nil.
		wantNil bool
		// wantZero is true when the result should be a non-nil pointer to time.Time{}.
		wantZero bool
	}{
		{
			name:    "zero returns nil",
			timeout: 0,
			wantNil: true,
		},
		{
			name:     "negative returns zero value",
			timeout:  -1,
			wantZero: true,
		},
		{
			name:    "positive returns future time",
			timeout: 5 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := time.Now()
			got := getDeadline(tt.timeout)
			after := time.Now()

			if tt.wantNil {
				assert.Nil(t, got)
				return
			}

			assert.NotNil(t, got)
			if tt.wantZero {
				assert.Equal(t, *got, time.Time{})
				return
			}

			// Positive timeout: getDeadline uses time.Now() between our before and after,
			// so deadline is in [before+timeout, after+timeout].
			assert.True(t, !got.Before(before.Add(tt.timeout)),
				assert.Sprintf("deadline %v should not be before %v", got, before.Add(tt.timeout)))
			assert.True(t, !got.After(after.Add(tt.timeout)),
				assert.Sprintf("deadline %v should not be after %v", got, after.Add(tt.timeout)))
		})
	}
}

func TestApplyDeadlines(t *testing.T) {
	t.Parallel()
	setErr := errors.New("set deadline failed")
	tests := []struct {
		name         string
		readTimeout  time.Duration
		writeTimeout time.Duration
		setReadErr   error
		setWriteErr  error
		wantErr      bool
		// wantReadSet: was SetReadDeadline called (timeout was non-zero)?
		wantReadSet bool
		// wantWriteSet: was SetWriteDeadline called?
		wantWriteSet bool
	}{
		{
			name:         "both zero neither set",
			readTimeout:  0,
			writeTimeout: 0,
			wantReadSet:  false,
			wantWriteSet: false,
		},
		{
			name:         "read only set",
			readTimeout:  5 * time.Second,
			writeTimeout: 0,
			wantReadSet:  true,
			wantWriteSet: false,
		},
		{
			name:         "write only set",
			readTimeout:  0,
			writeTimeout: time.Second,
			wantReadSet:  false,
			wantWriteSet: true,
		},
		{
			name:         "both set",
			readTimeout:  time.Second,
			writeTimeout: 2 * time.Second,
			wantReadSet:  true,
			wantWriteSet: true,
		},
		{
			name:        "SetReadDeadline error returned",
			readTimeout: time.Second,
			setReadErr:  setErr,
			wantErr:     true,
		},
		{
			name:         "SetWriteDeadline error returned",
			writeTimeout: time.Second,
			setWriteErr:  setErr,
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeDeadlineSetter{setReadErr: tt.setReadErr, setWriteErr: tt.setWriteErr}
			before := time.Now()
			err := applyDeadlines(tt.readTimeout, tt.writeTimeout, fake)
			after := time.Now()
			if tt.wantErr {
				assert.NotNil(t, err)
				return
			}
			assert.Nil(t, err)
			if tt.wantReadSet {
				assert.NotNil(t, fake.readDeadline)
				assert.True(t, !fake.readDeadline.Before(before.Add(tt.readTimeout)),
					assert.Sprintf("read deadline %v before %v", *fake.readDeadline, before.Add(tt.readTimeout)))
				assert.True(t, !fake.readDeadline.After(after.Add(tt.readTimeout)),
					assert.Sprintf("read deadline %v after %v", *fake.readDeadline, after.Add(tt.readTimeout)))
			} else {
				assert.Nil(t, fake.readDeadline)
			}
			if tt.wantWriteSet {
				assert.NotNil(t, fake.writeDeadline)
				assert.True(t, !fake.writeDeadline.Before(before.Add(tt.writeTimeout)),
					assert.Sprintf("write deadline %v before %v", *fake.writeDeadline, before.Add(tt.writeTimeout)))
				assert.True(t, !fake.writeDeadline.After(after.Add(tt.writeTimeout)),
					assert.Sprintf("write deadline %v after %v", *fake.writeDeadline, after.Add(tt.writeTimeout)))
			} else {
				assert.Nil(t, fake.writeDeadline)
			}
		})
	}
}
