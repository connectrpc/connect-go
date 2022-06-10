package connect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bufbuild/connect-go/internal/assert"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
)

var errFailedToCreateStream = errors.New("failed to create stream")

func TestInterceptorsCalledIfSenderReceiverNil(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	pingPath := "/connect.ping.v1.PingService/Ping"
	handler := NewUnaryHandler[pingv1.PingRequest, pingv1.PingResponse](
		pingPath,
		func(ctx context.Context, request *Request[pingv1.PingRequest]) (*Response[pingv1.PingResponse], error) {
			t.Error("shouldn't call handler implementation")
			return nil, NewError(CodeUnimplemented, nil)
		},
		WithInterceptors(UnaryInterceptorFunc(func(next UnaryFunc) UnaryFunc {
			return func(ctx context.Context, request AnyRequest) (AnyResponse, error) {
				_, err := next(ctx, request)
				assert.NotNil(t, err)
				assert.True(t, errors.Is(err, errFailedToCreateStream))
				return nil, err
			}
		})),
	)
	// Override gRPC handler with custom one
	handler.protocolHandlers = append([]protocolHandler{&nilProtocolHandler{}}, handler.protocolHandlers...)
	mux.Handle(pingPath, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClient[pingv1.PingRequest, pingv1.PingResponse](server.Client(), server.URL+"/connect.ping.v1.PingService/Ping")
	response, err := client.CallUnary(context.Background(), NewRequest(&pingv1.PingRequest{Text: "hello"}))
	assert.Nil(t, response)
	assert.NotNil(t, err)
}

type nilProtocolHandler struct{}

var _ protocolHandler = (*nilProtocolHandler)(nil)

func (n nilProtocolHandler) ContentTypes() map[string]struct{} {
	return map[string]struct{}{
		"application/proto": {},
	}
}

func (n nilProtocolHandler) SetTimeout(request *http.Request) (context.Context, context.CancelFunc, error) {
	return request.Context(), nil, nil
}

func (n nilProtocolHandler) NewStream(http.ResponseWriter, *http.Request) (Sender, Receiver, error) {
	return nil, nil, errFailedToCreateStream
}
