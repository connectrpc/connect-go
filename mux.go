package rerpc

import (
	"net/http"
)

// NewServeMux mounts reRPC handlers on a mux. The signature is designed to
// work with with reRPC's generated code, which models each protobuf service as
// a slice of *Handlers.
//
// Requests that don't match the paths for any of the supplied services are
// handled by the fallback handler. For a simple fallback, see
// NewNotFoundHandler. A more complex fallback could proxy to a server that can
// serve the requested RPC.
func NewServeMux(fallback http.Handler, services ...[]Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", fallback)
	for _, svc := range services {
		for _, method := range svc {
			method := method // don't want ref to loop variable
			mux.Handle(method.Path(), &method)
		}
	}
	return mux
}
