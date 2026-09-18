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

// Command server monitors clients that send malformed frames.
//
// A framing fault is reported to the offending client in the end-of-stream
// envelope, which means it is a *successful* outcome from the server's point of
// view: it never reaches the logger, and a Session wrapper sees a nil error. An
// interceptor does see it, but only as InvalidArgument — the same code every
// framing fault carries. WithServerProtocolErrorHandler is the hook that says
// which kind of mistake it was and who made it.
//
// Run this, then the matching client, which misframes on purpose.
//
// The client watches for the opposite fault — a server that misframes its
// responses — with WithClientProtocolErrorHandler. To make that half fire,
// start this server with -misframe, which answers with malformed frames
// instead of mounting the real service.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectwebsocket"
	"connectrpc.com/connect/v2/internal/example/websocket/protocol_errors/misframe"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (pingServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		total += req.Number
		if err := stream.Send(&v1.CumSumResponse{Sum: total}); err != nil {
			return err
		}
	}
}

// faultMonitor is the shape a real deployment would give this: a counter per
// peer and per kind of mistake, so a client that consistently mis-states
// envelope lengths stands out from one that trips over a flag bit once.
type faultMonitor struct {
	mu     sync.Mutex
	counts map[string]map[connectwebsocket.ProtocolFault]int
}

func newFaultMonitor() *faultMonitor {
	return &faultMonitor{counts: make(map[string]map[connectwebsocket.ProtocolFault]int)}
}

// observe is the ProtocolErrorHandler. It returns nothing: the error reaches
// the offending client either way, so a monitor cannot accidentally swallow a
// fault by mishandling it.
//
// It runs on the connection's read path, so it must not block. Counting under
// a mutex is fine; exporting to a metrics backend should be asynchronous.
func (m *faultMonitor) observe(
	info connectwebsocket.SessionInfo,
	fault connectwebsocket.ProtocolFault,
	err *connect.Error,
) {
	// Keyed on the host, not on PeerAddr: that carries the ephemeral port, so
	// counting by it would start over on every connection — and one connection
	// carries one RPC, so a repeat offender would never register. A deployment
	// with authenticated clients should key on the account instead, which
	// info.Request makes reachable.
	client := info.PeerAddr
	if host, _, splitErr := net.SplitHostPort(client); splitErr == nil {
		client = host
	}

	m.mu.Lock()
	byFault, ok := m.counts[client]
	if !ok {
		byFault = make(map[connectwebsocket.ProtocolFault]int)
		m.counts[client] = byFault
	}
	byFault[fault]++
	total := byFault[fault]
	m.mu.Unlock()

	log.Printf("protocol fault from %s on %s: %s (#%d for this client) — %v",
		client, info.Request.URL.Path, fault, total, err)
}

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	misframing := flag.Bool("misframe", false,
		"answer with malformed frames, to exercise the client's error handler")
	flag.Parse()

	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	monitor := newFaultMonitor()

	mux := http.NewServeMux()
	if *misframing {
		log.Println("misframing mode: responses are deliberately malformed")
		mux.Handle(pingv1connect.PingServiceCumSumProcedure, http.HandlerFunc(misframe.Handler))
	} else {
		connectwebsocket.Mount(mux, server,
			connectwebsocket.WithServerProtocolErrorHandler(monitor.observe),
		)
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true) // an upgrade needs a hijackable connection
	httpServer := &http.Server{Addr: *addr, Handler: mux, Protocols: protocols}
	log.Println("listening on", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
