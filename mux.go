package rerpc

import (
	"net/http"
)

// NewServeMux mounts reRPC handlers on a mux. The signature is designed to
// work with with reRPC's generated code, which models each protobuf service as
// a slice of *Handlers.
func NewServeMux(services ...[]Handler) *http.ServeMux {
	mux := http.NewServeMux()
	for _, svc := range services {
		for _, method := range svc {
			method := method // don't want ref to loop variable
			mux.Handle(method.Path(), &method)
		}
	}
	return mux
}
