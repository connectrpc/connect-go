package connect_test

import (
	"net/http"

	"github.com/bufbuild/connect/connecttest"
	pingrpc "github.com/bufbuild/connect/internal/gen/proto/go-connect/connect/ping/v1test"
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
	mux := http.NewServeMux()
	mux.Handle(pingrpc.NewPingServiceHandler(pingServer{}))
	examplePingServer = connecttest.NewServer(mux)
}
