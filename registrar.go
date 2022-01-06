package rerpc

import (
	"sort"
	"sync"
)

// A Registrar collects information to support gRPC server reflection
// when building handlers. Registrars are valid HandlerOptions.
type Registrar struct {
	mu       sync.RWMutex
	services map[string]struct{}
}

var _ HandlerOption = (*Registrar)(nil)

// NewRegistrar constructs an empty Registrar.
func NewRegistrar() *Registrar {
	return &Registrar{services: make(map[string]struct{})}
}

// Services returns the fully-qualified names of the registered protobuf
// services. The returned slice is a copy, so it's safe for callers to modify.
// This method is safe to call concurrently.
func (r *Registrar) Services() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.services))
	for n := range r.services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// IsRegistered checks whether a fully-qualified protobuf service name is
// registered. It's safe to call concurrently.
func (r *Registrar) IsRegistered(service string) bool {
	r.mu.RLock()
	_, ok := r.services[service]
	r.mu.RUnlock()
	return ok
}

// Registers a protobuf package and service combination. Safe to call
// concurrently.
func (r *Registrar) register(pkg, service string) {
	if pkg == "" || service == "" {
		return
	}
	fqn := pkg + "." + service
	r.mu.Lock()
	r.services[fqn] = struct{}{}
	r.mu.Unlock()
}

func (r *Registrar) applyToHandler(cfg *handlerCfg) {
	cfg.Registrar = r
}
