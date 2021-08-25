package rerpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	statuspb "github.com/rerpc/rerpc/internal/status/v1"
	"github.com/rerpc/rerpc/internal/twirp"
)

// Stream is a bidirectional stream of protobuf messages.
//
// Stream implementations must support a limited form of concurrency: one
// goroutine may call Send and CloseSend, and another may call Receive and
// CloseReceive. Either goroutine may call Context.
type Stream interface {
	// Implementations must ensure that Context is safe to call concurrently. It
	// must not race with any other methods.
	Context() context.Context

	// Implementations must ensure that Send and CloseSend don't race with
	// Context, Receive, or CloseReceive. They may race with each other.
	Send(proto.Message) error
	CloseSend(error) error

	// Implementations must ensure that Receive and CloseReceive don't race with
	// Context, Send, or CloseSend. They may race with each other.
	Receive(proto.Message) error
	CloseReceive() error
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
	maxReadBytes int64

	// send
	writer    *io.PipeWriter
	marshaler marshaler

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
	url string,
	maxReadBytes int64,
	gzipRequest bool,
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
	stream := clientStream{
		ctx:           ctx,
		doer:          doer,
		url:           url,
		maxReadBytes:  maxReadBytes,
		writer:        pw,
		marshaler:     marshaler{w: pw, ctype: TypeDefaultGRPC, gzipGRPC: gzipRequest},
		reader:        pr,
		responseReady: make(chan struct{}),
	}
	// stream.makeRequest hands the read side of the pipe off to net/http and
	// waits to establish the response stream. There's a small class of errors we
	// can catch without even sending data over the network, though, so we don't
	// return from this constructor until we're sure that we're actually waiting
	// on the server. This makes user-visible behavior more predictable: for
	// example, if they've configured the server's base URL as "hwws://acme.com",
	// they'll always get an invalid URL error on their first attempt to send.
	requestPrepared := make(chan struct{})
	go stream.makeRequest(requestPrepared)
	<-requestPrepared
	return &stream
}

func (cs *clientStream) Context() context.Context {
	return cs.ctx
}

func (cs *clientStream) Send(msg proto.Message) error {
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
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) Receive(msg proto.Message) error {
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
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) makeRequest(prepared chan struct{}) {
	// This runs concurrently with Send and CloseSend. Receive and CloseReceive
	// wait on cs.responseReady, so we can't race with them.
	defer close(cs.responseReady)

	md, ok := CallMetadata(cs.ctx)
	if !ok {
		cs.setResponseError(errorf(CodeInternal, "no call metadata available on context"))
		close(prepared)
		return
	}

	if deadline, ok := cs.ctx.Deadline(); ok {
		untilDeadline := time.Until(deadline)
		if enc, err := encodeTimeout(untilDeadline); err == nil {
			// Tests verify that the error in encodeTimeout is unreachable, so we
			// should be safe without observability for the error case.
			md.req.raw["Grpc-Timeout"] = []string{enc}
		}
	}

	req, err := http.NewRequestWithContext(cs.ctx, http.MethodPost, cs.url, cs.reader)
	if err != nil {
		cs.setResponseError(errorf(CodeUnknown, "construct *http.Request: %w", err))
		close(prepared)
		return
	}
	req.Header = md.req.raw

	// Before we send off a request, check if we're already out of time.
	if err := cs.ctx.Err(); err != nil {
		code := CodeUnknown
		if errors.Is(err, context.Canceled) {
			code = CodeCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = CodeDeadlineExceeded
		}
		cs.setResponseError(wrap(code, err))
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
		cs.setResponseError(wrap(code, err))
		return
	}
	*md.res = NewHeader(res.Header)

	if res.StatusCode != http.StatusOK {
		code := CodeUnknown
		if c, ok := httpToGRPC[res.StatusCode]; ok {
			code = c
		}
		cs.setResponseError(errorf(code, "HTTP status %v", res.Status))
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
		cs.setResponseError(errorf(
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
	cs.unmarshaler = unmarshaler{r: res.Body, ctype: TypeDefaultGRPC, max: cs.maxReadBytes}
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
	ctx         context.Context
	unmarshaler unmarshaler
	marshaler   marshaler
	writer      http.ResponseWriter
	reader      io.ReadCloser
	ctype       string
}

var _ Stream = (*serverStream)(nil)

// Thankfully, the server stream is much simpler than the client. net/http
// gives us the request body and response writer at the same time, so we don't
// need to worry about concurrency.
func newServerStream(
	ctx context.Context,
	w http.ResponseWriter,
	r io.ReadCloser,
	ctype string,
	maxReadBytes int64,
	gzipResponse bool,
) *serverStream {
	return &serverStream{
		ctx:         ctx,
		unmarshaler: unmarshaler{r: r, ctype: ctype, max: maxReadBytes},
		marshaler:   marshaler{w: w, ctype: ctype, gzipGRPC: gzipResponse},
		writer:      w,
		reader:      r,
		ctype:       ctype,
	}
}

func (ss *serverStream) Context() context.Context {
	return ss.ctx
}

func (ss *serverStream) Receive(msg proto.Message) error {
	if err := ss.unmarshaler.Unmarshal(msg); err != nil {
		return err // already coded
	}
	// don't return typed nils
	return nil
}

func (ss *serverStream) CloseReceive() error {
	discard(ss.reader)
	if err := ss.reader.Close(); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (ss *serverStream) Send(msg proto.Message) error {
	defer ss.flush()
	if err := ss.marshaler.Marshal(msg); err != nil {
		return err
	}
	// don't return typed nils
	return nil
}

func (ss *serverStream) CloseSend(err error) error {
	defer ss.flush()
	switch ss.ctype {
	case TypeJSON, TypeProtoTwirp:
		return ss.sendErrorTwirp(err)
	case TypeDefaultGRPC, TypeProtoGRPC:
		return ss.sendErrorGRPC(err)
	default:
		return errorf(CodeInvalidArgument, "unsupported Content-Type %q", ss.ctype)
	}
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
		return errorf(CodeInternal, "couldn't marshal protobuf status: %w", err)
	}
	ss.writer.Header().Set("Grpc-Status", code)
	ss.writer.Header().Set("Grpc-Message", percentEncode(s.Message))
	ss.writer.Header().Set("Grpc-Status-Details-Bin", encodeBinaryHeader(bin))
	return nil
}

func (ss *serverStream) sendErrorTwirp(err error) error {
	if err == nil {
		return nil
	}
	gs := statusFromError(err)
	s := &twirp.Status{
		Code:    Code(gs.Code).twirp(),
		Message: gs.Message,
	}
	if te, ok := asTwirpError(err); ok {
		s.Code = te.TwirpCode()
	}
	// Even if the caller sends TypeProtoTwirp, we respond with TypeJSON on
	// errors.
	ss.writer.Header().Set("Content-Type", TypeJSON)
	bs, merr := json.Marshal(s)
	if merr != nil {
		ss.writer.WriteHeader(http.StatusInternalServerError)
		// codes don't need to be escaped in JSON, so this is okay
		const tmpl = `{"code": "%s", "msg": "error marshaling error with code %s"}`
		// Ignore this error. We're well past the point of no return here.
		_, _ = fmt.Fprintf(ss.writer, tmpl, CodeInternal.twirp(), s.Code)
		return errorf(CodeInternal, "couldn't marshal Twirp status to JSON: %w", merr)
	}
	ss.writer.WriteHeader(CodeOf(err).http())
	if _, err = ss.writer.Write(bs); err != nil {
		return wrap(CodeUnknown, err)
	}
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
	codeIsSuccess := (codeHeader == "" || codeHeader == "0")
	if codeIsSuccess {
		return nil
	}

	code, err := strconv.ParseUint(codeHeader, 10 /* base */, 32 /* bitsize */)
	if err != nil {
		return errorf(CodeUnknown, "gRPC protocol error: got invalid error code %q", codeHeader)
	}
	message := percentDecode(h.Get("Grpc-Message"))
	ret := wrap(Code(code), errors.New(message))

	detailsBinaryEncoded := h.Get("Grpc-Status-Details-Bin")
	if len(detailsBinaryEncoded) > 0 {
		detailsBinary, err := decodeBinaryHeader(detailsBinaryEncoded)
		if err != nil {
			return errorf(CodeUnknown, "server returned invalid grpc-error-details-bin trailer: %w", err)
		}
		var status statuspb.Status
		if err := proto.Unmarshal(detailsBinary, &status); err != nil {
			return errorf(CodeUnknown, "server returned invalid protobuf for error details: %w", err)
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
