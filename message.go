package rerpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"
)

var (
	sizeZeroPrefix = make([]byte, 4)
)

type marshaler struct {
	w    io.Writer
	gzip bool
}

func (m *marshaler) Marshal(msg proto.Message) *Error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return Errorf(CodeInternal, "couldn't marshal protobuf message: %w", err)
	}
	if !m.gzip || !isWorthCompressing(raw) {
		if err := m.writeGRPCPrefix(false, len(raw)); err != nil {
			return err // already enriched
		}
		if _, err := m.w.Write(raw); err != nil {
			return Errorf(CodeUnknown, "couldn't write message of length-prefixed message: %w", err)
		}
		return nil
	}
	data := getBuffer()
	defer putBuffer(data)
	gw := getGzipWriter(data)
	defer putGzipWriter(gw)

	if _, err = gw.Write(raw); err != nil { // returns uncompressed size, which isn't useful
		return Errorf(CodeInternal, "couldn't gzip data: %w", err)
	}
	if err := gw.Close(); err != nil {
		return Errorf(CodeInternal, "couldn't close gzip writer: %w", err)
	}
	if err := m.writeGRPCPrefix(true, data.Len()); err != nil {
		return err // already enriched
	}
	if _, err := io.Copy(m.w, data); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return Errorf(CodeUnknown, "couldn't write message of length-prefixed message: %w", err)
	}
	return nil
}

func (m *marshaler) writeGRPCPrefix(compressed bool, size int) *Error {
	prefixes := [5]byte{}
	if compressed {
		prefixes[0] = 1
	}
	binary.BigEndian.PutUint32(prefixes[1:5], uint32(size))
	if _, err := m.w.Write(prefixes[:]); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return Errorf(CodeUnknown, "couldn't write prefix of length-prefixed message: %w", err)
	}
	return nil
}

type unmarshaler struct {
	r   io.Reader
	max int64
}

func (u *unmarshaler) Unmarshal(msg proto.Message) *Error {
	// Each length-prefixed message starts with 5 bytes of metadata: a one-byte
	// unsigned integer indicating whether the payload is compressed, and a
	// four-byte unsigned integer indicating the message length.
	prefixes := make([]byte, 5)
	n, err := u.r.Read(prefixes)
	if (err == nil || errors.Is(err, io.EOF)) && n == 5 && bytes.Equal(prefixes[1:5], sizeZeroPrefix) {
		// Successfully read prefix and expect no additional data, so there's
		// nothing left to do - the zero value of the msg is correct.
		return nil
	} else if err != nil || n < 5 {
		// Even an EOF is unacceptable here, since we always need a message for
		// unary RPC.
		return Errorf(
			CodeInvalidArgument,
			"gRPC protocol error: missing length-prefixed message metadata: %w", err,
		)
	}

	// NB: grpc-web uses the MSB of this byte to indicate that the LPM contains
	// trailers.
	var compressed bool
	switch prefixes[0] {
	case 0:
		compressed = false
	case 1:
		compressed = true
	default:
		return Errorf(
			CodeInvalidArgument,
			"gRPC protocol error: length-prefixed message has invalid compressed flag %v", prefixes[0],
		)
	}

	size := int(binary.BigEndian.Uint32(prefixes[1:5]))
	if size < 0 {
		return Errorf(CodeInvalidArgument, "message size %d overflowed uint32", size)
	} else if u.max > 0 && int64(size) > u.max {
		return Errorf(CodeInvalidArgument, "message size %d is larger than configured max %d", size, u.max)
	}
	buf := getBuffer()
	defer putBuffer(buf)

	buf.Grow(size)
	raw := buf.Bytes()[0:size]
	if size > 0 {
		// At layer 7, we don't know exactly what's happening down in L4. Large
		// length-prefixed messages may arrive in chunks, so we may need to read
		// the request body past EOF. We also need to take care that we don't retry
		// forever if the LPM is malformed.
		remaining := size
		for remaining > 0 {
			n, err = u.r.Read(raw[size-remaining : size])
			if err != nil && !errors.Is(err, io.EOF) {
				return Errorf(CodeUnknown, "error reading length-prefixed message data: %w", err)
			}
			if errors.Is(err, io.EOF) && n == 0 {
				// Message is likely malformed, stop waiting around.
				return Errorf(
					CodeInvalidArgument,
					"gRPC protocol error: promised %d bytes in length-prefixed message, got %d bytes",
					size,
					size-remaining,
				)
			}
			remaining -= n
		}
	}

	if compressed {
		gr, err := getGzipReader(bytes.NewReader(raw))
		if err != nil {
			return Errorf(CodeInvalidArgument, "can't decompress gzipped data: %w", err)
		}
		defer putGzipReader(gr)
		defer gr.Close()
		decompressed := getBuffer()
		defer putBuffer(decompressed)
		if _, err := decompressed.ReadFrom(gr); err != nil {
			return Errorf(CodeInvalidArgument, "can't decompress gzipped data: %w", err)
		}
		raw = decompressed.Bytes()
	}

	if err := proto.Unmarshal(raw, msg); err != nil {
		fqn := msg.ProtoReflect().Descriptor().FullName()
		return Errorf(CodeInvalidArgument, "can't unmarshal protobuf into %v: %w", fqn, err)
	}

	return nil
}
