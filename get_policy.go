// Copyright 2021-2023 Buf Technologies, Inc.
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

// GetPolicy controls the behavior around requests that can be carried out via
// HTTP Get requests. By default, requests will not be carried out via HTTP Get
// even if they can be. Using WithHTTPGet, you can set the Get request policy
// for a client.
type GetPolicy int

const (
	// GetPolicyNever will cause a client to never use Get requests.
	GetPolicyNever GetPolicy = 0

	// GetPolicyOpportunistic will cause a client to use Get requests for
	// eligible (side-effect free) procedures, as long as they would not exceed
	// the configured max URL size. If this happens, the request will fall back
	// to HTTP Post instead.
	GetPolicyOpportunistic GetPolicy = 1

	// GetPolicyEnforce will cause a client to use Get requests for eligible
	// (side-effect free) procedures, and fail with an error if the request
	// would exceed the configured max URL size.
	GetPolicyEnforce GetPolicy = 2
)
