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
	"context"
	"errors"
	"io"
	"maps"
	"net/http"

	"connectrpc.com/connect/v2"
)

// handlerStream adapts a [streamingHandlerConn] to [connect.ServerStream]. It
// flushes the response metadata from [connect.CallInfo] onto the wire conn
// before the first Send (the conn writes headers on first write) and owns the
// trailing io.EOF for unary receives.
type handlerStream struct {
	conn          streamingHandlerConn
	info          *connect.CallInfo
	unary         bool
	singleRequest bool

	headerFlushed  bool
	trailerFlushed bool
	rxEnd          bool
}

func (s *handlerStream) flushHeader() {
	if s.headerFlushed {
		return
	}
	s.headerFlushed = true
	toHTTPHeader(s.conn.ResponseHeader(), s.info.ResponseHeader())
}

func (s *handlerStream) flushTrailer() {
	if s.trailerFlushed {
		return
	}
	s.trailerFlushed = true
	toHTTPHeader(s.conn.ResponseTrailer(), s.info.ResponseTrailer())
}

func (s *handlerStream) Receive(dst any) error {
	if !s.singleRequest {
		return s.conn.Receive(dst)
	}
	if s.rxEnd {
		return io.EOF
	}
	s.rxEnd = true
	if err := s.conn.Receive(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return connect.Errorf(connect.CodeUnimplemented, "unary request has zero messages")
		}
		return err
	}
	if err := s.conn.Receive(dst); err == nil {
		return connect.Errorf(connect.CodeUnimplemented, "unary request has multiple messages")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *handlerStream) SendHeaders() error { return nil }

func (s *handlerStream) Send(msg any) error {
	s.flushHeader()
	if s.unary {
		s.flushTrailer()
	}
	return s.conn.Send(msg)
}

// newServerHandlerConfig builds a v1 handlerConfig for spec from the resolved
// server options, adapting the v2 codec/compressors to the in-package types.
func newServerHandlerConfig(spec connect.Spec, opts *options) *handlerConfig {
	codecs := make(map[string]connect.Codec, len(opts.codecs))
	maps.Copy(codecs, opts.codecs)
	pools := make(map[string]*compressionPool, len(opts.compressors))
	for name, compressor := range opts.compressors {
		pools[name] = newCompressionPool(compressor)
	}
	return &handlerConfig{
		CompressionPools:             pools,
		CompressionNames:             opts.compressorNames,
		Codecs:                       codecs,
		CompressMinBytes:             opts.compressMinBytes,
		Procedure:                    spec.Procedure,
		Schema:                       spec.Schema,
		RequireConnectProtocolHeader: opts.requireConnectProtocolHeader,
		IdempotencyLevel:             spec.IdempotencyLevel,
		ReadMaxBytes:                 opts.readMaxBytes,
		SendMaxBytes:                 opts.sendMaxBytes,
		StreamType:                   spec.StreamType,
	}
}

// newProcedureHandler returns an [http.Handler] for a single procedure that
// negotiates the wire protocol (reusing the v1 [Handler.ServeHTTP] skeleton)
// and dispatches through [connect.Server.Call].
func newProcedureHandler(server *connect.Server, spec connect.Spec, opts *options) http.Handler {
	handlerCfg := newServerHandlerConfig(spec, opts.forSpec(spec))
	protocolHandlers := handlerCfg.newProtocolHandlers()
	unary := spec.StreamType == connect.StreamTypeUnary
	singleRequest := unary || spec.StreamType == connect.StreamTypeServer
	implementation := func(ctx context.Context, conn streamingHandlerConn, info *connect.CallInfo) error {
		info.Spec = spec
		info.PeerAddr = conn.Peer().Addr
		info.Protocol = conn.Peer().Protocol
		fromHTTPHeader(info.RequestHeader(), conn.RequestHeader())
		stream := &handlerStream{conn: conn, info: info, unary: unary, singleRequest: singleRequest}
		err := server.Call(ctx, spec.Procedure, info, stream)
		stream.flushHeader()
		stream.flushTrailer()
		return err
	}
	return &handler{
		spec:             handlerCfg.newSpec(),
		implementation:   implementation,
		protocolHandlers: mappedMethodHandlers(protocolHandlers),
		allowMethod:      sortedAllowMethodValue(protocolHandlers),
		acceptPost:       sortedAcceptPostValue(protocolHandlers),
	}
}
