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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
		name    string
		marker  rune
		text    bool
		payload []byte
		// raw replaces the whole frame, for a message the marker scheme cannot
		// express.
		raw  []byte
		want connectwebsocket.ProtocolFault
	}{
		{
			name:    "unknown marker",
			marker:  'Z',
			payload: message,
			want:    connectwebsocket.FaultMarker,
		},
		{
			name:    "server-only marker",
			marker:  wireServerEndStream,
			text:    true,
			payload: []byte("{}"),
			want:    connectwebsocket.FaultMarker,
		},
		{
			name: "marker outside the BMP",
			// U+1F600, four bytes: rejected on the leading byte alone.
			raw:  append([]byte(string(rune(0x1F600))), message...),
			want: connectwebsocket.FaultMarker,
		},
		{
			name: "empty message",
			raw:  []byte{},
			want: connectwebsocket.FaultMarker,
		},
		{
			name:    "message past the read limit",
			marker:  wireBody,
			payload: make([]byte, 4096),
			want:    connectwebsocket.FaultSizeLimit,
		},
		{
			name:    "malformed metadata",
			marker:  wireMetadata,
			text:    true,
			payload: []byte("not json"),
			want:    connectwebsocket.FaultMetadata,
		},
		{
			name:    "undecodable payload",
			marker:  wireBody,
			payload: []byte{0xff, 0xff, 0xff, 0xff},
			want:    connectwebsocket.FaultMessageEncoding,
		},
		{
			name:   "empty body in a text frame",
			marker: wireBody,
			text:   true,
			want:   connectwebsocket.FaultFrameType,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recorder := &faultRecorder{}
			httpServer := newFaultServer(t, recorder)
			conn := dialCumSum(t, httpServer, "")

			frame := test.raw
			if frame == nil {
				frame = append([]byte(string(test.marker)), test.payload...)
			}
			messageType := websocket.MessageBinary
			if test.text {
				messageType = websocket.MessageText
			}
			assert.Nil(t, conn.Write(t.Context(), messageType, frame))

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
	writeRawFrame(conn, false, 'Z', message)

	data := readServerOpening(t, conn)
	// An S message carrying the error, exactly as without a handler.
	assert.Equal(t, rune(data[0]), wireServerEndStream)
	assert.True(t, strings.Contains(string(data), "invalid_argument"))
	assert.True(t, strings.Contains(string(data), "unknown marker Z"))

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
				Subprotocols:       []string{"connectrpc.1+proto"},
				InsecureSkipVerify: true,
			})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			// Every response stream opens with an M, so the malformed message
			// has to come behind a well-formed one. Without this the client
			// refuses the opening itself, and every case below would report
			// the same metadata fault rather than the one it provokes.
			_ = conn.Write(request.Context(), websocket.MessageText, []byte("M{}"))
			write(conn)
			// Stay up until the client is done. Closing straight after the
			// write would race the client's own frames, and the test would see
			// a write failure rather than the fault it asked for.
			readCtx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
			defer cancel()
			for {
				if _, _, err := conn.Read(readCtx); err != nil {
					return
				}
			}
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
			name: "unknown marker",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, false, 'Z', message)
			},
			want: connectwebsocket.FaultMarker,
		},
		{
			name: "client-only marker",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, true, wireClientEndStream, nil)
			},
			want: connectwebsocket.FaultMarker,
		},
		{
			name: "marker outside the BMP",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, false, rune(0x1F600), message)
			},
			want: connectwebsocket.FaultMarker,
		},
		{
			name: "undecodable payload",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, false, wireBody, []byte{0xff, 0xff, 0xff, 0xff})
			},
			want: connectwebsocket.FaultMessageEncoding,
		},
		{
			name: "malformed EndStream message",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, true, wireServerEndStream, []byte("not json"))
			},
			want: connectwebsocket.FaultMetadata,
		},
		{
			name: "empty body in a text frame",
			write: func(conn *websocket.Conn) {
				writeRawFrame(conn, true, wireBody, nil)
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

// writeRawFrame sends one message with an arbitrary marker and frame type,
// which is how a misframing server produces a mistake the transport would not.
func writeRawFrame(conn *websocket.Conn, text bool, marker rune, payload []byte) {
	frame := append([]byte(string(marker)), payload...)
	messageType := websocket.MessageBinary
	if text {
		messageType = websocket.MessageText
	}
	_ = conn.Write(context.Background(), messageType, frame)
}
