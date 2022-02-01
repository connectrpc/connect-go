package connect

import "net/http"

// NewNotFoundHandler returns an HTTP handler that always responds with a 404.
// It's useful as a simple fallback handler in calls to NewServeMux.
func NewNotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discard(r.Body)
		r.Body.Close()
		w.WriteHeader(http.StatusNotFound)
	})
}
