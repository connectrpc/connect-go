package rerpc

import (
	"context"
)

// NewBadRouteHandler always returns gRPC and Twirp's equivalent of the
// standard library's http.StatusNotFound. To be fully compatible with the
// Twirp specification, include this in your call to NewServeMux (so that it
// handles any requests for invalid protobuf methods).
func NewBadRouteHandler(opts ...HandlerOption) []*Handler {
	var cfg handlerCfg
	for _, opt := range opts {
		opt.applyToHandler(&cfg)
	}
	// Expressing this logic with a Func ensures that we can still apply
	// middleware (e.g., for observability).
	wrapped := func(ctx context.Context, _ interface{}) (interface{}, error) {
		// No point checking the context for cancellation or deadline - retries won't
		// help.
		path := "???"
		if md, ok := HandlerMetadata(ctx); ok {
			path = md.Spec.Path
		}
		return nil, Wrap(CodeNotFound, newBadRouteError(path))
	}
	if ic := cfg.Interceptor; ic != nil {
		wrapped = ic.Wrap(wrapped)
	}

	h := NewStreamingHandler(
		StreamTypeUnary,
		"", "", "", // protobuf package, service, method names
		func(ctx context.Context, sf StreamFunc) {
			stream := sf(ctx)
			_ = stream.CloseReceive()
			_, err := wrapped(ctx, nil)
			_ = stream.CloseSend(err)
		},
		opts...,
	)
	return []*Handler{h}
}
