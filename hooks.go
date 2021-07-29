package rerpc

import "context"

// Hooks are observability tie-ins for the handful of error paths that aren't
// visible to interceptors. It's safe to pass nil function pointers if you
// don't need observability for a particular class of errors. Any supplied
// functions must be safe to call concurrently.
//
// Hooks are valid Options.
type Hooks struct {
	// OnInternalError logs severe errors internal to reRPC. These errors usually
	// indicate that there's a bug in reRPC - please file GitHub issues and let
	// us know about them!
	OnInternalError func(context.Context, error)
	// OnNetworkError logs low-level network errors that occur after the
	// interceptor chain has returned. Typically, these occur when a Handler
	// encounters errors trying to write to the response body.
	OnNetworkError func(context.Context, error)
	// OnMarshalError logs unexpected errors marshaling in-memory data
	// structures. Since we're always marshaling protobuf-generated structs to
	// supported formats (either binary protobuf or protobuf-flavored JSON),
	// this class of errors should only crop up if you're using non-standard
	// protobuf code generation.
	OnMarshalError func(context.Context, error)
}

func (h *Hooks) applyToCall(cfg *callCfg) {
	cfg.Hooks = h
}

func (h *Hooks) applyToHandler(cfg *handlerCfg) {
	cfg.Hooks = h
}

func (h *Hooks) onInternalError(ctx context.Context, err error) {
	if h == nil {
		return
	}
	if h.OnInternalError == nil {
		return
	}
	h.OnInternalError(ctx, err)
}

func (h *Hooks) onNetworkError(ctx context.Context, err error) {
	if h == nil {
		return
	}
	if h.OnNetworkError == nil {
		return
	}
	h.OnNetworkError(ctx, err)
}

func (h *Hooks) onMarshalError(ctx context.Context, err error) {
	if h == nil {
		return
	}
	if h.OnMarshalError == nil {
		return
	}
	h.OnMarshalError(ctx, err)
}
