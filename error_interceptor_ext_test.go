package connect_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/bufconnect/connect"
	"github.com/bufconnect/connect/connecttest"
	"github.com/bufconnect/connect/internal/assert"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
	pingpb "github.com/bufconnect/connect/internal/gen/proto/go/connect/ping/v1test"
)

type customErrorPingService struct {
	pingrpc.UnimplementedPingServiceHandler
}

func (s *customErrorPingService) Fail(
	_ context.Context,
	_ *connect.Request[pingpb.FailRequest],
) (*connect.Response[pingpb.FailResponse], error) {
	return nil, newLocationError("some_file.go", 42)
}

// Silly example of a custom error type whose extra attributes are easily
// modeled with a protobuf message we've already defined.
type locationError struct {
	ping pingpb.PingRequest
}

func newLocationError(file string, line int64) *locationError {
	return &locationError{
		ping: pingpb.PingRequest{
			Msg:    file,
			Number: line,
		},
	}
}

func (e *locationError) Error() string {
	return "oh no:" + e.Location()
}

func (e *locationError) Location() string {
	return fmt.Sprintf("%s:%d", e.ping.Msg, e.ping.Number)
}

func TestErrorTranslatingInterceptor(t *testing.T) {
	toWire := func(err error) error {
		if connectErr, ok := connect.AsError(err); ok {
			return connectErr
		}
		var loc *locationError
		if ok := errors.As(err, &loc); !ok {
			return err
		}
		connectErr := connect.Wrap(connect.CodeAborted, err)
		detail, err := anypb.New(&loc.ping)
		assert.Nil(t, err, "create proto.Any")
		connectErr.AddDetail(detail)
		return connectErr
	}
	fromWire := func(err error) error {
		connectErr, ok := connect.AsError(err)
		if !ok || connectErr.Code() != connect.CodeAborted {
			return err
		}
		ping := &pingpb.PingRequest{}
		for _, d := range connectErr.Details() {
			if d.UnmarshalTo(ping) == nil {
				return newLocationError(ping.Msg, ping.Number)
			}
		}
		return err
	}
	mux, err := connect.NewServeMux(
		pingrpc.WithPingServiceHandler(
			&customErrorPingService{},
			connect.Interceptors(connect.NewErrorInterceptor(toWire, nil /* fromWire */)),
		),
	)
	assert.Nil(t, err, "serve mux error")
	server := connecttest.NewServer(mux)
	client, err := pingrpc.NewPingServiceClient(
		server.URL(),
		server.Client(),
		connect.Interceptors(connect.NewErrorInterceptor(nil /* toWire */, fromWire)),
	)
	assert.Nil(t, err, "client construction error")
	_, err = client.Fail(context.Background(), connect.NewRequest(&pingpb.FailRequest{}))
	assert.NotNil(t, err, "client-visible error")
	lerr, ok := err.(*locationError)
	assert.True(t, ok, "convert to custom error type")
	assert.NotZero(t, lerr.ping.Number, "error details sent over network")
}
