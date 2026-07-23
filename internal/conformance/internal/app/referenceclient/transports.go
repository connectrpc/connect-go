// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package referenceclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2/internal/conformance/internal"
	conformancev1 "connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1"
	"connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1/conformancev1connect"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

type transportSpec struct {
	httpVersion   conformancev1.HTTPVersion
	serverTLSCert string
	clientTLSCert string
	clientTLSKey  string
}

type transports struct {
	cache sync.Map // map[transportSpec]http.RoundTripper
}

func (t *transports) get(req *conformancev1.ClientCompatRequest) (http.RoundTripper, *url.URL, error) {
	tlsConf, err := createTLSConfig(req)
	if err != nil {
		return nil, nil, err
	}
	var scheme string
	if tlsConf != nil {
		scheme = "https://"
	} else {
		scheme = "http://"
	}
	urlString := scheme + net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))
	serverURL, err := url.ParseRequestURI(urlString)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid url: %s", urlString)
	}

	spec := transportSpec{
		httpVersion:   req.GetHttpVersion(),
		serverTLSCert: string(req.GetServerTlsCert()),
		clientTLSCert: string(req.GetClientTlsCreds().GetCert()),
		clientTLSKey:  string(req.GetClientTlsCreds().GetKey()),
	}

	// Optimistically skip logic if it's already cached. We will still do an
	// atomic store to share the transport in all cases even if this misses.
	if tr, ok := t.cache.Load(spec); ok {
		return tr.(http.RoundTripper), serverURL, nil //nolint:errcheck,forcetypeassert
	}

	var transport http.RoundTripper
	switch req.HttpVersion {
	case conformancev1.HTTPVersion_HTTP_VERSION_1:
		if tlsConf != nil {
			tlsConf.NextProtos = []string{"http/1.1"}
		}
		tx := &http.Transport{
			DisableCompression: true,
			TLSClientConfig:    tlsConf,
		}
		transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := tx.RoundTrip(req)
			if resp != nil &&
				strings.HasSuffix(req.URL.Path, conformancev1connect.ConformanceServiceBidiStreamProcedure) {
				// To force support for bidirectional RPC over HTTP 1.1 (for half-duplex testing),
				// we "trick" the client into thinking this is HTTP/2. We have to do this because
				// otherwise, connect-go refuses to support bidi streams over HTTP 1.1.
				resp.ProtoMajor, resp.ProtoMinor = 2, 0
			}
			return resp, err
		})
	case conformancev1.HTTPVersion_HTTP_VERSION_2:
		var prots *http.Protocols
		forceAttemptHTTP2 := false
		if tlsConf != nil {
			tlsConf.NextProtos = []string{"h2"}
			forceAttemptHTTP2 = true
		} else {
			prots = &http.Protocols{}
			prots.SetUnencryptedHTTP2(true)
		}
		transport = &http.Transport{
			DisableCompression: true,
			TLSClientConfig:    tlsConf,
			ForceAttemptHTTP2:  forceAttemptHTTP2,
			Protocols:          prots,
		}
	case conformancev1.HTTPVersion_HTTP_VERSION_3:
		if tlsConf == nil {
			return nil, nil, errors.New("HTTP/3 indicated in request but no TLS info provided")
		}
		transport = &contextFixTransport{http3.Transport{
			DisableCompression: true,
			TLSClientConfig:    tlsConf,
			QUICConfig:         &quic.Config{MaxIdleTimeout: 20 * time.Second, KeepAlivePeriod: 5 * time.Second},
		}}
	case conformancev1.HTTPVersion_HTTP_VERSION_UNSPECIFIED:
		return nil, nil, errors.New("an HTTP version must be specified")
	default:
		return nil, nil, fmt.Errorf("unknown HTTP version specified :%d", req.HttpVersion)
	}

	// Even if two requests for the same spec make it here, they will use the same connection.
	actual, _ := t.cache.LoadOrStore(spec, transport)
	return actual.(http.RoundTripper), serverURL, nil //nolint:errcheck,forcetypeassert
}

// contextFixTransport wraps an HTTP/3 transport so that context errors can be correctly
// classified by the connect-go framework. This is a work-around until a fix
// can be implemented in connect-go and/or quic-go.
// See: https://github.com/quic-go/quic-go/issues/4196
type contextFixTransport struct {
	http3.Transport
}

func (t *contextFixTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	resp, err := t.Transport.RoundTrip(req)
	if err != nil {
		return nil, maybeWrapContextError(ctx, err)
	}
	resp.Body = &contextFixReader{ctx: ctx, r: resp.Body}
	return resp, nil
}

type contextFixReader struct {
	ctx context.Context //nolint:containedctx
	r   io.ReadCloser
}

func (r *contextFixReader) Read(data []byte) (int, error) {
	n, err := r.r.Read(data)
	return n, maybeWrapContextError(r.ctx, err)
}

func (r *contextFixReader) Close() error {
	return maybeWrapContextError(r.ctx, r.r.Close())
}

func maybeWrapContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &contextFixError{timeout: true, error: err}
	}
	var httpErr *http3.Error
	if errors.As(err, &httpErr) && httpErr.ErrorCode == http3.ErrCodeRequestCanceled {
		return &contextFixError{timeout: errors.Is(ctxErr, context.DeadlineExceeded), error: err}
	}
	return err
}

type contextFixError struct {
	timeout bool
	error
}

//nolint:goerr113
func (e *contextFixError) Is(err error) bool {
	return (e.timeout && err == context.DeadlineExceeded) ||
		(!e.timeout && err == context.Canceled)
}

func createTLSConfig(req *conformancev1.ClientCompatRequest) (*tls.Config, error) {
	if req.ServerTlsCert == nil {
		if req.ClientTlsCreds != nil {
			return nil, errors.New("request indicated TLS client credentials but not server TLS cert provided")
		}
		return nil, nil //nolint:nilnil
	}
	return internal.NewClientTLSConfig(req.ServerTlsCert, req.ClientTlsCreds.GetCert(), req.ClientTlsCreds.GetKey())
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
