package connect

import (
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/compress"
)

// Thankfully, the handler stream is much simpler than the client. net/http
// gives us the request body and response writer at the same time, so we don't
// need to worry about concurrency.
func newHandlerStream(
	spec Specification,
	web bool,
	w http.ResponseWriter,
	r *http.Request,
	maxReadBytes int64,
	codec codec.Codec,
	protobuf codec.Codec,
	requestCompressor compress.Compressor,
	responseCompressor compress.Compressor,
) (*handlerSender, *handlerReceiver) {
	sender := &handlerSender{
		spec: spec,
		web:  web,
		marshaler: marshaler{
			w:          w,
			compressor: responseCompressor,
			codec:      codec,
		},
		protobuf: protobuf,
		writer:   w,
	}
	receiver := &handlerReceiver{
		spec: spec,
		unmarshaler: unmarshaler{
			r:          r.Body,
			max:        maxReadBytes,
			compressor: requestCompressor,
			codec:      codec,
		},
		request: r,
	}
	return sender, receiver
}

type handlerSender struct {
	spec      Specification
	web       bool
	marshaler marshaler
	protobuf  codec.Codec // for errors
	writer    http.ResponseWriter
}

var _ Sender = (*handlerSender)(nil)

func (hs *handlerSender) Send(m any) error {
	// TODO: update this when codec is pluggable
	msg, ok := m.(proto.Message)
	if !ok {
		return Errorf(CodeInternal, "expected proto.Message, got %T", m)
	}
	defer hs.flush()
	if err := hs.marshaler.Marshal(msg); err != nil {
		return err
	}
	// don't return typed nils
	return nil
}

func (hs *handlerSender) Close(err error) error {
	defer hs.flush()
	if !hs.web {
		return grpcErrorToTrailer(hs.writer.Header(), hs.protobuf, err)
	}
	trailer := make(http.Header, 3)
	if trailerErr := grpcErrorToTrailer(trailer, hs.protobuf, err); trailerErr != nil {
		return trailerErr
	}
	if marshalErr := hs.marshaler.MarshalWebTrailers(trailer); marshalErr != nil {
		return marshalErr
	}
	return nil
}

func (hs *handlerSender) Spec() Specification {
	return hs.spec
}

func (hs *handlerSender) Header() http.Header {
	return hs.writer.Header()
}

func (hs *handlerSender) flush() {
	if f, ok := hs.writer.(http.Flusher); ok {
		f.Flush()
	}
}

type handlerReceiver struct {
	spec        Specification
	unmarshaler unmarshaler
	request     *http.Request
}

var _ Receiver = (*handlerReceiver)(nil)

func (hr *handlerReceiver) Receive(m any) error {
	// TODO: update this when codec is pluggable
	msg, ok := m.(proto.Message)
	if !ok {
		return Errorf(CodeInternal, "expected proto.Message, got %T", m)
	}
	if err := hr.unmarshaler.Unmarshal(msg); err != nil {
		return err // already coded
	}
	// don't return typed nils
	return nil
}

func (hr *handlerReceiver) Close() error {
	discard(hr.request.Body)
	if err := hr.request.Body.Close(); err != nil {
		if cerr, ok := AsError(err); ok {
			return cerr
		}
		return Wrap(CodeUnknown, err)
	}
	return nil
}

func (hr *handlerReceiver) Spec() Specification {
	return hr.spec
}

func (hr *handlerReceiver) Header() http.Header {
	return hr.request.Header
}
