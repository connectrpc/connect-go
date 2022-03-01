// Copyright 2021-2022 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect

import (
	"sort"
	"sync"
)

// A Registrar collects information to support gRPC server reflection
// when building handlers.
type Registrar struct {
	mu       sync.RWMutex
	services map[string]struct{}
}

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
	for name := range r.services {
		names = append(names, name)
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
func (r *Registrar) register(name string) {
	r.mu.Lock()
	r.services[name] = struct{}{}
	r.mu.Unlock()
}
