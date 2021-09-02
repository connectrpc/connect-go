package rerpc

import (
	"context"
)

// NewBadRouteHandler always returns gRPC and Twirp's equivalent of the
// standard library's http.StatusNotFound. To be fully compatible with the
// Twirp specification, include this in your call to NewServeMux (so that it
// handles any requests for invalid protobuf methods).
func NewBadRouteHandler(opts ...HandlerOption) []*Handler {
	wrapped := Func(badRouteUnaryImpl)
	if ic := ConfiguredHandlerInterceptor(opts); ic != nil {
		wrapped = ic.Wrap(wrapped)
	}
	h := NewHandler(
		StreamTypeUnary,
		"", "", "", // protobuf package, service, method names
		func(ctx context.Context, sf StreamFunc) {
			stream := sf(ctx)
			_ = stream.CloseReceive()
			_, err := wrapped(ctx, nil)
			_ = stream.CloseSend(err)
		},
	)
	return []*Handler{h}
}

func badRouteUnaryImpl(ctx context.Context, _ interface{}) (interface{}, error) {
	// There's no point checking the context and sending CodeCanceled or
	// CodeDeadlineExceeded here - it's just as fast to send the bad route error.
	path := "???"
	if md, ok := HandlerMetadata(ctx); ok {
		path = md.Spec.Path
	}
	return nil, Wrap(CodeNotFound, newBadRouteError(path))
}
