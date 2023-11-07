// Copyright 2021-2023 The Connect Authors
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

package connect_test

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

	connect "connectrpc.com/connect"
	"connectrpc.com/connect/internal/assert"
	pingv1 "connectrpc.com/connect/internal/gen/connect/ping/v1"
	"connectrpc.com/connect/internal/gen/connect/ping/v1/pingv1connect"
	"connectrpc.com/connect/internal/memhttp/memhttptest"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestHandler_ServeHTTP(t *testing.T) {
	t.Parallel()
	path, handler := pingv1connect.NewPingServiceHandler(successPingServer{})
	prefixed := http.NewServeMux()
	prefixed.Handle(path, handler)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.Handle("/prefixed/", http.StripPrefix("/prefixed", prefixed))
	const pingProcedure = pingv1connect.PingServicePingProcedure
	const sumProcedure = pingv1connect.PingServiceSumProcedure
	server := memhttptest.NewServer(t, mux)
	client := server.Client()

	t.Run("get_method_no_encoding", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			context.Background(),
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
			context.Background(),
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

	t.Run("idempotent_get_method", func(t *testing.T) {
		t.Parallel()
		request, err := http.NewRequestWithContext(
			context.Background(),
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
			context.Background(),
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
			context.Background(),
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
			context.Background(),
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
			"application/grpc+json; charset=utf-8",
			"application/grpc+proto",
			"application/grpc-web",
			"application/grpc-web+json",
			"application/grpc-web+json; charset=utf-8",
			"application/grpc-web+proto",
			"application/json",
			"application/json; charset=utf-8",
			"application/proto",
		}, ", "))
	})

	t.Run("charset_in_content_type_header", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequestWithContext(
			context.Background(),
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
			context.Background(),
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
			context.Background(),
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
		assert.Equal(t, resp.StatusCode, http.StatusNotFound)

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
	mux.Handle(pingv1connect.NewPingServiceHandler(successPingServer{}))
	server := memhttptest.NewServer(t, mux)

	const (
		concurrency  = 256
		spuriousSize = 1024 * 1024 * 512 // 512 MB
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		body := make([]byte, 16)
		// Envelope prefix indicates a large payload which we're not actually
		// sending.
		binary.BigEndian.PutUint32(body[1:5], spuriousSize)
		req, err := http.NewRequestWithContext(
			context.Background(),
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
		dynamicPing := func(_ context.Context, req *connect.Request[dynamicpb.Message]) (*connect.Response[dynamicpb.Message], error) {
			got := req.Msg.Get(methodDesc.Input().Fields().ByName("number")).Int()
			msg := dynamicpb.NewMessage(methodDesc.Output())
			msg.Set(
				methodDesc.Output().Fields().ByName("number"),
				protoreflect.ValueOfInt64(got),
			)
			return connect.NewResponse(msg), nil
		}
		mux := http.NewServeMux()
		mux.Handle("/connect.ping.v1.PingService/Ping",
			connect.NewUnaryHandler(
				"/connect.ping.v1.PingService/Ping",
				dynamicPing,
				connect.WithSchema(methodDesc),
				connect.WithIdempotency(connect.IdempotencyNoSideEffects),
			),
		)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		rsp, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{
			Number: 42,
		}))
		if !assert.Nil(t, err) {
			return
		}
		got := rsp.Msg.Number
		assert.Equal(t, got, 42)
	})
	t.Run("clientStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.Sum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		dynamicSum := func(_ context.Context, stream *connect.ClientStream[dynamicpb.Message]) (*connect.Response[dynamicpb.Message], error) {
			var sum int64
			for stream.Receive() {
				got := stream.Msg().Get(
					methodDesc.Input().Fields().ByName("number"),
				).Int()
				sum += got
			}
			msg := dynamicpb.NewMessage(methodDesc.Output())
			msg.Set(
				methodDesc.Output().Fields().ByName("sum"),
				protoreflect.ValueOfInt64(sum),
			)
			return connect.NewResponse(msg), nil
		}
		mux := http.NewServeMux()
		mux.Handle("/connect.ping.v1.PingService/Sum",
			connect.NewClientStreamHandler(
				"/connect.ping.v1.PingService/Sum",
				dynamicSum,
				connect.WithSchema(methodDesc),
			),
		)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		stream := client.Sum(context.Background())
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 42}))
		assert.Nil(t, stream.Send(&pingv1.SumRequest{Number: 42}))
		rsp, err := stream.CloseAndReceive()
		if !assert.Nil(t, err) {
			return
		}
		assert.Equal(t, rsp.Msg.Sum, 42*2)
	})
	t.Run("serverStream", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CountUp")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		dynamicCountUp := func(_ context.Context, req *connect.Request[dynamicpb.Message], stream *connect.ServerStream[dynamicpb.Message]) error {
			number := req.Msg.Get(methodDesc.Input().Fields().ByName("number")).Int()
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
		}
		mux := http.NewServeMux()
		mux.Handle("/connect.ping.v1.PingService/CountUp",
			connect.NewServerStreamHandler(
				"/connect.ping.v1.PingService/CountUp",
				dynamicCountUp,
				connect.WithSchema(methodDesc),
			),
		)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		stream, err := client.CountUp(context.Background(), connect.NewRequest(&pingv1.CountUpRequest{
			Number: 2,
		}))
		if !assert.Nil(t, err) {
			return
		}
		var sum int64
		for stream.Receive() {
			sum += stream.Msg().Number
		}
		assert.Nil(t, stream.Err())
		assert.Equal(t, sum, 3) // 1 + 2
	})
	t.Run("bidi", func(t *testing.T) {
		t.Parallel()
		desc, err := protoregistry.GlobalFiles.FindDescriptorByName("connect.ping.v1.PingService.CumSum")
		assert.Nil(t, err)
		methodDesc, ok := desc.(protoreflect.MethodDescriptor)
		assert.True(t, ok)
		dynamicCumSum := func(
			_ context.Context,
			stream *connect.BidiStream[dynamicpb.Message, dynamicpb.Message],
		) error {
			var sum int64
			for {
				msg, err := stream.Receive()
				if errors.Is(err, io.EOF) {
					return nil
				} else if err != nil {
					return err
				}
				got := msg.Get(methodDesc.Input().Fields().ByName("number")).Int()
				sum += got
				out := dynamicpb.NewMessage(methodDesc.Output())
				out.Set(
					methodDesc.Output().Fields().ByName("sum"),
					protoreflect.ValueOfInt64(sum),
				)
				if err := stream.Send(out); err != nil {
					return err
				}
			}
		}
		mux := http.NewServeMux()
		mux.Handle("/connect.ping.v1.PingService/CumSum",
			connect.NewBidiStreamHandler(
				"/connect.ping.v1.PingService/CumSum",
				dynamicCumSum,
				connect.WithSchema(methodDesc),
			),
		)
		server := memhttptest.NewServer(t, mux)
		client := pingv1connect.NewPingServiceClient(server.Client(), server.URL())
		stream := client.CumSum(context.Background())
		assert.Nil(t, stream.Send(&pingv1.CumSumRequest{Number: 1}))
		msg, err := stream.Receive()
		if !assert.Nil(t, err) {
			return
		}
		assert.Equal(t, msg.Sum, int64(1))
		assert.Nil(t, stream.CloseRequest())
		assert.Nil(t, stream.CloseResponse())
	})
}

type successPingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (successPingServer) Ping(context.Context, *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	return &connect.Response[pingv1.PingResponse]{}, nil
}
