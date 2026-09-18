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

package connectwebsocket_test

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// faultRecorder collects what the protocol error handler was told.
type faultRecorder struct {
	mu     sync.Mutex
	faults []connectwebsocket.ProtocolFault
	peers  []string
	errors []*connect.Error
}

func (r *faultRecorder) handle(
	info connectwebsocket.SessionInfo,
	fault connectwebsocket.ProtocolFault,
	err *connect.Error,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults = append(r.faults, fault)
	r.peers = append(r.peers, info.PeerAddr)
	r.errors = append(r.errors, err)
}

func (r *faultRecorder) seen() ([]connectwebsocket.ProtocolFault, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]connectwebsocket.ProtocolFault(nil), r.faults...),
		append([]string(nil), r.peers...)
}

// newFaultServer mounts a server whose framing faults are reported to recorder.
func newFaultServer(tb testing.TB, recorder *faultRecorder) *httptest.Server {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	mux := http.NewServeMux()
	connectwebsocket.Mount(mux, server,
		connectwebsocket.WithReadMaxBytes(1024),
		connectwebsocket.WithServerProtocolErrorHandler(recorder.handle),
	)
	httpServer := httptest.NewServer(mux)
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// Each kind of framing fault must be reported under its own classification.
// The Connect code cannot do this: all but one of these are InvalidArgument,
// and telling them apart otherwise means matching message strings.
func TestProtocolFaultsAreClassified(t *testing.T) {
	t.Parallel()
	message, err := proto.Marshal(&pingv1.CumSumRequest{Number: 7})
	assert.Nil(t, err)

	for _, test := range []struct {
		name  string
		flags byte
		// declare overrides the length field; -1 means "tell the truth".
		declare int
		payload []byte
		text    bool
		want    connectwebsocket.ProtocolFault
	}{
		{
			name:    "length shorter than the frame",
			declare: len(message) - 1,
			payload: message,
			want:    connectwebsocket.FaultEnvelopeLength,
		},
		{
			name:    "length longer than the frame",
			declare: len(message) + 1,
			payload: message,
			want:    connectwebsocket.FaultEnvelopeLength,
		},
		{
			name:    "reserved flag bit",
			flags:   0b00010000,
			declare: -1,
			payload: message,
			want:    connectwebsocket.FaultEnvelopeFlags,
		},
		{
			name:    "server-only flag bit",
			flags:   0b00000010,
			declare: -1,
			payload: []byte("{}"),
			want:    connectwebsocket.FaultEnvelopeFlags,
		},
		{
			name:    "Connect-level compression flag",
			flags:   0b00000001,
			declare: -1,
			payload: message,
			want:    connectwebsocket.FaultEnvelopeFlags,
		},
		{
			name:    "message past the read limit",
			declare: -1,
			payload: make([]byte, 4096),
			want:    connectwebsocket.FaultSizeLimit,
		},
		{
			name:    "malformed Leading-Metadata",
			flags:   0b00001000,
			declare: -1,
			payload: []byte("not json"),
			want:    connectwebsocket.FaultMetadata,
		},
		{
			name:    "undecodable payload",
			declare: -1,
			payload: []byte{0xff, 0xff, 0xff, 0xff},
			want:    connectwebsocket.FaultMessageEncoding,
		},
		{
			name: "text frame",
			text: true,
			want: connectwebsocket.FaultFrameType,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recorder := &faultRecorder{}
			httpServer := newFaultServer(t, recorder)
			conn := dialCumSum(t, httpServer, "")

			if test.text {
				assert.Nil(t, conn.Write(t.Context(), websocket.MessageText, []byte("hello")))
			} else {
				declared := len(test.payload)
				if test.declare >= 0 {
					declared = test.declare
				}
				frame := make([]byte, 5+len(test.payload))
				frame[0] = test.flags
				binary.BigEndian.PutUint32(frame[1:5], uint32(declared))
				copy(frame[5:], test.payload)
				assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary, frame))
			}

			// Read the server's verdict, which is what makes the fault
			// observable to the peer as well as to the monitor.
			_, _, readErr := conn.Read(t.Context())
			_ = readErr

			faults, peers := recorder.seen()
			assert.Equal(t, len(faults), 1)
			assert.Equal(t, faults[0], test.want)
			assert.True(t, strings.HasPrefix(peers[0], "127.0.0.1:"))
		})
	}
}

// The handler observes; it cannot suppress. Whatever it does, the peer still
// gets the error and the RPC still fails.
func TestProtocolErrorHandlerCannotSuppress(t *testing.T) {
	t.Parallel()
	recorder := &faultRecorder{}
	httpServer := newFaultServer(t, recorder)
	conn := dialCumSum(t, httpServer, "")

	message, err := proto.Marshal(&pingv1.CumSumRequest{Number: 7})
	assert.Nil(t, err)
	frame := make([]byte, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(message)-1))
	copy(frame[5:], message)
	assert.Nil(t, conn.Write(t.Context(), websocket.MessageBinary, frame))

	_, data, err := conn.Read(t.Context())
	assert.Nil(t, err)
	// An EndStream envelope carrying the error, exactly as without a handler.
	assert.Equal(t, data[0], byte(0b00000010))
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "extra bytes after envelope"))

	faults, _ := recorder.seen()
	assert.Equal(t, len(faults), 1)
}

// A well-behaved peer must not be reported at all: the handler is for faults,
// not for traffic.
func TestProtocolErrorHandlerSilentOnCleanTraffic(t *testing.T) {
	t.Parallel()
	recorder := &faultRecorder{}
	httpServer := newFaultServer(t, recorder)

	transport, err := connectwebsocket.NewTransport(
		httpServer.URL,
		connectwebsocket.WithHTTPClient(httpServer.Client()),
	)
	assert.Nil(t, err)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))
	stream, err := client.CumSum(t.Context())
	assert.Nil(t, err)
	assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
	_, err = stream.Receive()
	assert.Nil(t, err)
	assert.Nil(t, stream.CloseSend())
	assert.Nil(t, stream.Close())

	faults, _ := recorder.seen()
	assert.Equal(t, len(faults), 0)
}

// clientFaultRecorder is the client-side twin of faultRecorder.
type clientFaultRecorder struct {
	mu         sync.Mutex
	faults     []connectwebsocket.ProtocolFault
	procedures []string
}

func (r *clientFaultRecorder) handle(
	spec connect.Spec,
	fault connectwebsocket.ProtocolFault,
	_ *connect.Error,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults = append(r.faults, fault)
	r.procedures = append(r.procedures, spec.Procedure)
}

func (r *clientFaultRecorder) seen() ([]connectwebsocket.ProtocolFault, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]connectwebsocket.ProtocolFault(nil), r.faults...),
		append([]string(nil), r.procedures...)
}

// misframingServer accepts an upgrade and writes whatever frame the test wants,
// so the client's read path can be driven the way a real server never would.
func misframingServer(tb testing.TB, write func(*websocket.Conn)) *httptest.Server {
	tb.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(responseWriter http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
				Subprotocols:       []string{"connect.v2+proto"},
				InsecureSkipVerify: true,
			})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			write(conn)
		},
	))
	tb.Cleanup(httpServer.Close)
	return httpServer
}

// A server that frames its responses wrongly is reported to the client's
// handler, classified the same way a client's mistakes are to the server's.
func TestClientProtocolFaultsAreClassified(t *testing.T) {
	t.Parallel()
	message, err := proto.Marshal(&pingv1.CumSumResponse{Sum: 7})
	assert.Nil(t, err)

	for _, test := range []struct {
		name  string
		write func(*websocket.Conn)
		want  connectwebsocket.ProtocolFault
	}{
		{
			name: "length shorter than the frame",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, 0, len(message)-1, message)
			},
			want: connectwebsocket.FaultEnvelopeLength,
		},
		{
			name: "reserved flag bit",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, 0b00010000, len(message), message)
			},
			want: connectwebsocket.FaultEnvelopeFlags,
		},
		{
			name: "client-only flag bit",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, 0b00000100, 0, nil)
			},
			want: connectwebsocket.FaultEnvelopeFlags,
		},
		{
			name: "undecodable payload",
			write: func(conn *websocket.Conn) {
				bad := []byte{0xff, 0xff, 0xff, 0xff}
				writeRawFrame(conn, 0, len(bad), bad)
			},
			want: connectwebsocket.FaultMessageEncoding,
		},
		{
			name: "malformed EndStream message",
			write: func(conn *websocket.Conn) {
				bad := []byte("not json")
				writeRawFrame(conn, 0b00000010, len(bad), bad)
			},
			want: connectwebsocket.FaultMetadata,
		},
		{
			name: "text frame",
			write: func(conn *websocket.Conn) {
				_ = conn.Write(context.Background(), websocket.MessageText, []byte("hello"))
			},
			want: connectwebsocket.FaultFrameType,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			httpServer := misframingServer(t, test.write)
			recorder := &clientFaultRecorder{}
			transport, err := connectwebsocket.NewTransport(
				httpServer.URL,
				connectwebsocket.WithHTTPClient(httpServer.Client()),
				connectwebsocket.WithClientProtocolErrorHandler(recorder.handle),
			)
			assert.Nil(t, err)
			client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

			stream, err := client.CumSum(t.Context())
			assert.Nil(t, err)
			t.Cleanup(func() { _ = stream.Close() })
			assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
			_, err = stream.Receive()
			assert.NotNil(t, err)

			faults, procedures := recorder.seen()
			assert.Equal(t, len(faults), 1)
			assert.Equal(t, faults[0], test.want)
			assert.Equal(t, procedures[0], pingv1connect.PingServiceCumSumProcedure)
		})
	}
}

// writeRawFrame sends one envelope with an arbitrary flags byte and declared
// length, which is how a hostile server misframes a response.
func writeRawFrame(conn *websocket.Conn, flags byte, declared int, payload []byte) {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(declared))
	copy(frame[5:], payload)
	_ = conn.Write(context.Background(), websocket.MessageBinary, frame)
}
