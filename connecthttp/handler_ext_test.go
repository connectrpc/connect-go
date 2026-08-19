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

package connecthttp_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/assert"
	pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/v2/internal/memhttp/memhttptest"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestHandler_ServeHTTP(t *testing.T) {
	t.Parallel()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, successPingServer{})
	prefixed := http.NewServeMux()
	connecthttp.Mount(prefixed, srv)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	mux.Handle("/prefixed/", http.StripPrefix("/prefixed", prefixed))
	const pingProcedure = pingv1connect.PingServicePingProcedure
	const sumProcedure = pingv1connect.PingServiceSumProcedure
	server := memhttptest.NewServer(t, mux)
	client := server.Client()

	t.Run("get_method_no_encoding", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+pingProcedure,
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
	})

	t.Run("get_method_bad_encoding", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+pingProcedure+`?encoding=unk&message={}`,
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
	})

	t.Run("get_method_body_not_allowed", func(t *testing.T) {
		t.Parallel()
		const queryStringSuffix = `?encoding=json&message={}`
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+pingProcedure+queryStringSuffix,
			strings.NewReader("!"), // non-empty body
		)
		assert.Nil(t, err)
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)

		// Same thing, but this time w/ a content-length header
		request, err = http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+pingProcedure+queryStringSuffix,
			strings.NewReader("!"), // non-empty body
		)
		assert.Nil(t, err)
		request.Header.Set("content-length", "1")
		resp, err = client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
	})

	t.Run("idempotent_get_method", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+pingProcedure+`?encoding=json&message={}`,
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusOK)
	})

	t.Run("prefixed_get_method", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+"/prefixed"+pingProcedure+`?encoding=json&message={}`,
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusOK)
	})

	t.Run("method_not_allowed", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			server.URL()+sumProcedure,
			strings.NewReader(""),
		)
		assert.Nil(t, err)
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed)
		assert.Equal(t, resp.Header.Get("Allow"), http.MethodPost)
	})

	t.Run("unsupported_content_type", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+pingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		request.Header.Set("Content-Type", "application/x-custom-json")
		resp, err := client.Do(request)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
		assert.Equal(t, resp.Header.Get("Accept-Post"), strings.Join([]string{
			"application/grpc",
			"application/grpc+json",
			"application/grpc+proto",
			"application/grpc-web",
			"application/grpc-web+json",
			"application/grpc-web+proto",
			"application/json",
			"application/proto",
		}, ", "))
	})

	t.Run("charset_in_content_type_header", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+pingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json;Charset=Utf-8")
		resp, err := client.Do(req)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusOK)
	})

	t.Run("unsupported_charset", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+pingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json; charset=shift-jis")
		resp, err := client.Do(req)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusUnsupportedMediaType)
	})

	t.Run("unsupported_content_encoding", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+pingProcedure,
			strings.NewReader("{}"),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "invalid")
		resp, err := client.Do(req)
		assert.Nil(t, err)
		defer resp.Body.Close()
		assert.Equal(t, resp.StatusCode, http.StatusNotImplemented)

		type errorMessage struct {
			Code    string `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
		}
		var message errorMessage
		err = json.NewDecoder(resp.Body).Decode(&message)
		assert.Nil(t, err)
		assert.Equal(t, message.Message, `unknown compression "invalid": supported encodings are gzip`)
		assert.Equal(t, message.Code, connect.CodeUnimplemented.String())
	})
}

func TestHandlerMaliciousPrefix(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srv := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(srv, successPingServer{})
	connecthttp.Mount(mux, srv)
	server := memhttptest.NewServer(t, mux)

	const (
		concurrency  = 256
		spuriousSize = 1024 * 1024 * 512 // 512 MB
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range concurrency {
		body := make([]byte, 16)
		// Envelope prefix indicates a large payload which we're not actually
		// sending.
		binary.BigEndian.PutUint32(body[1:5], spuriousSize)
		req, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			server.URL()+pingv1connect.PingServicePingProcedure,
			bytes.NewReader(body),
		)
		assert.Nil(t, err)
		req.Header.Set("Content-Type", "application/grpc")
		wg.Add(1)
		go func(req *http.Request) {
			defer wg.Done()
			<-start
			response, err := server.Client().Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
		}(req)
	}
	close(start)
	wg.Wait()
}

func TestDynamicHandler(t *testing.T) {
	t.Parallel()
	t.Run("unary", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Ping")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		mux := http.NewServeMux()
		srv := connect.NewServer()
		srv.Register(connect.Method{
			Spec: connect.Spec{
				Procedure:        "/connect.ping.v1.PingService/Ping",
				Schema:           methodDesc,
				StreamType:       connect.StreamTypeUnary,
				IdempotencyLevel: connect.IdempotencyNoSideEffects,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				req := dynamicpb.NewMessage(methodDesc.Input())
				if err := stream.Receive(req); err != nil {
					return err
				}
				got := req.Get(methodDesc.Input().Fields().ByName("number")).Int()
				msg := dynamicpb.NewMessage(methodDesc.Output())
				msg.Set(
					methodDesc.Output().Fields().ByName("number"),
					protoreflect.ValueOfInt64(got),
				)
				return stream.Send(msg)
			},
		})
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		rsp, err := client.Ping(t.Context(), &pingv1.PingRequest{
			Number: 42,
		})
		if !assert.Nil(t, err) {
			return
		}
		got := rsp.Number
		assert.Equal(t, got, 42)
	})
	t.Run("clientStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Sum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		mux := http.NewServeMux()
		srv := connect.NewServer()
		srv.Register(connect.Method{
			Spec: connect.Spec{
				Procedure:  "/connect.ping.v1.PingService/Sum",
				Schema:     methodDesc,
				StreamType: connect.StreamTypeClient,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				var sum int64
				for {
					msg := dynamicpb.NewMessage(methodDesc.Input())
					if err := stream.Receive(msg); err != nil {
						if errors.Is(err, io.EOF) {
							break
						}
						return err
					}
					sum += msg.Get(methodDesc.Input().Fields().ByName("number")).Int()
				}
				out := dynamicpb.NewMessage(methodDesc.Output())
				out.Set(
					methodDesc.Output().Fields().ByName("sum"),
					protoreflect.ValueOfInt64(sum),
				)
				return stream.Send(out)
			},
		})
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		stream, err := client.Sum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 42}))
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 42}))
		rsp, err := stream.CloseAndReceive()
		if !assert.Nil(t, err) {
			return
		}
		assert.Equal(t, rsp.Sum, 42*2)
	})
	t.Run("serverStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CountUp")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		mux := http.NewServeMux()
		srv := connect.NewServer()
		srv.Register(connect.Method{
			Spec: connect.Spec{
				Procedure:  "/connect.ping.v1.PingService/CountUp",
				Schema:     methodDesc,
				StreamType: connect.StreamTypeServer,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				req := dynamicpb.NewMessage(methodDesc.Input())
				if err := stream.Receive(req); err != nil {
					return err
				}
				number := req.Get(methodDesc.Input().Fields().ByName("number")).Int()
				for i := int64(1); i <= number; i++ {
					msg := dynamicpb.NewMessage(methodDesc.Output())
					msg.Set(
						methodDesc.Output().Fields().ByName("number"),
						protoreflect.ValueOfInt64(i),
					)
					if err := stream.Send(msg); err != nil {
						return err
					}
				}
				return nil
			},
		})
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		stream, err := client.CountUp(t.Context(), &pingv1.CountUpRequest{
			Number: 2,
		})
		if !assert.Nil(t, err) {
			return
		}
		var sum int64
		for {
			msg, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatal(err)
			}
			sum += msg.Number
		}
		assert.Nil(t, stream.Close())
		assert.Equal(t, sum, 3) // 1 + 2
	})
	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CumSum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		mux := http.NewServeMux()
		srv := connect.NewServer()
		srv.Register(connect.Method{
			Spec: connect.Spec{
				Procedure:  "/connect.ping.v1.PingService/CumSum",
				Schema:     methodDesc,
				StreamType: connect.StreamTypeBidi,
			},
			Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
				var sum int64
				for {
					msg := dynamicpb.NewMessage(methodDesc.Input())
					if err := stream.Receive(msg); err != nil {
						if errors.Is(err, io.EOF) {
							return nil
						}
						return err
					}
					sum += msg.Get(methodDesc.Input().Fields().ByName("number")).Int()
					out := dynamicpb.NewMessage(methodDesc.Output())
					out.Set(
						methodDesc.Output().Fields().ByName("sum"),
						protoreflect.ValueOfInt64(sum),
					)
					if err := stream.Send(out); err != nil {
						return err
					}
				}
			},
		})
		connecthttp.Mount(mux, srv)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(connect.NewClient(connecthttp.NewTransport(server.Client(), server.URL())))
		stream, err := client.CumSum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
		msg, err := stream.Receive()
		if !assert.Nil(t, err) {
			return
		}
		assert.Equal(t, msg.Sum, int64(1))
		assert.Nil(t, stream.CloseSend())
		assert.Nil(t, stream.Close())
	})
}

type successPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (successPingServer) Ping(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{}, nil
}
