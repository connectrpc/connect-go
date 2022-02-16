package connect

import (
	"net/http"
)

type muxConfiguration struct {
	fallback http.Handler
	handlers []Handler
	err      error
}

type MuxOption interface {
	apply(*muxConfiguration)
}

type withHandlersOption struct {
	handlers []Handler
	err      error
}

var _ MuxOption = (*withHandlersOption)(nil)

// WithHandlers reduces error-handling boilerplate by bundling a collection of
// handlers and an error into a MuxOption. Any errors encountered during
// handler construction are returned from NewServeMux.
//
// This option is used in generated code. Most users won't need to interact
// with it directly.
func WithHandlers(handlers []Handler, err error) MuxOption {
	return &withHandlersOption{
		handlers: handlers,
		err:      err,
	}
}

func (o *withHandlersOption) apply(config *muxConfiguration) {
	if config.err != nil {
		return
	}
	config.err = o.err
	config.handlers = append(config.handlers, o.handlers...)
}

type withFallbackOption struct {
	fallback http.Handler
}

var _ MuxOption = (*withFallbackOption)(nil)

// WithFallback sets the fallback HTTP handler for a mux. Requests that don't
// match the paths for any of the mux's registered services are handled by the
// fallback handler.
//
// The default fallback handler responds with a 404. A more complex fallback
// could proxy to a server that can handle the requested RPC.
func WithFallback(fallback http.Handler) MuxOption {
	return &withFallbackOption{fallback: fallback}
}

func (o *withFallbackOption) apply(config *muxConfiguration) {
	config.fallback = o.fallback
}

// NewServeMux mounts connect handlers on a mux.
func NewServeMux(options ...MuxOption) (*http.ServeMux, error) {
	config := &muxConfiguration{fallback: newNotFoundHandler()}
	for _, opt := range options {
		opt.apply(config)
	}
	if config.err != nil {
		return nil, config.err
	}
	mux := http.NewServeMux()
	mux.Handle("/", config.fallback)
	for _, handler := range config.handlers {
		handler := handler // don't want ref to loop variable
		mux.Handle(handler.path(), &handler)
	}
	return mux, nil
}

// newNotFoundHandler returns an HTTP handler that always responds with a 404.
// It's useful as a simple fallback handler in calls to NewServeMux.
func newNotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discard(r.Body)
		r.Body.Close()
		w.WriteHeader(http.StatusNotFound)
	})
}
