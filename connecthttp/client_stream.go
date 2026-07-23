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
	"errors"
	"io"

	"connectrpc.com/connect/v2"
)

// selectClientProtocol resolves a protocol name to the in-package protocol
// implementation used to build a client.
func selectClientProtocol(name string) (protocol, error) {
	switch name {
	case "", connect.ProtocolNameConnect:
		return &protocolConnect{}, nil
	case connect.ProtocolNameGRPC:
		return &protocolGRPC{web: false}, nil
	case connect.ProtocolNameGRPCWeb:
		return &protocolGRPC{web: true}, nil
	default:
		return nil, connect.Errorf(connect.CodeInternal, "unknown protocol %q", name)
	}
}

// connectUnaryClientStream adapts a unary [streamingClientConn] to [connect.ClientStream].
type connectUnaryClientStream struct {
	conn        streamingClientConn
	info        *connect.CallInfo
	protoClient protocolClient
	streamType  connect.StreamType

	headerFlushed bool
	sentOnce      bool
	sendClosed    bool
	rxEnd         bool
}

// flushHeader merges request metadata, then writes protocol headers.
func (s *connectUnaryClientStream) flushHeader() {
	if s.headerFlushed {
		return
	}
	s.headerFlushed = true
	header := s.conn.RequestHeader()
	if s.info != nil {
		toHTTPHeader(header, s.info.RequestHeader())
	}
	s.protoClient.WriteRequestHeader(s.streamType, header)
}

func (s *connectUnaryClientStream) SendHeaders() error {
	s.flushHeader()
	return nil
}

func (s *connectUnaryClientStream) Send(msg any) error {
	if s.sendClosed {
		return io.EOF
	}
	if s.sentOnce {
		return errors.New("connecthttp: unary stream sent more than once")
	}
	s.sentOnce = true
	s.flushHeader()
	return s.conn.Send(msg)
}

func (s *connectUnaryClientStream) CloseSend() error {
	s.sendClosed = true
	s.flushHeader()
	return s.conn.CloseRequest()
}

func (s *connectUnaryClientStream) Receive(dst any) error {
	if s.rxEnd {
		return io.EOF
	}
	s.rxEnd = true
	return receiveUnaryResponse(s.conn, dst, s.info)
}

// receiveUnaryResponse reads the single response message, mapping a cardinality
// violation to CodeUnimplemented, then syncs trailers and closes the response.
func receiveUnaryResponse(conn streamingClientConn, dst any, info *connect.CallInfo) error {
	err := conn.Receive(dst)
	if err == nil {
		if drainErr := conn.Receive(dst); drainErr == nil {
			err = connect.Errorf(connect.CodeUnimplemented, "unary stream has multiple messages")
		} else if !errors.Is(drainErr, io.EOF) {
			err = drainErr
		}
	} else if errors.Is(err, io.EOF) {
		err = connect.Errorf(connect.CodeUnimplemented, "unary stream has zero messages")
	}
	if info != nil {
		fromHTTPHeader(info.ResponseHeader(), conn.ResponseHeader())
		fromHTTPHeader(info.ResponseTrailer(), conn.ResponseTrailer())
	}
	_ = conn.CloseResponse()
	return err
}

func (s *connectUnaryClientStream) Close() error {
	_ = s.conn.CloseRequest()
	return s.conn.CloseResponse()
}

// connectStreamingClientStream adapts a streaming [streamingClientConn] to [connect.ClientStream].
type connectStreamingClientStream struct {
	conn        streamingClientConn
	info        *connect.CallInfo
	protoClient protocolClient
	streamType  connect.StreamType

	headerFlushed bool
	syncedHeader  bool
	rxEnd         bool
}

// flushHeader merges request metadata, then writes protocol headers.
func (s *connectStreamingClientStream) flushHeader() {
	if s.headerFlushed {
		return
	}
	s.headerFlushed = true
	header := s.conn.RequestHeader()
	if s.info != nil {
		toHTTPHeader(header, s.info.RequestHeader())
	}
	s.protoClient.WriteRequestHeader(s.streamType, header)
}

// SendHeaders opens the stream without sending a message.
func (s *connectStreamingClientStream) SendHeaders() error {
	s.flushHeader()
	return s.conn.Send(nil)
}

func (s *connectStreamingClientStream) Send(msg any) error {
	s.flushHeader()
	return s.conn.Send(msg)
}

func (s *connectStreamingClientStream) CloseSend() error {
	s.flushHeader()
	return s.conn.CloseRequest()
}

func (s *connectStreamingClientStream) Receive(dst any) error {
	if s.streamType == connect.StreamTypeClient {
		if s.rxEnd {
			return io.EOF
		}
		s.rxEnd = true
		return receiveUnaryResponse(s.conn, dst, s.info)
	}
	err := s.conn.Receive(dst)
	if !s.syncedHeader && s.info != nil {
		fromHTTPHeader(s.info.ResponseHeader(), s.conn.ResponseHeader())
		s.syncedHeader = true
	}
	if err != nil {
		// Stream ended or failed: trailers are now available.
		if s.info != nil {
			fromHTTPHeader(s.info.ResponseTrailer(), s.conn.ResponseTrailer())
		}
		_ = s.conn.CloseResponse()
	}
	return err
}

func (s *connectStreamingClientStream) Close() error {
	_ = s.conn.CloseRequest()
	return s.conn.CloseResponse()
}

// newProtocolClient builds a v1 protocol client for spec from the transport's
// resolved options, adapting the v2 codec/compressors to the in-package types.
func (t *transport) newProtocolClient(spec connect.Spec, opts *options) (protocolClient, error) {
	proto, err := selectClientProtocol(opts.protocol)
	if err != nil {
		return nil, err
	}
	pools := make(map[string]*compressionPool, len(opts.compressors))
	for name, compressor := range opts.compressors {
		pools[name] = newCompressionPool(compressor)
	}
	return proto.NewClient(&protocolClientParams{
		CompressionName:  opts.sendCompressor,
		CompressionPools: newReadOnlyCompressionPools(pools, opts.compressorNames),
		Codec:            opts.codecs[opts.sendCodecName],
		Protobuf:         newReadOnlyCodecs(opts.codecs).Protobuf(),
		CompressMinBytes: opts.compressMinBytes,
		HTTPClient:       t.httpClient,
		URL:              t.urlForProcedure(spec.Procedure),
		ReadMaxBytes:     opts.readMaxBytes,
		SendMaxBytes:     opts.sendMaxBytes,
		EnableGet:        opts.getEnabled,
		GetURLMaxBytes:   opts.getMaxURLBytes,
		GetUseFallback:   opts.getUseFallback,
	})
}
