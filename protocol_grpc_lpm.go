package connect

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/textproto"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/compress"
)

const (
	flagLPMCompressed = 0b00000001
	flagLPMTrailer    = 0b10000000
)

var (
	sizeZeroPrefix    = make([]byte, 4)
	errGotWebTrailers = Errorf(
		CodeUnknown,
		"end of stream: got gRPC-Web trailers instead of response message: %w",
		// User code checks for end of stream with errors.Is(err, io.EOF).
		io.EOF,
	)
)

type marshaler struct {
	w          io.Writer
	compressor compress.Compressor
	codec      codec.Codec
}

func (m *marshaler) Marshal(msg any) *Error {
	raw, err := m.codec.Marshal(msg)
	if err != nil {
		return Errorf(CodeInternal, "couldn't marshal message: %w", err)
	}
	return m.writeLPM(false /* trailer */, raw)
}

func (m *marshaler) MarshalWebTrailers(trailer http.Header) *Error {
	// OPT: easy opportunity to pool buffers
	raw := bytes.NewBuffer(nil)
	if err := trailer.Write(raw); err != nil {
		return Errorf(CodeInternal, "couldn't format trailers: %w", err)
	}
	return m.writeLPM(true /* trailer */, raw.Bytes())
}

func (m *marshaler) writeLPM(trailer bool, message []byte) *Error {
	if m.compressor == nil || !m.compressor.ShouldCompress(message) {
		if err := m.writeGRPCPrefix(false /* compressed */, trailer, len(message)); err != nil {
			return err // already enriched
		}
		if _, err := m.w.Write(message); err != nil {
			return Errorf(CodeUnknown, "couldn't write message of length-prefixed message: %w", err)
		}
		return nil
	}
	// OPT: easy opportunity to pool buffers
	data := bytes.NewBuffer(make([]byte, 0, len(message)))
	cw := m.compressor.GetWriter(data)
	defer m.compressor.PutWriter(cw)

	if _, err := cw.Write(message); err != nil { // returns uncompressed size, which isn't useful
		return Errorf(CodeInternal, "couldn't compress data: %w", err)
	}
	if err := cw.Close(); err != nil {
		return Errorf(CodeInternal, "couldn't close compressing writer: %w", err)
	}
	if err := m.writeGRPCPrefix(true /* compressed */, trailer, data.Len()); err != nil {
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

func (m *marshaler) writeGRPCPrefix(compressed, trailer bool, size int) *Error {
	prefixes := [5]byte{}
	if compressed {
		prefixes[0] = flagLPMCompressed
	}
	if trailer {
		prefixes[0] = prefixes[0] | flagLPMTrailer
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
	r          io.Reader
	max        int64
	codec      codec.Codec
	compressor compress.Compressor

	web        bool
	webTrailer http.Header
}

func (u *unmarshaler) Unmarshal(msg any) *Error {
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
		// Even an EOF is unacceptable here, since we always need a message.
		return Errorf(
			CodeInvalidArgument,
			"gRPC protocol error: missing length-prefixed message metadata: %w", err,
		)
	}

	flags := uint8(prefixes[0])
	// LPM is compressed if the least significant bit is set.
	compressed := (flags&flagLPMCompressed == flagLPMCompressed)
	// gRPC-Web uses the most significant bit to indicate that the LPM contains trailers.
	isWebTrailer := u.web && (flags&flagLPMTrailer == flagLPMTrailer)
	// We could check to make sure that the remaining bits are zero, but any
	// non-zero bits are likely flags from a future protocol revision. In a sane
	// world, any new flags would be backward-compatible and safe to ignore.
	// Let's be optimistic!

	size := int(binary.BigEndian.Uint32(prefixes[1:5]))
	if size < 0 {
		return Errorf(CodeInvalidArgument, "message size %d overflowed uint32", size)
	} else if u.max > 0 && int64(size) > u.max {
		return Errorf(CodeInvalidArgument, "message size %d is larger than configured max %d", size, u.max)
	}
	// OPT: easy opportunity to pool buffers and grab the underlying byte slice
	raw := make([]byte, size)
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

	if compressed && u.compressor == nil {
		return Errorf(
			CodeInvalidArgument,
			"gRPC protocol error: sent compressed message without Grpc-Encoding header",
		)
	}

	if compressed {
		cr, err := u.compressor.GetReader(bytes.NewReader(raw))
		if err != nil {
			return Errorf(CodeInvalidArgument, "can't decompress gzipped data: %w", err)
		}
		defer u.compressor.PutReader(cr)
		defer cr.Close()
		// OPT: easy opportunity to pool buffers
		decompressed := bytes.NewBuffer(make([]byte, 0, len(raw)))
		if _, err := decompressed.ReadFrom(cr); err != nil {
			return Errorf(CodeInvalidArgument, "can't decompress gzipped data: %w", err)
		}
		raw = decompressed.Bytes()
	}

	if isWebTrailer {
		// Per the gRPC-Web specification, trailers should be encoded as an HTTP/1
		// headers block _without_ the terminating newline. To make the headers
		// parseable by net/textproto, we need to add the newline.
		raw = append(raw, '\n')
		bufferedReader := bufio.NewReader(bytes.NewReader(raw))
		mimeReader := textproto.NewReader(bufferedReader)
		mimeHeader, err := mimeReader.ReadMIMEHeader()
		if err != nil {
			return Errorf(
				CodeInvalidArgument,
				"gRPC-Web protocol error: received invalid trailers %q: %w",
				string(raw),
				err,
			)
		}
		u.webTrailer = http.Header(mimeHeader)
		return errGotWebTrailers
	}

	if err := u.codec.Unmarshal(raw, msg); err != nil {
		return Errorf(CodeInvalidArgument, "can't unmarshal into %T: %w", msg, err)
	}

	return nil
}

func (u *unmarshaler) WebTrailer() http.Header {
	return u.webTrailer
}
