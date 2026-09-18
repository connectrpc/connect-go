// Copyright 2021-2026 The Connect Authors
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

package connecthttp

import (
	envpkg "connectrpc.com/connect/v2/internal/envelope"
)

// Enveloped-Message framing lives in internal/envelope so that non-HTTP
// transports can share it. These aliases keep the in-package spellings,
// including the names of embedded fields.
type (
	envelope       = envpkg.Envelope
	envelopeReader = envpkg.Reader
	envelopeWriter = envpkg.Writer
	messagePayload = envpkg.MessagePayload
	messageSender  = envpkg.MessageSender
)

var errSpecialEnvelope = envpkg.ErrSpecialEnvelope
