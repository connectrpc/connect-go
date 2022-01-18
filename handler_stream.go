package rerpc

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/proto"

	"github.com/rerpc/rerpc/compress"
	statuspb "github.com/rerpc/rerpc/internal/gen/proto/go/grpc/status/v1"
)

// Thankfully, the handler stream is much simpler than the client. net/http
// gives us the request body and response writer at the same time, so we don't
// need to worry about concurrency.
func newHandlerStream(
	spec Specification,
	w http.ResponseWriter,
	r *http.Request,
	maxReadBytes int64,
	requestCompressor compress.Compressor,
	responseCompressor compress.Compressor,
) (*handlerSender, *handlerReceiver) {
	sender := &handlerSender{
		spec:      spec,
		marshaler: marshaler{w: w, compressor: responseCompressor},
		writer:    w,
	}
	receiver := &handlerReceiver{
		spec:        spec,
		unmarshaler: unmarshaler{r: r.Body, max: maxReadBytes, compressor: requestCompressor},
		request:     r,
	}
	return sender, receiver
}

type handlerSender struct {
	spec      Specification
	marshaler marshaler
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
	return hs.sendErrorGRPC(err)
}

func (hs *handlerSender) Spec() Specification {
	return hs.spec
}

func (hs *handlerSender) Header() Header {
	return Header{raw: hs.writer.Header()}
}

func (hs *handlerSender) sendErrorGRPC(err error) error {
	if CodeOf(err) == CodeOK { // safe for nil errors
		hs.writer.Header().Set("Grpc-Status", strconv.Itoa(int(CodeOK)))
		hs.writer.Header().Set("Grpc-Message", "")
		hs.writer.Header().Set("Grpc-Status-Details-Bin", "")
		return nil
	}
	s := statusFromError(err)
	code := strconv.Itoa(int(s.Code))
	bin, err := proto.Marshal(s)
	if err != nil {
		hs.writer.Header().Set("Grpc-Status", strconv.Itoa(int(CodeInternal)))
		hs.writer.Header().Set("Grpc-Message", percentEncode("error marshaling protobuf status with code "+code))
		return Errorf(CodeInternal, "couldn't marshal protobuf status: %w", err)
	}
	hs.writer.Header().Set("Grpc-Status", code)
	hs.writer.Header().Set("Grpc-Message", percentEncode(s.Message))
	hs.writer.Header().Set("Grpc-Status-Details-Bin", encodeBinaryHeader(bin))
	return nil
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
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return Wrap(CodeUnknown, err)
	}
	return nil
}

func (hr *handlerReceiver) Spec() Specification {
	return hr.spec
}

func (hr *handlerReceiver) Header() Header {
	return Header{raw: hr.request.Header}
}

func statusFromError(err error) *statuspb.Status {
	s := &statuspb.Status{
		Code:    int32(CodeUnknown),
		Message: err.Error(),
	}
	if re, ok := AsError(err); ok {
		s.Code = int32(re.Code())
		s.Details = re.Details()
		if e := re.Unwrap(); e != nil {
			s.Message = e.Error() // don't repeat code
		}
	}
	return s
}

func extractError(h http.Header) *Error {
	codeHeader := h.Get("Grpc-Status")
	if codeHeader == "" || codeHeader == "0" {
		return nil
	}

	code, err := strconv.ParseUint(codeHeader, 10 /* base */, 32 /* bitsize */)
	if err != nil {
		return Errorf(CodeUnknown, "gRPC protocol error: got invalid error code %q", codeHeader)
	}
	message := percentDecode(h.Get("Grpc-Message"))
	ret := Wrap(Code(code), errors.New(message))

	detailsBinaryEncoded := h.Get("Grpc-Status-Details-Bin")
	if len(detailsBinaryEncoded) > 0 {
		detailsBinary, err := decodeBinaryHeader(detailsBinaryEncoded)
		if err != nil {
			return Errorf(CodeUnknown, "server returned invalid grpc-error-details-bin trailer: %w", err)
		}
		var status statuspb.Status
		if err := proto.Unmarshal(detailsBinary, &status); err != nil {
			return Errorf(CodeUnknown, "server returned invalid protobuf for error details: %w", err)
		}
		ret.details = status.Details
		// Prefer the protobuf-encoded data to the headers (grpc-go does this too).
		ret.code = Code(status.Code)
		ret.err = errors.New(status.Message)
	}

	return ret
}

func discard(r io.Reader) {
	if lr, ok := r.(*io.LimitedReader); ok {
		io.Copy(io.Discard, lr)
		return
	}
	// We don't want to get stuck throwing data away forever, so limit how much
	// we're willing to do here: at most, we'll copy 4 MiB.
	lr := &io.LimitedReader{R: r, N: 1024 * 1024 * 4}
	io.Copy(io.Discard, lr)
}
