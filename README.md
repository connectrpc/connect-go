# Connect

[![Build](https://github.com/bufbuild/connect-go/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/bufbuild/connect-go/actions/workflows/ci.yaml) [![Report Card](https://goreportcard.com/badge/github.com/bufbuild/connect-go)](https://goreportcard.com/report/github.com/bufbuild/connect-go) [![GoDoc](https://pkg.go.dev/badge/github.com/bufbuild/connect-go.svg)](https://pkg.go.dev/github.com/bufbuild/connect-go)

Connect is a slim library for building browser and gRPC-compatible HTTP APIs. You write a short [Protocol Buffer](https://developers.google.com/protocol-buffers) schema and implement your application logic, and Connect generates code to handle marshaling, routing, compression, and content type negotiation. It also generates an idiomatic, type-safe client. Handlers and clients support three protocols: gRPC, gRPC-Web, and Connect's own protocol.

The [Connect protocol](https://connect.build/docs/protocol) is a simple, POST-only protocol that works over HTTP/1.1 or HTTP/2. It takes the best portions of gRPC and gRPC-Web, including streaming, and packages them into a protocol that works equally well in browsers, monoliths, and microservices. Calling to Connect API is as easy as using `curl`. Try it with our live demo:

```
curl \
    --header "Content-Type: application/json" \
    --data '{"sentence": "I feel happy."}' \
    https://demo.connect.build/buf.connect.demo.eliza.v1.ElizaService/Say
```

Handlers and clients also support the gRPC and gRPC-Web protocols, including streaming, headers, trailers, and error details. gRPC-compatible [server reflection](https://github.com/bufbuild/connect-grpcreflect-go) and [health checks](https://github.com/bufbuild/connect-grpchealth-go) are available as standalone packages. Instead of cURL, we could call our API with `grpcurl`:

```
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
grpcurl \
    -d '{"sentence": "I feel happy."}' \
    demo.connect.build:443 \
    buf.connect.demo.eliza.v1.ElizaService/Say
```

Under the hood, Connect is just [Protocol Buffers](https://developers.google.com/protocol-buffers) and the standard library: no custom HTTP implementation, no new name resolution or load balancing APIs, and no surprises. Everything you already know about `net/http` still applies, and any package that works with an `http.Server`, `http.Client`, or `http.Handler` also works with Connect.

For more on Connect, see the [announcement blog post](https://buf.build/blog/connect-a-better-grpc), the documentation on [connect.build](https://connect.build) (especially the [Getting Started](https://connect.build/docs/go/getting-started) guide for Go), the [demo service](https://github.com/bufbuild/connect-demo), or the [protocol specification](https://connect.build/docs/protocol).

## A small example

Curious what all this looks like in practice? From a [Protobuf schema](internal/proto/connect/ping/v1/ping.proto), we generate [a small RPC package](internal/gen/connect/ping/v1/pingv1connect/ping.connect.go). Using that package, we can build a [server](examples/ping/server/server.go):

```go mdox-exec="sed -n '15,69p' examples/ping/server/server.go"
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type PingServer struct {
	// Returns errors from all methods we don't want to implement for now.
	pingv1connect.UnimplementedPingServiceHandler
}

func (ps *PingServer) Ping(
	_ context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {

	// Finally `connect.Request` behaves like standard HTTP library.
	// For example, it gives you direct access to headers and trailers.
	log.Println("Request header:", req.Header().Get("Some-Header"))

	// The request for this method is a strongly-typed as defined by *pingv1.PingRequest,
	// so we can access for example `Msg` field safely!
	res := connect.NewResponse(&pingv1.PingResponse{Number: req.Msg.Number})

	// Similarly we can add extra headers to response.
	res.Header().Set("Some-Other-Header", "hello!")

	// Just return response or error like you would do in non-remote Go method!
	return res, nil
}

func main() {
	// Standard mux for handlers.
	mux := http.NewServeMux()

	// Register handlers for gRPC connect ping service we implemented above.
	mux.Handle(pingv1connect.NewPingServiceHandler(&PingServer{}))

	log.Println("Starting server that accepts both Connect enabled HTTP Post, gRPC and gRPC-Web!")
	if err := http.ListenAndServe(
		"localhost:8080",
		// For the example gRPC clients, it's convenient to support HTTP/2 without TLS.
		h2c.NewHandler(mux, &http2.Server{}),
	); err != nil {
		log.Fatal(err)
	}
}
```

With that server running, you can make requests with any HTTP (post), gRPC or Connect client. For example, we could use `curl`:

```bash
curl --header "Content-Type: application/json" --data '{"number": 5}' http://localhost:8080/connect.ping.v1.PingService/Ping
```

We can also craft a type-safe HTTP client in Go. See [client](examples/ping/http/client.go) using `connect-go` below:

```go mdox-exec="sed -n '15,62p' examples/ping/http/client.go"
package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
	"golang.org/x/net/http2"
)

func newInsecureHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true, // Permits HTTP/2 requests using the insecure.
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}
}

func main() {
	// Create client.
	client := pingv1connect.NewPingServiceClient(
		newInsecureHTTPClient(),
		"https://localhost:8080/",
	)

	// Create request with type-safe payload and extra header.
	req := connect.NewRequest(&pingv1.PingRequest{Number: 42})
	req.Header().Set("Some-Header", "Hello from connect! 💪")

	// Call remote gRPC server.
	res, err := client.Ping(context.Background(), req)
	if err != nil {
		log.Fatalln(err)
	}

	// Enjoy the type-safe response!
	log.Println(res.Msg)
	log.Println("Response header:", res.Header().Get("Some-Other-Header"))
}
```

To force client to use gRPC instead of HTTP it's as easy as adding `connect.WithGRPC`. See a snippet from the [client](examples/ping/grpc/client.go):

```go mdox-exec="sed -n '42,47p' examples/ping/grpc/client.go"
	// Create client.
	client := pingv1connect.NewPingServiceClient(
		newInsecureHTTPClient(),
		"https://localhost:8080/",
		connect.WithGRPC(), // Force using gRPC instead of HTTP (post).
	)
```

Of course, as with standard HTTP server, default configurations of `http.ListenAndServe` and `http.DefaultClient` are not what you should run on production. See Connect's recommended options in [deployment docs](https://connect.build/docs/go/deployment). It guides you through suggested timeouts, connection pools, observability, and h2c.

## Ecosystem

* [connect-grpchealth-go](https://github.com/bufbuild/connect-grpchealth-go): gRPC-compatible health checks
* [connect-grpcreflect-go](https://github.com/bufbuild/connect-grpcreflect-go): gRPC-compatible server reflection
* [connect-demo](https://github.com/bufbuild/connect-demo): demonstration service powering demo.connect.build, including bidi streaming
* [connect-crosstest](https://github.com/bufbuild/connect-crosstest): gRPC and gRPC-Web interoperability tests

## Status

This module is a beta: we rely on it in production, but we may make a few changes as we gather feedback from early adopters. We're planning to tag a stable v1 in October, soon after the Go 1.19 release.

## Support and versioning

`connect-go` supports:

* The [two most recent major releases](https://golang.org/doc/devel/release#policy) of Go, with a minimum of Go 1.18.
* [APIv2](https://blog.golang.org/protobuf-apiv2) of Protocol Buffers in Go (`google.golang.org/protobuf`).

Within those parameters, Connect follows semantic versioning.

## Legal

Offered under the [Apache 2 license](https://github.com/bufbuild/connect-go/blob/main/LICENSE).
