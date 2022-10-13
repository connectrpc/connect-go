package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type PingServer struct {
	pingv1connect.UnimplementedPingServiceHandler // returns errors from all methods
}

func (ps *PingServer) Ping(
	_ context.Context,
	_ *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("foobar"))
}

func main() {
	mux := http.NewServeMux()
	mux.Handle(pingv1connect.NewPingServiceHandler(&PingServer{}))
	err := http.ListenAndServe(
		"localhost:8080",
		h2c.NewHandler(mux, &http2.Server{}),
	)
	log.Fatalf("listen failed: %v", err)
}
