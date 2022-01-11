package rerpc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	statuspb "github.com/rerpc/rerpc/internal/gen/proto/go/grpc/status/v1"
)

// Request is a request message and a variety of metadata.
type Request[Req any] struct {
	Msg *Req

	spec Specification
	hdr  Header
}

func NewRequest[Req any](msg *Req) *Request[Req] {
	return &Request[Req]{
		Msg: msg,
		hdr: Header{raw: make(http.Header)},
	}
}

func ReceiveRequest[Req any](stream Stream) (*Request[Req], error) {
	var msg Req
	if err := stream.Receive(&msg); err != nil {
		return nil, err
	}
	return &Request[Req]{
		Msg:  &msg,
		spec: stream.Spec(),
		hdr:  stream.ReceivedHeader(),
	}, nil
}

func (r *Request[_]) Any() any {
	return r.Msg
}

func (r *Request[_]) Spec() Specification {
	return r.spec
}

func (r *Request[_]) Header() Header {
	return r.hdr
}

func (r *Request[_]) internalOnly() {}

type Response[Res any] struct {
	Msg *Res

	hdr Header
}

func NewResponse[Res any](msg *Res) *Response[Res] {
	return &Response[Res]{
		Msg: msg,
		hdr: Header{raw: make(http.Header)},
	}
}

func newResponseWithHeader[Res any](msg *Res, header Header) *Response[Res] {
	return &Response[Res]{
		Msg: msg,
		hdr: header,
	}
}

func (r *Response[_]) Any() any {
	return r.Msg
}

func (r *Response[_]) Header() Header {
	return r.hdr
}

func (r *Response[_]) internalOnly() {}

// Stream is a bidirectional stream of protobuf messages.
//
// Stream implementations must support a limited form of concurrency: one
// goroutine may call Send and CloseSend, and another may call Receive and
// CloseReceive. Either goroutine may call Context.
type Stream interface {
	Spec() Specification

	// Implementations must ensure that Send, CloseSend, and Header don't race
	// with Context, Receive, CloseReceive, ReceivedHeader, and ReceivedTrailer.
	// They may race with each other.
	Send(any) error
	CloseSend(error) error
	Header() Header

	// Implementations must ensure that Receive, CloseReceive, ReceivedHeader,
	// and ReceivedTrailer don't race with Context, Send, CloseSend, or Header.
	// They may race with each other.
	Receive(any) error
	CloseReceive() error
	ReceivedHeader() Header // blocks until response headers arrive
}

// clientStream represents a bidirectional exchange of protobuf messages
// between the client and server. The request body is the stream from client to
// server, and the response body is the reverse.
//
// The way we do this with net/http is very different from the typical HTTP/1.1
// request/response code. Since this is the most complex code in reRPC, it has
// many more comments than usual.
type clientStream struct {
	ctx          context.Context
	doer         Doer
	url          string
	spec         Specification
	maxReadBytes int64

	// send
	prepareOnce sync.Once
	writer      *io.PipeWriter
	marshaler   marshaler
	header      Header

	// receive goroutine
	reader        *io.PipeReader
	response      *http.Response
	responseReady chan struct{}
	unmarshaler   unmarshaler

	responseErrMu sync.Mutex
	responseErr   error
}

var _ Stream = (*clientStream)(nil)

func newClientStream(
	ctx context.Context,
	doer Doer,
	baseURL string,
	spec Specification,
	header Header,
	gzipRequest bool,
	maxReadBytes int64,
) *clientStream {
	// In a typical HTTP/1.1 request, we'd put the body into a bytes.Buffer, hand
	// the buffer to http.NewRequest, and fire off the request with doer.Do. That
	// won't work here because we're establishing a stream - we don't even have
	// all the data we'll eventually send. Instead, we use io.Pipe as the request
	// body.
	//
	// net/http will own the read side of the pipe, and we'll hold onto the write
	// side. Writes to pw will block until net/http pulls the data from pr and
	// puts it onto the network - there's no buffer between the two. (The two
	// sides of the pipe are meant to be used concurrently.) Once the server gets
	// the first protobuf message that we send, it'll send back headers and start
	// the response stream.
	pr, pw := io.Pipe()
	return &clientStream{
		ctx:           ctx,
		doer:          doer,
		url:           baseURL + "/" + spec.Procedure,
		spec:          spec,
		maxReadBytes:  maxReadBytes,
		writer:        pw,
		marshaler:     marshaler{w: pw, gzip: gzipRequest},
		header:        header,
		reader:        pr,
		responseReady: make(chan struct{}),
	}
}

func (cs *clientStream) Spec() Specification {
	return cs.spec
}

func (cs *clientStream) Header() Header {
	return cs.header
}

func (cs *clientStream) Send(m any) error {
	// stream.makeRequest hands the read side of the pipe off to net/http and
	// waits to establish the response stream. There's a small class of errors we
	// can catch without even sending data over the network, though, so we don't
	// want to start writing to the stream until we're sure that we're actually
	// waiting on the server. This makes user-visible behavior more predictable:
	// for example, if they've configured the server's base URL as
	// "hwws://acme.com", they'll always get an invalid URL error on their first
	// attempt to send.
	cs.prepareOnce.Do(func() {
		requestPrepared := make(chan struct{})
		go cs.makeRequest(requestPrepared)
		<-requestPrepared
	})
	// TODO: update this when codec is pluggable
	msg, ok := m.(proto.Message)
	if !ok {
		return Errorf(CodeInternal, "expected proto.Message, got %T", m)
	}
	// Calling Marshal writes data to the send stream. It's safe to do this while
	// makeRequest is running, because we're writing to our side of the pipe
	// (which is safe to do while net/http reads from the other side).
	if err := cs.marshaler.Marshal(msg); err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			// net/http closed the request body, so it's sure that we can't send more
			// data. In these cases, we expect a response from the server. Wait for
			// that response so we can give the user a more informative error than
			// "pipe closed".
			<-cs.responseReady
			if err := cs.getResponseError(); err != nil {
				return err
			}
		}
		// In this case, the read side of the pipe was closed with an explicit
		// error (possibly sent by the server, possibly just io.EOF). The io.Pipe
		// makes that error visible to us on the write side without any data races.
		// We've already enriched the error with a status code, so we can just
		// return it to the caller.
		return err
	}
	// Marshal returns an *Error. To avoid returning a typed nil, use a literal
	// nil here.
	return nil
}

func (cs *clientStream) CloseSend(_ error) error {
	// The user calls CloseSend to indicate that they're done sending data. All
	// we do here is close the write side of the pipe, so it's safe to do this
	// while makeRequest is running. (This method takes an error to accommodate
	// server-side streams. Clients can't send an error when they stop sending
	// data, so we just ignore it.)
	//
	// Because reRPC also supports some RPC types over HTTP/1.1, we need to be
	// careful how we expose this method to users. HTTP/1.1 doesn't support
	// bidirectional streaming - the send stream (aka request body) must be
	// closed before we start waiting on the response or we'll just block
	// forever. To make sure users don't have to worry about this, the generated
	// code for unary, client streaming, and server streaming RPCs must call
	// CloseSend automatically rather than requiring the user to do it.
	if err := cs.writer.Close(); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return Wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) Receive(m any) error {
	// TODO: update this when codec is pluggable
	msg, ok := m.(proto.Message)
	if !ok {
		return Errorf(CodeInternal, "expected proto.Message, got %T", m)
	}
	// First, we wait until we've gotten the response headers and established the
	// server-to-client side of the stream.
	<-cs.responseReady
	if err := cs.getResponseError(); err != nil {
		// The stream is already closed or corrupted.
		return err
	}
	// Consume one message from the response stream.
	err := cs.unmarshaler.Unmarshal(msg)
	if err != nil {
		// If we can't read this LPM, see if the server sent an explicit error in
		// trailers. First, we need to read the body to EOF.
		discard(cs.response.Body)
		if serverErr := extractError(cs.response.Trailer); serverErr != nil {
			cs.setResponseError(serverErr)
			return serverErr
		}
		// There's no error in the trailers, so this was probably an error
		// converting the bytes to a message, an error reading from the network, or
		// just an EOF. We're going to return it to the user, but we also want to
		// setResponseError so Send errors out.
		cs.setResponseError(err)
		return err
	}
	return nil
}

func (cs *clientStream) CloseReceive() error {
	<-cs.responseReady
	if cs.response == nil {
		return nil
	}
	discard(cs.response.Body)
	if err := cs.response.Body.Close(); err != nil {
		return Wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) ReceivedHeader() Header {
	<-cs.responseReady
	return Header{raw: cs.response.Header}
}

func (cs *clientStream) makeRequest(prepared chan struct{}) {
	// This runs concurrently with Send and CloseSend. Receive and CloseReceive
	// wait on cs.responseReady, so we can't race with them.
	defer close(cs.responseReady)

	if deadline, ok := cs.ctx.Deadline(); ok {
		untilDeadline := time.Until(deadline)
		if enc, err := encodeTimeout(untilDeadline); err == nil {
			// Tests verify that the error in encodeTimeout is unreachable, so we
			// should be safe without observability for the error case.
			cs.header.raw["Grpc-Timeout"] = []string{enc}
		}
	}

	req, err := http.NewRequestWithContext(cs.ctx, http.MethodPost, cs.url, cs.reader)
	if err != nil {
		cs.setResponseError(Errorf(CodeUnknown, "construct *http.Request: %w", err))
		close(prepared)
		return
	}
	req.Header = cs.header.raw

	// Before we send off a request, check if we're already out of time.
	if err := cs.ctx.Err(); err != nil {
		code := CodeUnknown
		if errors.Is(err, context.Canceled) {
			code = CodeCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = CodeDeadlineExceeded
		}
		cs.setResponseError(Wrap(code, err))
		close(prepared)
		return
	}

	// At this point, we've caught all the errors we can - it's time to send data
	// to the server. Unblock the constructor.
	close(prepared)
	// It's possible that the server sends back a response immediately after
	// receiving the request headers, but in most cases we'll block here until
	// the user calls Send. Once we send a message to the server, they send a
	// message back and establish the receive side of the stream.
	res, err := cs.doer.Do(req)
	if err != nil {
		code := CodeUnknown
		if errors.Is(err, context.Canceled) {
			code = CodeCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = CodeDeadlineExceeded
		}
		cs.setResponseError(Wrap(code, err))
		return
	}

	if res.StatusCode != http.StatusOK {
		code := CodeUnknown
		if c, ok := httpToGRPC[res.StatusCode]; ok {
			code = c
		}
		cs.setResponseError(Errorf(code, "HTTP status %v", res.Status))
		return
	}
	compression := res.Header.Get("Grpc-Encoding")
	if compression == "" {
		compression = CompressionIdentity
	}
	switch compression {
	case CompressionIdentity, CompressionGzip:
	default:
		// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
		// should return CodeInternal and specify acceptable compression(s) (in
		// addition to setting the Grpc-Accept-Encoding header).
		cs.setResponseError(Errorf(
			CodeInternal,
			"unknown compression %q: accepted grpc-encoding values are %v",
			compression,
			acceptEncodingValue,
		))
		return
	}
	// When there's no body, errors sent from the first-party gRPC servers will
	// be in the headers.
	if err := extractError(res.Header); err != nil {
		cs.setResponseError(err)
		return
	}
	// Success! We got a response with valid headers and no error, so there's
	// probably a message waiting in the stream.
	cs.response = res
	cs.unmarshaler = unmarshaler{r: res.Body, max: cs.maxReadBytes}
}

func (cs *clientStream) setResponseError(err error) {
	cs.responseErrMu.Lock()
	cs.responseErr = err
	cs.responseErrMu.Unlock()
	// The write end of the pipe will now return this error too. It's safe to
	// call this method more than once and/or concurrently (calls after the first
	// are no-ops), so it's okay for us to call this even though net/http
	// sometimes closes the reader too.
	cs.reader.CloseWithError(err)
}

func (cs *clientStream) getResponseError() error {
	cs.responseErrMu.Lock()
	defer cs.responseErrMu.Unlock()
	return cs.responseErr
}

type serverStream struct {
	spec        Specification
	unmarshaler unmarshaler
	marshaler   marshaler
	writer      http.ResponseWriter
	request     *http.Request
}

var _ Stream = (*serverStream)(nil)

// Thankfully, the server stream is much simpler than the client. net/http
// gives us the request body and response writer at the same time, so we don't
// need to worry about concurrency.
func newServerStream(
	spec Specification,
	w http.ResponseWriter,
	r *http.Request,
	maxReadBytes int64,
	gzipResponse bool,
) *serverStream {
	return &serverStream{
		spec:        spec,
		unmarshaler: unmarshaler{r: r.Body, max: maxReadBytes},
		marshaler:   marshaler{w: w, gzip: gzipResponse},
		writer:      w,
		request:     r,
	}
}

func (ss *serverStream) Spec() Specification {
	return ss.spec
}

func (ss *serverStream) Header() Header {
	return Header{raw: ss.writer.Header()}
}

func (ss *serverStream) ReceivedHeader() Header {
	return Header{raw: ss.request.Header}
}

func (ss *serverStream) Receive(m any) error {
	// TODO: update this when codec is pluggable
	msg, ok := m.(proto.Message)
	if !ok {
		return Errorf(CodeInternal, "expected proto.Message, got %T", m)
	}
	if err := ss.unmarshaler.Unmarshal(msg); err != nil {
		return err // already coded
	}
	// don't return typed nils
	return nil
}

func (ss *serverStream) CloseReceive() error {
	discard(ss.request.Body)
	if err := ss.request.Body.Close(); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return Wrap(CodeUnknown, err)
	}
	return nil
}

func (ss *serverStream) Send(m any) error {
	// TODO: update this when codec is pluggable
	msg, ok := m.(proto.Message)
	if !ok {
		return Errorf(CodeInternal, "expected proto.Message, got %T", m)
	}
	defer ss.flush()
	if err := ss.marshaler.Marshal(msg); err != nil {
		return err
	}
	// don't return typed nils
	return nil
}

func (ss *serverStream) CloseSend(err error) error {
	defer ss.flush()
	return ss.sendErrorGRPC(err)
}

func (ss *serverStream) sendErrorGRPC(err error) error {
	if CodeOf(err) == CodeOK { // safe for nil errors
		ss.writer.Header().Set("Grpc-Status", strconv.Itoa(int(CodeOK)))
		ss.writer.Header().Set("Grpc-Message", "")
		ss.writer.Header().Set("Grpc-Status-Details-Bin", "")
		return nil
	}
	s := statusFromError(err)
	code := strconv.Itoa(int(s.Code))
	bin, err := proto.Marshal(s)
	if err != nil {
		ss.writer.Header().Set("Grpc-Status", strconv.Itoa(int(CodeInternal)))
		ss.writer.Header().Set("Grpc-Message", percentEncode("error marshaling protobuf status with code "+code))
		return Errorf(CodeInternal, "couldn't marshal protobuf status: %w", err)
	}
	ss.writer.Header().Set("Grpc-Status", code)
	ss.writer.Header().Set("Grpc-Message", percentEncode(s.Message))
	ss.writer.Header().Set("Grpc-Status-Details-Bin", encodeBinaryHeader(bin))
	return nil
}

func (ss *serverStream) flush() {
	if f, ok := ss.writer.(http.Flusher); ok {
		f.Flush()
	}
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
