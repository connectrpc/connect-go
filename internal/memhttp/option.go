// Copyright 2021-2023 The Connect Authors
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

type config struct {
	DisableTLS     bool
	DisableHTTP2   bool
	CleanupTimeout time.Duration
	ErrorLog       *log.Logger
}

// An Option configures a Server.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (f optionFunc) apply(cfg *config) { f(cfg) }

// WithoutHTTP2 disables HTTP/2 on the server and client.
func WithoutHTTP2() Option {
	return optionFunc(func(cfg *config) {
		cfg.DisableHTTP2 = true
	})
}

// WithOptions composes multiple Options into one.
func WithOptions(opts ...Option) Option {
	return optionFunc(func(cfg *config) {
		for _, opt := range opts {
			opt.apply(cfg)
		}
	})
}

// WithCleanupTimeout customizes the default five-second timeout for the
// server's Cleanup method. It's most useful with the memhttptest subpackage.
func WithCleanupTimeout(d time.Duration) Option {
	return optionFunc(func(cfg *config) {
		cfg.CleanupTimeout = d
	})
}

// WithErrorLog sets [http.Server.ErrorLog].
func WithErrorLog(l *log.Logger) Option {
	return optionFunc(func(cfg *config) {
		cfg.ErrorLog = l
	})
}
