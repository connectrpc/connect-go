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
	"net/http"
	"net/url"

	"connectrpc.com/connect/v2"
)

// streamingHandlerConn is the server's view of a bidirectional message
// exchange. Interceptors for streaming RPCs may wrap StreamingHandlerConns.
//
// Like the standard library's [http.ResponseWriter], StreamingHandlerConns write
// response headers to the network with the first call to Send. Any subsequent
// mutations are effectively no-ops. Handlers may mutate response trailers at
// any time before returning. When the client has finished sending data,
// Receive returns an error wrapping [io.EOF]. Handlers should check for this
// using the standard library's [errors.Is].
//
// Headers and trailers beginning with "Connect-" and "Grpc-" are reserved for
// use by the gRPC and Connect protocols: applications may read them but
// shouldn't write them.
//
// streamingHandlerConn implementations provided by this module guarantee that
// all returned errors can be cast to [*connect.Error] using the standard library's
// [errors.As].
//
// streamingHandlerConn implementations provided by this module support limited
// concurrent use: the read side (Receive, RequestHeader) may be called
// concurrently with the write side (Send, ResponseHeader, ResponseTrailer), but
// the read side must not be called concurrently with itself, and the write side
// must not be called concurrently with itself.
type streamingHandlerConn interface {
	Spec() connect.Spec
	Peer() peer

	// Receive and RequestHeader form the read side of the stream. They are not
	// safe to call concurrently with each other, but may be called concurrently
	// with Send, ResponseHeader, and ResponseTrailer.
	Receive(any) error
	RequestHeader() http.Header

	// Send, ResponseHeader, and ResponseTrailer form the write side of the
	// stream. They are not safe to call concurrently with each other, but may
	// be called concurrently with Receive and RequestHeader.
	Send(any) error
	ResponseHeader() http.Header
	ResponseTrailer() http.Header
}

// peer describes the other party to an RPC.
//
// When accessed client-side, Addr contains the host or host:port from the
// server's URL. When accessed server-side, Addr contains the client's address
// in IP:port format.
//
// On both the client and the server, Protocol is the RPC protocol in use.
// Currently, it's either [connect.ProtocolNameConnect], [connect.ProtocolNameGRPC],
// or [connect.ProtocolNameGRPCWeb], but additional protocols may be added in the future.
//
// Query contains the query parameters for the request. For the server, this
// will reflect the actual query parameters sent. For the client, it is unset.
type peer struct {
	Addr     string
	Protocol string
	Query    url.Values // server-only
}

func newPeerForURL(url *url.URL, protocol string) peer {
	return peer{
		Addr:     url.Host,
		Protocol: protocol,
	}
}

// handlerConnCloser extends streamingHandlerConn with a method for handlers to
// terminate the message exchange (and optionally send an error to the client).
type handlerConnCloser interface {
	streamingHandlerConn

	Close(error) error
}
