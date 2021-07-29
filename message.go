package rerpc

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	// Marshal JSON with the options required by Twirp.
	jsonpbMarshaler   = protojson.MarshalOptions{UseProtoNames: true}
	jsonpbUnmarshaler = protojson.UnmarshalOptions{DiscardUnknown: true}
	sizeZeroPrefix    = make([]byte, 4)
)

func marshalJSON(ctx context.Context, w io.Writer, msg proto.Message, hooks *Hooks) {
	bs, err := jsonpbMarshaler.Marshal(msg)
	if err != nil {
		hooks.onMarshalError(ctx, fmt.Errorf("couldn't marshal protobuf message: %w", err))
		return
	}
	if _, err := w.Write(bs); err != nil {
		hooks.onNetworkError(ctx, fmt.Errorf("couldn't write JSON: %w", err))
		return
	}
}

func marshalTwirpProto(ctx context.Context, w io.Writer, msg proto.Message, hooks *Hooks) {
	bs, err := proto.Marshal(msg)
	if err != nil {
		hooks.onMarshalError(ctx, fmt.Errorf("couldn't marshal protobuf message: %w", err))
		return
	}
	if _, err := w.Write(bs); err != nil {
		hooks.onNetworkError(ctx, fmt.Errorf("couldn't write Twirp protobuf: %w", err))
		return
	}
}

func unmarshalJSON(r io.Reader, msg proto.Message) error {
	bs, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("can't read data: %w", err)
	}
	if err := jsonpbUnmarshaler.Unmarshal(bs, msg); err != nil {
		return fmt.Errorf("can't unmarshal JSON data into type %T: %w", msg, err)
	}
	return nil
}

func unmarshalTwirpProto(r io.Reader, msg proto.Message) error {
	bs, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("can't read data: %w", err)
	}
	if err := proto.Unmarshal(bs, msg); err != nil {
		return fmt.Errorf("can't unmarshal protobuf data into type %T: %w", msg, err)
	}
	return nil
}

func marshalLPM(ctx context.Context, w io.Writer, msg proto.Message, compression string, maxBytes int, hooks *Hooks) error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		err = fmt.Errorf("couldn't marshal protobuf message: %w", err)
		hooks.onMarshalError(ctx, err)
		return err
	}
	data := &bytes.Buffer{}
	var dataW io.Writer = data
	switch compression {
	case CompressionIdentity:
	case CompressionGzip:
		dataW = gzip.NewWriter(data)
	default:
		err := fmt.Errorf("unsupported length-prefixed message compression %q", compression)
		hooks.onInternalError(ctx, err)
		return err
	}
	_, err = dataW.Write(raw) // returns uncompressed size, which isn't useful
	if err != nil {
		err = fmt.Errorf("couldn't compress with %q: %w", compression, err)
		hooks.onInternalError(ctx, err)
		return err
	}
	if c, ok := dataW.(io.Closer); ok {
		if err := c.Close(); err != nil {
			err = fmt.Errorf("couldn't close writer with compression %q: %w", compression, err)
			hooks.onInternalError(ctx, err)
			return err
		}
	}

	size := data.Len()
	if maxBytes > 0 && size > maxBytes {
		return fmt.Errorf("message too large: got %d bytes, max is %d", size, maxBytes)
	}
	prefixes := [5]byte{}
	if compression == CompressionIdentity {
		prefixes[0] = 0
	} else {
		prefixes[0] = 1
	}
	binary.BigEndian.PutUint32(prefixes[1:5], uint32(size))

	if _, err := w.Write(prefixes[:]); err != nil {
		err = fmt.Errorf("couldn't write prefix of length-prefixed message: %w", err)
		hooks.onNetworkError(ctx, err)
		return err
	}
	if _, err := io.Copy(w, data); err != nil {
		err = fmt.Errorf("couldn't write data portion of length-prefixed message: %w", err)
		hooks.onNetworkError(ctx, err)
		return err
	}
	return nil
}

func unmarshalLPM(r io.Reader, msg proto.Message, compression string, maxBytes int) error {
	// Each length-prefixed message starts with 5 bytes of metadata: a one-byte
	// unsigned integer indicating whether the payload is compressed, and a
	// four-byte unsigned integer indicating the message length.
	prefixes := make([]byte, 5)
	n, err := r.Read(prefixes)
	if err != nil && errors.Is(err, io.EOF) && n == 5 && bytes.Equal(prefixes[1:5], sizeZeroPrefix) {
		// Successfully read prefix, expect no additional data, and got an EOF, so
		// there's nothing left to do - the zero value of the msg is correct.
		return nil
	} else if err != nil || n < 5 {
		// Even an EOF is unacceptable here, since we always need a message for
		// unary RPC.
		return fmt.Errorf("gRPC protocol error: missing length-prefixed message metadata: %w", err)
	}

	var compressed bool
	switch prefixes[0] {
	case 0:
		compressed = false
		if compression != CompressionIdentity {
			return fmt.Errorf("gRPC protocol error: protobuf is uncompressed but message compression is %q", compression)
		}
	case 1:
		compressed = true
		if compression == CompressionIdentity {
			return errors.New("gRPC protocol error: protobuf is compressed but message should be uncompressed")
		}
	default:
		return fmt.Errorf("gRPC protocol error: length-prefixed message has invalid compressed flag %v", prefixes[0])
	}

	size := int(binary.BigEndian.Uint32(prefixes[1:5]))
	if size < 0 {
		return fmt.Errorf("message size %d overflows uint32", size)
	}
	if maxBytes > 0 && size > maxBytes {
		return fmt.Errorf("message too large: got %d bytes, max is %d", size, maxBytes)
	}

	raw := make([]byte, size)
	if size > 0 {
		n, err = r.Read(raw)
		if err != nil && err != io.EOF {
			return fmt.Errorf("error reading length-prefixed message data: %w", err)
		}
		if n < size {
			return fmt.Errorf("gRPC protocol error: promised %d bytes in length-prefixed message, got %d bytes", size, n)
		}
	}

	if compressed && compression == CompressionGzip {
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("can't decompress gzipped data: %w", err)
		}
		defer gr.Close()
		decompressed, err := ioutil.ReadAll(gr)
		if err != nil {
			return fmt.Errorf("can't decompress gzipped data: %w", err)
		}
		raw = decompressed
	}

	if err := proto.Unmarshal(raw, msg); err != nil {
		return fmt.Errorf("can't unmarshal data into type %T: %w", msg, err)
	}

	return nil
}
