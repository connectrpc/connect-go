package rerpc

import (
	"net/http"
)

// A Service bundles the output of reRPC's generated handler constructors,
// which return a slice of Handlers and an error.
type Service struct {
	handlers []Handler
	err      error
}

// NewService constructs a Service. It's used in generated code.
func NewService(handlers []Handler, err error) *Service {
	return &Service{handlers: handlers, err: err}
}

// NewServeMux mounts reRPC handlers on a mux. The signature is designed to
// work with with reRPC's generated code, where server-side constructors return
// *Service.
//
// Requests that don't match the paths for any of the supplied services are
// handled by the fallback handler. For a simple fallback, see
// NewNotFoundHandler. A more complex fallback could proxy to a server that can
// serve the requested RPC.
func NewServeMux(fallback http.Handler, services ...*Service) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.Handle("/", fallback)
	for _, svc := range services {
		if svc.err != nil {
			return nil, svc.err
		}
		for _, method := range svc.handlers {
			method := method // don't want ref to loop variable
			mux.Handle(method.path(), &method)
		}
	}
	return mux, nil
}
