package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bufconnect/connect/codec"
	"github.com/bufconnect/connect/compress"
	statuspb "github.com/bufconnect/connect/internal/gen/proto/go/grpc/status/v1"
)

// See clientStream below: the send and receive sides of client streams are
// tightly interconnected, so it's simpler to implement the Sender interface
// as a facade over a full-duplex stream.
type clientSender struct {
	stream *clientStream
}

var _ Sender = (*clientSender)(nil)

func (cs *clientSender) Send(m any) error      { return cs.stream.Send(m) }
func (cs *clientSender) Close(err error) error { return cs.stream.CloseSend(err) }
func (cs *clientSender) Spec() Specification   { return cs.stream.Spec() }
func (cs *clientSender) Header() http.Header   { return cs.stream.Header() }

// See clientStream below: the send and receive sides of client streams are
// tightly interconnected, so it's simpler to implement the Receiver interface
// as a facade over a full-duplex stream.
type clientReceiver struct {
	stream *clientStream
}

var _ Receiver = (*clientReceiver)(nil)

func (cr *clientReceiver) Receive(m any) error { return cr.stream.Receive(m) }
func (cr *clientReceiver) Close() error        { return cr.stream.CloseReceive() }
func (cr *clientReceiver) Spec() Specification { return cr.stream.Spec() }
func (cr *clientReceiver) Header() http.Header { return cr.stream.ResponseHeader() }

// clientStream represents a bidirectional exchange of protobuf messages
// between the client and server. The request body is the stream from client to
// server, and the response body is the reverse.
//
// The way we do this with net/http is very different from the typical HTTP/1.1
// request/response code. Since this is the most complex code in connect, it has
// many more comments than usual.
type clientStream struct {
	ctx          context.Context
	doer         Doer
	url          string
	spec         Specification
	maxReadBytes int64
	codec        codec.Codec
	protobuf     codec.Codec // for errors

	// send
	prepareOnce sync.Once
	writer      *io.PipeWriter
	marshaler   marshaler
	header      http.Header

	// receive goroutine
	web           bool
	reader        *io.PipeReader
	response      *http.Response
	responseReady chan struct{}
	unmarshaler   unmarshaler
	compressors   readOnlyCompressors

	responseErrMu sync.Mutex
	responseErr   error
}

func (cs *clientStream) Spec() Specification {
	return cs.spec
}

func (cs *clientStream) Header() http.Header {
	return cs.header
}

func (cs *clientStream) Send(msg any) error {
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
	// Because connect also supports some RPC types over HTTP/1.1, we need to be
	// careful how we expose this method to users. HTTP/1.1 doesn't support
	// bidirectional streaming - the send stream (aka request body) must be
	// closed before we start waiting on the response or we'll just block
	// forever. To make sure users don't have to worry about this, the generated
	// code for unary, client streaming, and server streaming RPCs must call
	// CloseSend automatically rather than requiring the user to do it.
	if err := cs.writer.Close(); err != nil {
		if cerr, ok := AsError(err); ok {
			return cerr
		}
		return Wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) Receive(msg any) error {
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
		trailer := cs.response.Trailer
		if errors.Is(err, errGotWebTrailers) {
			trailer = cs.unmarshaler.WebTrailer()
		}
		if serverErr := extractError(cs.protobuf, trailer); serverErr != nil {
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

func (cs *clientStream) ResponseHeader() http.Header {
	<-cs.responseReady
	return cs.response.Header
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
			cs.header["Grpc-Timeout"] = []string{enc}
		}
	}

	req, err := http.NewRequestWithContext(cs.ctx, http.MethodPost, cs.url, cs.reader)
	if err != nil {
		cs.setResponseError(Errorf(CodeUnknown, "construct *http.Request: %w", err))
		close(prepared)
		return
	}
	req.Header = cs.header

	// Before we send off a request, check if we're already out of time.
	if err := cs.ctx.Err(); err != nil {
		cs.setResponseError(err)
		close(prepared)
		return
	}

	// At this point, we've caught all the errors we can - it's time to send data
	// to the server. Unblock Send.
	close(prepared)
	// Once we send a message to the server, they send a message back and
	// establish the receive side of the stream.
	res, err := cs.doer.Do(req)
	if err != nil {
		cs.setResponseError(err)
		return
	}

	if res.StatusCode != http.StatusOK {
		code := CodeUnknown
		if c, ok := httpToCode[res.StatusCode]; ok {
			code = c
		}
		cs.setResponseError(Errorf(code, "HTTP status %v", res.Status))
		return
	}
	compression := res.Header.Get("Grpc-Encoding")
	if compression == "" || compression == compress.NameIdentity {
		compression = compress.NameIdentity
	} else if !cs.compressors.Contains(compression) {
		// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
		// should return CodeInternal and specify acceptable compression(s) (in
		// addition to setting the Grpc-Accept-Encoding header).
		cs.setResponseError(Errorf(
			CodeInternal,
			"unknown compression %q: accepted grpc-encoding values are %v",
			compression,
			cs.compressors.CommaSeparatedNames(),
		))
		return
	}
	// When there's no body, errors sent from the first-party gRPC servers will
	// be in the headers.
	if err := extractError(cs.protobuf, res.Header); err != nil {
		cs.setResponseError(err)
		return
	}
	// Success! We got a response with valid headers and no error, so there's
	// probably a message waiting in the stream.
	cs.response = res
	cs.unmarshaler = unmarshaler{
		r:          res.Body,
		max:        cs.maxReadBytes,
		codec:      cs.codec,
		compressor: cs.compressors.Get(compression),
		web:        cs.web,
	}
}

func (cs *clientStream) setResponseError(err error) {
	// Normally, we can rely on our built-in middleware to add codes to
	// context-related errors. However, errors set here are exposed on the
	// io.Writer end of the pipe, where they'll likely be miscoded if they're not
	// already wrapped.
	err = wrapIfContextError(err)
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

// The gRPC wire protocol specifies that errors should be serialized using the
// binary protobuf format, even if the messages in the request/response stream
// use a different codec. Consequently, this function needs a protobuf codec to
// unmarshal error information in the headers.
func extractError(protobuf codec.Codec, h http.Header) *Error {
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
		detailsBinary, err := DecodeBinaryHeader(detailsBinaryEncoded)
		if err != nil {
			return Errorf(CodeUnknown, "server returned invalid grpc-status-details-bin trailer: %w", err)
		}
		var status statuspb.Status
		if err := protobuf.Unmarshal(detailsBinary, &status); err != nil {
			return Errorf(CodeUnknown, "server returned invalid protobuf for error details: %w", err)
		}
		for _, d := range status.Details {
			ret.details = append(ret.details, d)
		}
		// Prefer the protobuf-encoded data to the headers (grpc-go does this too).
		ret.code = Code(status.Code)
		ret.err = errors.New(status.Message)
	}

	return ret
}
