package rerpc

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"
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
	wrapped := func(ctx context.Context, req AnyRequest) (AnyResponse, error) {
		// No point checking the context for cancellation or deadline - retries won't
		// help.
		return nil, Wrap(CodeNotFound, newBadRouteError(req.Spec().Procedure))
	}
	if ic := cfg.Interceptor; ic != nil {
		wrapped = ic.Wrap(wrapped)
	}

	h := NewStreamingHandler(
		StreamTypeUnary,
		"", "", "", // protobuf package, service, method names
		func(ctx context.Context, stream Stream) {
			_ = stream.CloseReceive()
			// TODO: This doesn't work correctly, but we're deleting Twirp support
			// soon anyways.
			_, err := wrapped(ctx, NewRequest(&emptypb.Empty{}))
			_ = stream.CloseSend(err)
		},
		opts...,
	)
	return []*Handler{h}
}
