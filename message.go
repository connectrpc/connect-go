package rerpc

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	jsonpbMarshaler   = protojson.MarshalOptions{}
	jsonpbUnmarshaler = protojson.UnmarshalOptions{DiscardUnknown: true}
	sizeZeroPrefix    = make([]byte, 4)
)

func marshalJSON(w io.Writer, msg proto.Message) error {
	bs, err := jsonpbMarshaler.Marshal(msg)
	if err != nil {
		return fmt.Errorf("couldn't marshal protobuf message: %w", err)
	}
	if _, err := w.Write(bs); err != nil {
		return fmt.Errorf("couldn't write JSON: %w", err)
	}
	return nil
}

func unmarshalJSON(r io.Reader, msg proto.Message) error {
	bs, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("can't read data: %w", err)
	}
	if err := jsonpbUnmarshaler.Unmarshal(bs, msg); err != nil {
		return fmt.Errorf("can't unmarshal data into type %T: %w", msg, err)
	}
	return nil
}

func marshalLPM(w io.Writer, msg proto.Message, compression string, maxBytes int) error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("couldn't marshal protobuf message: %w", err)
	}
	data := &bytes.Buffer{}
	var dataW io.Writer = data
	switch compression {
	case CompressionIdentity:
	case CompressionGzip:
		dataW = gzip.NewWriter(data)
	default:
		return fmt.Errorf("unsupported compression %q", compression)
	}
	_, err = dataW.Write(raw) // returns uncompressed size, which isn't useful
	if err != nil {
		return fmt.Errorf("couldn't compress with %q: %w", compression, err)
	}
	if c, ok := dataW.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("couldn't compress with %q: %w", compression, err)
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
		return fmt.Errorf("couldn't write prefix of length-prefixed message: %w", err)
	}
	if _, err := io.Copy(w, data); err != nil {
		return fmt.Errorf("couldn't write data portion of length-prefixed message: %w", err)
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
