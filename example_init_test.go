package connect_test

import (
	"github.com/bufconnect/connect"
	"github.com/bufconnect/connect/connecttest"
	pingrpc "github.com/bufconnect/connect/internal/gen/proto/go-connect/connect/ping/v1test"
)

var examplePingServer *connecttest.Server

func init() {
	// Generally, init functions are bad.
	//
	// To write testable examples that users can grok *and* can execute in the
	// playground, where networking is disabled, we need an HTTP server that uses
	// in-memory pipes instead of TCP. We don't want to pollute every example
	// with this setup code.
	//
	// The least-awful option is to set up the server in init().
	mux, err := connect.NewServeMux(
		pingrpc.WithPingServiceHandler(pingServer{}),
	)
	if err != nil {
		return
	}
	examplePingServer = connecttest.NewServer(mux)
}
