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

package connectinprocess_test

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	pingv1connect "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestInProcessUnary(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	resp, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got, want := resp.GetNumber(), int64(42); got != want {
		t.Errorf("Number = %d, want %d", got, want)
	}
	if got, want := resp.GetText(), "hello"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
}

// TestHandlerNestedCallDropsCallerHeaders verifies a nested outbound call
// from a handler does not inherit the caller's request headers.
func TestHandlerNestedCallDropsCallerHeaders(t *testing.T) {
	t.Parallel()
	probeServer := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(probeServer, probePingServer{})
	probeClient := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(probeServer)))

	frontServer := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(frontServer, frontPingServer{probe: probeClient})
	frontClient := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(frontServer)))

	ctx, callInfo := connect.NewClientContext(t.Context())
	callInfo.RequestHeader().Set("Authorization", "secret")

	resp, err := frontClient.Ping(ctx, &pingv1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.GetText() != "" {
		t.Errorf("nested call carried the caller's Authorization %q; the handler context leaked the client CallInfo", resp.GetText())
	}
}

// TestWithCopyFuncOverride verifies WithCopyFunc replaces the default copier.
func TestWithCopyFuncOverride(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	noop := func(dst, src any) error { return nil }
	transport := connectinprocess.New(handler, connectinprocess.WithCopyFunc(noop))
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	// The no-op copier drops both payloads, so the response is zero-valued.
	resp, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 7, Text: "x"})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.GetNumber() != 0 || resp.GetText() != "" {
		t.Errorf("expected zero-value response with no-op copier, got Number=%d Text=%q", resp.GetNumber(), resp.GetText())
	}
}

// TestTrailerCapturedBeforeDispatchResolvesAfter verifies metadata handles
// captured before the call observe values set during it.
func TestTrailerCapturedBeforeDispatchResolvesAfter(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{
		respHeaders:  map[string]string{"X-Custom-Header": "header-value"},
		respTrailers: map[string]string{"X-Audit-Id": "audit-123"},
	})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, info := connect.NewClientContext(t.Context())
	header := info.ResponseHeader()
	trailer := info.ResponseTrailer()

	if _, err := client.Ping(ctx, &pingv1.PingRequest{}); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if got := header.Get("X-Custom-Header"); got != "header-value" {
		t.Errorf("ResponseHeader captured early: Get = %q, want %q", got, "header-value")
	}
	if got := trailer.Get("X-Audit-Id"); got != "audit-123" {
		t.Errorf("ResponseTrailer captured early: Get = %q, want %q", got, "audit-123")
	}
}

func TestStreamContextCancelAborts(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.CumSum(ctx)
	if err != nil {
		t.Fatalf("CumSum: %v", err)
	}
	cancel()
	if _, err := stream.Receive(); !errors.Is(err, context.Canceled) {
		t.Errorf("Receive after cancel = %v, want context.Canceled", err)
	}
}

// TestStreamCloseSendEOFsSubsequentSend verifies Send after CloseSend returns
// io.EOF while Receive still works.
func TestStreamCloseSendEOFsSubsequentSend(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	transport := connectinprocess.New(handler)
	ctx, _ := connect.NewClientContext(t.Context())
	stream, err := transport.NewClientStream(ctx, connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  pingv1connect.PingServicePingProcedure,
	})
	if err != nil {
		t.Fatalf("NewClientStream: %v", err)
	}
	if err := stream.Send(&pingv1.PingRequest{Number: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.Send(&pingv1.PingRequest{Number: 2}); !errors.Is(err, io.EOF) {
		t.Errorf("Send after CloseSend = %v, want io.EOF", err)
	}
	var resp pingv1.PingResponse
	if err := stream.Receive(&resp); err != nil {
		t.Fatalf("Receive after CloseSend: %v", err)
	}
	if resp.GetNumber() != 1 {
		t.Errorf("Number = %d, want 1", resp.GetNumber())
	}
}

func TestInProcessServerStreaming(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 5})
	if err != nil {
		t.Fatalf("CountUp: %v", err)
	}
	var got []int64
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		got = append(got, msg.GetNumber())
	}
	want := []int64{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestInProcessServerStreamCloseReleasesHandler verifies abandoning a stream
// with Close, rather than reading to io.EOF, unblocks the handler.
func TestInProcessServerStreamCloseReleasesHandler(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, &closeSignalPingServer{done: done})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 1_000})
	if err != nil {
		t.Fatalf("CountUp: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler goroutine did not return after Close")
	}
}
func TestInProcessClientStreaming(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.Sum(t.Context())
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	for _, n := range []int64{1, 2, 3, 4, 5} {
		if err := stream.Send(&pingv1.SumRequest{Number: n}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	res, err := stream.CloseAndReceive()
	if err != nil {
		t.Fatalf("CloseAndReceive: %v", err)
	}
	if got, want := res.GetSum(), int64(15); got != want {
		t.Errorf("Sum = %d, want %d", got, want)
	}
}

func TestInProcessBidiStreaming(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, pingServer{})

	transport := connectinprocess.New(handler)
	client := pingv1connect.NewPingServiceClient(connect.NewClient(transport))

	stream, err := client.CumSum(t.Context())
	if err != nil {
		t.Fatalf("CumSum: %v", err)
	}

	inputs := []int64{10, 20, 30}
	wantSums := []int64{10, 30, 60}
	for i, n := range inputs {
		if err := stream.Send(&pingv1.CumSumRequest{Number: n}); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
		res, err := stream.Receive()
		if err != nil {
			t.Fatalf("Receive[%d]: %v", i, err)
		}
		if got, want := res.GetSum(), wantSums[i]; got != want {
			t.Errorf("Receive[%d].Sum = %d, want %d", i, got, want)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		t.Errorf("final Receive = %v, want io.EOF", err)
	}
}

func TestProtoCopyTypedToTyped(t *testing.T) {
	t.Parallel()
	src := &pingv1.PingRequest{Number: 42, Text: "hello"}
	dst := &pingv1.PingRequest{}
	if err := connectinprocess.ProtoCopy(dst, src); err != nil {
		t.Fatalf("ProtoCopy: %v", err)
	}
	if got, want := dst.GetNumber(), int64(42); got != want {
		t.Errorf("Number = %d, want %d", got, want)
	}
	if got, want := dst.GetText(), "hello"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
}

func TestProtoCopyDynamicToTyped(t *testing.T) {
	t.Parallel()
	desc := (&pingv1.PingRequest{}).ProtoReflect().Descriptor()
	src := dynamicpb.NewMessage(desc)
	src.Set(desc.Fields().ByName("number"), protoreflect.ValueOfInt64(7))
	src.Set(desc.Fields().ByName("text"), protoreflect.ValueOfString("from-dynamic"))

	dst := &pingv1.PingRequest{}
	if err := connectinprocess.ProtoCopy(dst, src); err != nil {
		t.Fatalf("ProtoCopy: %v", err)
	}
	if got, want := dst.GetNumber(), int64(7); got != want {
		t.Errorf("Number = %d, want %d", got, want)
	}
	if got, want := dst.GetText(), "from-dynamic"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
}

func TestProtoCopyTypedToDynamic(t *testing.T) {
	t.Parallel()
	desc := (&pingv1.PingRequest{}).ProtoReflect().Descriptor()
	src := &pingv1.PingRequest{Number: 99, Text: "to-dynamic"}
	dst := dynamicpb.NewMessage(desc)

	if err := connectinprocess.ProtoCopy(dst, src); err != nil {
		t.Fatalf("ProtoCopy: %v", err)
	}
	gotNum := dst.Get(desc.Fields().ByName("number")).Int()
	if gotNum != 99 {
		t.Errorf("Number = %d, want 99", gotNum)
	}
	gotText := dst.Get(desc.Fields().ByName("text")).String()
	if gotText != "to-dynamic" {
		t.Errorf("Text = %q, want %q", gotText, "to-dynamic")
	}
}

func TestProtoCopyRejectsNonProto(t *testing.T) {
	t.Parallel()
	t.Run("dst", func(t *testing.T) {
		t.Parallel()
		notProto := &struct{ X int }{X: 1}
		err := connectinprocess.ProtoCopy(notProto, &pingv1.PingRequest{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "ProtoCopy dst") {
			t.Errorf("error = %q, want mention of dst", err.Error())
		}
	})
	t.Run("src", func(t *testing.T) {
		t.Parallel()
		notProto := &struct{ X int }{X: 1}
		err := connectinprocess.ProtoCopy(&pingv1.PingRequest{}, notProto)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "ProtoCopy src") {
			t.Errorf("error = %q, want mention of src", err.Error())
		}
	})
}

// TestUnaryHandlerPanicRecovered verifies a handler panic surfaces as
// CodeInternal rather than crashing the caller's goroutine.
func TestUnaryHandlerPanicRecovered(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, panicPingServer{})
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(handler)))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	if err == nil {
		t.Fatal("expected an error from a panicking handler")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("code = %s, want CodeInternal", got)
	}
}

// TestUnaryHandlerRemoteErrorScrubbed verifies a forwarded remote error is
// scrubbed to CodeInternal with no message.
func TestUnaryHandlerRemoteErrorScrubbed(t *testing.T) {
	t.Parallel()
	handler := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(handler, remoteErrPingServer{})
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(handler)))

	_, err := client.Ping(t.Context(), &pingv1.PingRequest{})
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("code = %s, want CodeInternal (scrubbed)", got)
	}
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Message() != "" {
		t.Errorf("message = %q, want empty (downstream detail must not leak)", ce.Message())
	}
}

// TestClientStreamCloseAbortsServer verifies Close cancels the stream
// context, so a handler streaming indefinitely returns.
func TestClientStreamCloseAbortsServer(t *testing.T) {
	t.Parallel()
	handlerDone := make(chan struct{})
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, &infiniteCountUpServer{done: handlerDone})
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(server)))

	stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{Number: 1})
	if err != nil {
		t.Fatalf("CountUp: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server was not aborted by Close")
	}
}

// TestClientStreamCloseWithoutUse verifies closing a stream that was never
// sent on or received from does not leak goroutines.
//
//nolint:paralleltest // counts goroutines, which parallel siblings would skew
func TestClientStreamCloseWithoutUse(t *testing.T) {
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(server)))

	before := runtime.NumGoroutine()
	for range 100 {
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatalf("CumSum: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+10 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+10 {
		t.Fatalf("goroutines leaked: %d before, %d after", before, got)
	}
}

// TestSetUnknownHandler exercises the Server.Call fallback for procedures
// with no registered method.
func TestSetUnknownHandler(t *testing.T) {
	t.Parallel()
	const unknownProcedure = "/connect.ping.v1.PingService/DoesNotExist"
	unknownSpec := connect.Spec{
		StreamType: connect.StreamTypeUnary,
		Procedure:  unknownProcedure,
	}
	t.Run("default_unimplemented", func(t *testing.T) {
		t.Parallel()
		server := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(server, pingServer{})
		client := connect.NewClient(connectinprocess.New(server))
		err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{}, &pingv1.PingResponse{})
		if got, want := connect.CodeOf(err), connect.CodeUnimplemented; got != want {
			t.Errorf("CodeOf = %v, want %v", got, want)
		}
	})
	t.Run("fallback_answers", func(t *testing.T) {
		t.Parallel()
		server := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(server, pingServer{})
		var gotSpec connect.Spec
		server.SetUnknownHandler(func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			gotSpec = spec
			var req pingv1.PingRequest
			if err := stream.Receive(&req); err != nil {
				return err
			}
			return stream.Send(&pingv1.PingResponse{Number: req.GetNumber(), Text: "fallback"})
		})
		client := connect.NewClient(connectinprocess.New(server))
		var res pingv1.PingResponse
		if err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{Number: 42}, &res); err != nil {
			t.Fatalf("CallUnary: %v", err)
		}
		if res.GetText() != "fallback" || res.GetNumber() != 42 {
			t.Errorf("response = %d %q, want 42 %q", res.GetNumber(), res.GetText(), "fallback")
		}
		if gotSpec.Procedure != unknownProcedure {
			t.Errorf("Spec.Procedure = %q, want %q", gotSpec.Procedure, unknownProcedure)
		}
		if gotSpec.StreamType != connect.StreamTypeBidi {
			t.Errorf("Spec.StreamType = %v, want %v", gotSpec.StreamType, connect.StreamTypeBidi)
		}
		if gotSpec.Schema != nil {
			t.Errorf("Spec.Schema = %v, want nil", gotSpec.Schema)
		}
	})
	t.Run("wrapped_by_interceptors", func(t *testing.T) {
		t.Parallel()
		var intercepted []string
		interceptor := func(next connect.ServerFunc) connect.ServerFunc {
			return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
				intercepted = append(intercepted, spec.Procedure)
				return next(ctx, spec, stream)
			}
		}
		server := connect.NewServer(interceptor)
		pingv1connect.RegisterPingServiceHandler(server, pingServer{})
		server.SetUnknownHandler(func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			var req pingv1.PingRequest
			if err := stream.Receive(&req); err != nil {
				return err
			}
			return stream.Send(&pingv1.PingResponse{})
		})
		client := connect.NewClient(connectinprocess.New(server))
		if err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{}, &pingv1.PingResponse{}); err != nil {
			t.Fatalf("CallUnary: %v", err)
		}
		if len(intercepted) != 1 || intercepted[0] != unknownProcedure {
			t.Errorf("intercepted = %v, want [%q]", intercepted, unknownProcedure)
		}
	})
	t.Run("nil_restores_default", func(t *testing.T) {
		t.Parallel()
		server := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(server, pingServer{})
		server.SetUnknownHandler(func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return stream.Send(&pingv1.PingResponse{})
		})
		server.SetUnknownHandler(nil)
		client := connect.NewClient(connectinprocess.New(server))
		err := client.CallUnary(t.Context(), unknownSpec, &pingv1.PingRequest{}, &pingv1.PingResponse{})
		if got, want := connect.CodeOf(err), connect.CodeUnimplemented; got != want {
			t.Errorf("CodeOf = %v, want %v", got, want)
		}
	})
	t.Run("registered_method_unaffected", func(t *testing.T) {
		t.Parallel()
		server := connect.NewServer()
		pingv1connect.RegisterPingServiceHandler(server, pingServer{})
		server.SetUnknownHandler(func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return connect.NewError(connect.CodeInternal, "unknown handler must not serve registered methods")
		})
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(server)))
		res, err := client.Ping(t.Context(), &pingv1.PingRequest{Number: 42})
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if res.GetNumber() != 42 {
			t.Errorf("Number = %d, want 42", res.GetNumber())
		}
	})
}

// frontPingServer forwards Ping to a nested service on the handler context.
type frontPingServer struct {
	pingServer

	probe pingv1connect.PingServiceClient
}

func (s frontPingServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return s.probe.Ping(ctx, req)
}

// probePingServer echoes the received Authorization header in the response
// text.
type probePingServer struct {
	pingServer
}

func (probePingServer) Ping(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	auth := ""
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		auth = info.RequestHeader().Get("Authorization")
	}
	return &pingv1.PingResponse{Text: auth}, nil
}

type closeSignalPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	done chan struct{}
}

func (s *closeSignalPingServer) CountUp(_ context.Context, req *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	defer close(s.done)
	for i := int64(1); i <= req.GetNumber(); i++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

type pingServer struct {
	respHeaders  map[string]string
	respTrailers map[string]string
}

func (s pingServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		for k, v := range s.respHeaders {
			info.ResponseHeader().Set(k, v)
		}
		for k, v := range s.respTrailers {
			info.ResponseTrailer().Set(k, v)
		}
	}
	return &pingv1.PingResponse{Number: req.GetNumber(), Text: req.GetText()}, nil
}

func (pingServer) Fail(_ context.Context, _ *pingv1.FailRequest) (*pingv1.FailResponse, error) {
	return nil, connect.Errorf(connect.CodeUnimplemented, "Fail")
}

func (pingServer) Sum(_ context.Context, stream pingv1connect.PingServiceSumServerStream) (*pingv1.SumResponse, error) {
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		total += req.GetNumber()
	}
	return &pingv1.SumResponse{Sum: total}, nil
}

func (pingServer) CountUp(_ context.Context, req *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	for i := int64(1); i <= req.GetNumber(); i++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
	return nil
}

func (pingServer) CumSum(_ context.Context, stream pingv1connect.PingServiceCumSumServerStream) error {
	var sum int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		sum += req.GetNumber()
		if err := stream.Send(&pingv1.CumSumResponse{Sum: sum}); err != nil {
			return err
		}
	}
}

// panicPingServer panics in Ping to exercise unary panic recovery.
type panicPingServer struct{ pingServer }

func (panicPingServer) Ping(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	panic("boom") //nolint:forbidigo // exercises the transport's panic recovery
}

// remoteErrPingServer forwards an upstream error already marked remote.
type remoteErrPingServer struct{ pingServer }

func (remoteErrPingServer) Ping(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return nil, connect.NewError(connect.CodePermissionDenied, "upstream secret").WithRemote()
}

type infiniteCountUpServer struct {
	pingv1connect.UnimplementedPingServiceHandler

	done chan struct{}
}

func (s *infiniteCountUpServer) CountUp(_ context.Context, _ *pingv1.CountUpRequest, stream pingv1connect.PingServiceCountUpServerStream) error {
	defer close(s.done)
	for i := int64(1); ; i++ {
		if err := stream.Send(&pingv1.CountUpResponse{Number: i}); err != nil {
			return err
		}
	}
}

// TestCallInfoSpec verifies the transport sets Spec on both sides' CallInfo.
func TestCallInfoSpec(t *testing.T) {
	t.Parallel()
	var gotSpec connect.Spec
	interceptor := func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			gotSpec = info.Spec
			return next(ctx, spec, stream)
		}
	}
	server := connect.NewServer(interceptor)
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	client := pingv1connect.NewPingServiceClient(connect.NewClient(connectinprocess.New(server)))

	ctx, info := connect.NewClientContext(t.Context())
	if _, err := client.Ping(ctx, &pingv1.PingRequest{}); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got, want := info.Spec.Procedure, pingv1connect.PingServicePingProcedure; got != want {
		t.Errorf("client Spec.Procedure = %q, want %q", got, want)
	}
	if got, want := gotSpec.Procedure, pingv1connect.PingServicePingProcedure; got != want {
		t.Errorf("server Spec.Procedure = %q, want %q", got, want)
	}
}
