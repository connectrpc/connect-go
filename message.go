package rerpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	// Marshal JSON with the options required by Twirp.
	jsonpbMarshaler   = protojson.MarshalOptions{UseProtoNames: true}
	jsonpbUnmarshaler = protojson.UnmarshalOptions{DiscardUnknown: true}
	sizeZeroPrefix    = make([]byte, 4)
)

type marshaler struct {
	w        io.Writer
	ctype    string
	gzipGRPC bool
}

func (m *marshaler) Marshal(msg proto.Message) *Error {
	switch m.ctype {
	case TypeJSON:
		return m.marshalTwirpJSON(msg)
	case TypeProtoTwirp:
		return m.marshalTwirpProto(msg)
	case TypeDefaultGRPC, TypeProtoGRPC:
		return m.marshalGRPC(msg)
	default:
		return errorf(CodeInvalidArgument, "unsupported Content-Type %q", m.ctype)
	}
}

func (m *marshaler) marshalTwirpJSON(msg proto.Message) *Error {
	bs, err := jsonpbMarshaler.Marshal(msg)
	if err != nil {
		return wrap(CodeInternal, err) // errors here should be impossible
	}
	if _, err = m.w.Write(bs); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (m *marshaler) marshalTwirpProto(msg proto.Message) *Error {
	bs, err := proto.Marshal(msg)
	if err != nil {
		return wrap(CodeInternal, err) // errors here should be impossible
	}
	if _, err = m.w.Write(bs); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (m *marshaler) marshalGRPC(msg proto.Message) *Error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return errorf(CodeInternal, "couldn't marshal protobuf message: %w", err)
	}
	if !m.gzipGRPC || !isWorthCompressing(raw) {
		if err := m.writeGRPCPrefix(false, len(raw)); err != nil {
			return err // already enriched
		}
		if _, err := m.w.Write(raw); err != nil {
			return errorf(CodeUnknown, "couldn't write message of length-prefixed message: %w", err)
		}
		return nil
	}
	data := getBuffer()
	defer putBuffer(data)
	gw := getGzipWriter(data)
	defer putGzipWriter(gw)

	if _, err = gw.Write(raw); err != nil { // returns uncompressed size, which isn't useful
		return errorf(CodeInternal, "couldn't gzip data: %w", err)
	}
	if err := gw.Close(); err != nil {
		return errorf(CodeInternal, "couldn't close gzip writer: %w", err)
	}
	if err := m.writeGRPCPrefix(true, data.Len()); err != nil {
		return err // already enriched
	}
	if _, err := io.Copy(m.w, data); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return errorf(CodeUnknown, "couldn't write message of length-prefixed message: %w", err)
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
		return errorf(CodeUnknown, "couldn't write prefix of length-prefixed message: %w", err)
	}
	return nil
}

type unmarshaler struct {
	r     io.Reader
	ctype string
	max   int64
}

func (u *unmarshaler) Unmarshal(msg proto.Message) *Error {
	switch u.ctype {
	case TypeJSON:
		return u.unmarshalTwirpJSON(msg)
	case TypeProtoTwirp:
		return u.unmarshalTwirpProto(msg)
	case TypeDefaultGRPC, TypeProtoGRPC:
		return u.unmarshalGRPC(msg)
	default:
		return errorf(CodeInvalidArgument, "unsupported Content-Type %q", u.ctype)
	}
}

func (u *unmarshaler) unmarshalTwirpJSON(msg proto.Message) *Error {
	return u.unmarshalTwirp(msg, "JSON", jsonpbUnmarshaler.Unmarshal)
}

func (u *unmarshaler) unmarshalTwirpProto(msg proto.Message) *Error {
	return u.unmarshalTwirp(msg, "protobuf", proto.Unmarshal)
}

func (u *unmarshaler) unmarshalTwirp(msg proto.Message, variant string, do func([]byte, proto.Message) error) *Error {
	buf := getBuffer()
	defer putBuffer(buf)
	r := u.r
	if u.max > 0 {
		r = &io.LimitedReader{
			R: r,
			N: int64(u.max),
		}
	}
	if n, err := buf.ReadFrom(r); err != nil {
		return wrap(CodeUnknown, err)
	} else if n == 0 {
		return nil // zero value
	}
	if err := do(buf.Bytes(), msg); err != nil {
		if lr, ok := r.(*io.LimitedReader); ok && lr.N <= 0 {
			// likely more informative than unmarshaling error
			return errorf(CodeInvalidArgument, "request too large: max bytes set to %v", u.max)
		}
		fqn := msg.ProtoReflect().Descriptor().FullName()
		return wrap(
			CodeInvalidArgument,
			newMalformedError(fmt.Sprintf("can't unmarshal %s into %v: %v", variant, fqn, err)),
		)
	}
	return nil
}

func (u *unmarshaler) unmarshalGRPC(msg proto.Message) *Error {
	// Each length-prefixed message starts with 5 bytes of metadata: a one-byte
	// unsigned integer indicating whether the payload is compressed, and a
	// four-byte unsigned integer indicating the message length.
	prefixes := make([]byte, 5)
	n, err := u.r.Read(prefixes)
	if err != nil && errors.Is(err, io.EOF) && n == 5 && bytes.Equal(prefixes[1:5], sizeZeroPrefix) {
		// Successfully read prefix, expect no additional data, and got an EOF, so
		// there's nothing left to do - the zero value of the msg is correct.
		return nil
	} else if err != nil || n < 5 {
		// Even an EOF is unacceptable here, since we always need a message for
		// unary RPC.
		return errorf(
			CodeInvalidArgument,
			"gRPC protocol error: missing length-prefixed message metadata: %w", err,
		)
	}

	// TODO: grpc-web uses the MSB of this byte to indicate that the LPM contains
	// trailers.
	var compressed bool
	switch prefixes[0] {
	case 0:
		compressed = false
	case 1:
		compressed = true
	default:
		return errorf(
			CodeInvalidArgument,
			"gRPC protocol error: length-prefixed message has invalid compressed flag %v", prefixes[0],
		)
	}

	size := int(binary.BigEndian.Uint32(prefixes[1:5]))
	if size < 0 {
		return errorf(CodeInvalidArgument, "message size %d overflowed uint32", size)
	} else if u.max > 0 && int64(size) > u.max {
		return errorf(CodeInvalidArgument, "message size %d is larger than configured max %d", size, u.max)
	}
	buf := getBuffer()
	defer putBuffer(buf)

	buf.Grow(size)
	raw := buf.Bytes()[0:size]
	if size > 0 {
		n, err = u.r.Read(raw)
		if err != nil && !errors.Is(err, io.EOF) {
			return errorf(CodeUnknown, "error reading length-prefixed message data: %w", err)
		}
		if n < size {
			return errorf(
				CodeInvalidArgument,
				"gRPC protocol error: promised %d bytes in length-prefixed message, got %d bytes", size, n,
			)
		}
	}

	if compressed {
		gr, err := getGzipReader(bytes.NewReader(raw))
		if err != nil {
			return errorf(CodeInvalidArgument, "can't decompress gzipped data: %w", err)
		}
		defer putGzipReader(gr)
		defer gr.Close()
		decompressed := getBuffer()
		defer putBuffer(decompressed)
		if _, err := decompressed.ReadFrom(gr); err != nil {
			return errorf(CodeInvalidArgument, "can't decompress gzipped data: %w", err)
		}
		raw = decompressed.Bytes()
	}

	if err := proto.Unmarshal(raw, msg); err != nil {
		fqn := msg.ProtoReflect().Descriptor().FullName()
		return errorf(CodeInvalidArgument, "can't unmarshal protobuf into %v: %w", fqn, err)
	}

	return nil
}
