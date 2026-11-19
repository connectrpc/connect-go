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

// Command server implements every PingService RPC, covering all four stream
// types, and serves them over both Connect-over-HTTP and
// Connect-over-WebSocket. It exists for interop testing with other Connect
// implementations, such as connect-es in a browser.
//
// A browser page served from another origin needs that origin allowed:
//
//	go run ./websocket/all_streams/server -addr 127.0.0.1:8080 \
//	  -allow-origin http://127.0.0.1:3000
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	v1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

const (
	// requestHeaderKey is echoed back in the Ping-Header response header, so a
	// client can check that its request metadata arrived.
	requestHeaderKey  = "X-Custom"
	responseHeaderKey = "Ping-Header"
	trailerKey        = "Ping-Trailer"
	shutdownTimeout   = 5 * time.Second
)

type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (*pingServer) Ping(ctx context.Context, request *v1.PingRequest) (*v1.PingResponse, error) {
	setResponseMetadata(ctx)
	return &v1.PingResponse{Number: request.GetNumber(), Text: request.GetText()}, nil
}

// Fail returns an error with the requested code, and a trailer alongside it.
func (*pingServer) Fail(ctx context.Context, request *v1.FailRequest) (*v1.FailResponse, error) {
	setResponseMetadata(ctx)
	code := request.GetCode()
	if code < int32(connect.CodeCanceled) || code > int32(connect.CodeUnauthenticated) {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "code %d is not a Connect error code", code)
	}
	return nil, connect.Errorf(connect.Code(code), "failed as requested")
}

func (*pingServer) Sum(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*v1.SumResponse, error) {
	setResponseMetadata(ctx)
	var total int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return &v1.SumResponse{Sum: total}, nil
		}
		if err != nil {
			return nil, err
		}
		total += request.GetNumber()
	}
}

func (*pingServer) CountUp(
	ctx context.Context,
	request *v1.CountUpRequest,
	stream pingv1connect.PingServiceCountUpServerStream,
) error {
	setResponseMetadata(ctx)
	for number := int64(1); number <= request.GetNumber(); number++ {
		if err := stream.Send(&v1.CountUpResponse{Number: number}); err != nil {
			return err
		}
	}
	return nil
}

func (*pingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	setResponseMetadata(ctx)
	var total int64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		total += request.GetNumber()
		if err := stream.Send(&v1.CumSumResponse{Sum: total}); err != nil {
			return err
		}
	}
}

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	allowedOrigins := flag.String(
		"allow-origin",
		"",
		"comma-separated origins allowed to open WebSocket RPCs, in addition to the same origin",
	)
	flag.Parse()
	if err := run(*address, *allowedOrigins); err != nil {
		slog.ErrorContext(context.Background(), "server failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(address, allowedOrigins string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := connect.NewServer(logInterceptor)
	pingv1connect.RegisterPingServiceHandler(server, &pingServer{})
	mux := http.NewServeMux()
	var mountOptions []connecthttp.Option
	if allowedOrigins != "" {
		mountOptions = append(mountOptions, connecthttp.WithWebSocketCheckOrigin(
			newOriginCheck(strings.Split(allowedOrigins, ",")),
		))
	}
	connecthttp.Mount(mux, server, mountOptions...)

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return err
	}
	protocols := new(http.Protocols)
	// A WebSocket upgrade needs a hijackable HTTP/1.1 connection.
	protocols.SetHTTP1(true)
	httpServer := &http.Server{
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: shutdownTimeout,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()
	slog.InfoContext(ctx, "listening", slog.String("addr", listener.Addr().String()))
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// setResponseMetadata echoes the request's X-Custom header as a response
// header and sets a trailer, so clients can check both directions.
func setResponseMetadata(ctx context.Context) {
	info, ok := connect.CallInfoForServerContext(ctx)
	if !ok {
		return
	}
	if value := info.RequestHeader().Get(requestHeaderKey); value != "" {
		info.ResponseHeader().Set(responseHeaderKey, value)
	}
	info.ResponseTrailer().Set(trailerKey, "trailer-value")
}

// newOriginCheck replaces the default same-origin check, so it keeps that
// rule and adds the configured origins to it. A handshake without an Origin
// header is not from a browser and is allowed.
func newOriginCheck(allowedOrigins []string) func(*http.Request) bool {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[strings.ToLower(strings.TrimSpace(origin))] = struct{}{}
	}
	return func(request *http.Request) bool {
		origin := strings.ToLower(request.Header.Get("Origin"))
		if origin == "" {
			return true
		}
		if _, ok := allowed[origin]; ok {
			return true
		}
		_, host, found := strings.Cut(origin, "://")
		return found && strings.EqualFold(host, request.Host)
	}
}

func logInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		protocol := "unknown"
		if info, ok := connect.CallInfoForServerContext(ctx); ok {
			protocol = info.Protocol
		}
		slog.InfoContext(
			ctx,
			"serving",
			slog.String("procedure", spec.Procedure),
			slog.String("protocol", protocol),
		)
		return next(ctx, spec, stream)
	}
}
