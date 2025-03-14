// Copyright 2021-2025 The Connect Authors
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

package memhttp

import (
	"log"
	"time"
)

// config is the configuration for a Server.
type config struct {
	CleanupTimeout time.Duration
	ErrorLog       *log.Logger
}

// An Option configures a Server.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (f optionFunc) apply(cfg *config) { f(cfg) }

// WithOptions composes multiple Options into one.
func WithOptions(opts ...Option) Option {
	return optionFunc(func(cfg *config) {
		for _, opt := range opts {
			opt.apply(cfg)
		}
	})
}

// WithErrorLog sets [http.Server.ErrorLog].
func WithErrorLog(l *log.Logger) Option {
	return optionFunc(func(cfg *config) {
		cfg.ErrorLog = l
	})
}

// WithCleanupTimeout customizes the default five-second timeout for the
// server's Cleanup method.
func WithCleanupTimeout(d time.Duration) Option {
	return optionFunc(func(cfg *config) {
		cfg.CleanupTimeout = d
	})
}
