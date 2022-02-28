connect
=======

[![Build](https://github.com/bufbuild/connect/actions/workflows/test.yml/badge.svg?event=push)](https://github.com/bufbuild/connect/actions/workflows/test.yml)
[![Report Card](https://goreportcard.com/badge/github.com/bufbuild/connect)](https://goreportcard.com/report/github.com/bufbuild/connect)
[![GoDoc](https://pkg.go.dev/badge/github.com/bufbuild/connect.svg)](https://pkg.go.dev/github.com/bufbuild/connect)

Connect is a small framework for building HTTP APIs. You write a short API
definition file and implement your application logic, and connect generates
code to handle marshaling, routing, error handling, and content-type
negotiation. It also generates an idiomatic, type-safe client.

Connect is wire-compatible with the [gRPC][grpc] protocol, including streaming.
Connect servers interoperate seamlessly with generated clients in [more than a
dozen languages][grpc-implementations], command-line tools like [grpcurl][],
and proxies like [Envoy][envoy] and [gRPC-Gateway][grpc-gateway].

Under the hood, connect is just [protocol buffers][protobuf] and the standard
library: no custom HTTP implementation, no new name resolution or load
balancing APIs, and no surprises. Everything you already know about `net/http`
still applies, and any package that works with an `http.Server`, `http.Client`,
or `http.Handler` also works with connect.

For more on connect, including a walkthrough and a comparison to alternatives,
see the [docs][].

## A Small Example

Curious what all this looks like in practice? Here's a small h2c server:

```go
package main

import (
  "log"
  "net/http"

  "github.com/bufbuild/connect"
  pingpb "github.com/bufbuild/connect/internal/gen/proto/go/connect/ping/v1test"
  pingrpc "github.com/bufbuild/connect/internal/gen/proto/connect/connect/ping/v1test"
  "golang.org/x/net/http2"
  "golang.org/x/net/http2/h2c"
)

type PingServer struct {
  pingrpc.UnimplementedPingServiceHandler // returns errors from all methods
}

func (ps *PingServer) Ping(
  ctx context.Context,
  req *connect.Envelope[pingpb.PingRequest]) (*connect.Envelope[pingpb.PingResponse], error) {
  // connect.Envelope gives you direct access to headers and trailers.
  // No context-based nonsense!
  log.Println(req.Header().Get("Some-Header"))
  res := connect.NewEnvelope(&pingpb.PingResponse{
    // req.Msg is a strongly-typed *pingpb.PingRequest, so
    // we can access its fields without type assertions.
    Number: req.Msg.Number,
  })
  res.Header().Set("Some-Other-Header", "hello!")
  res.Trailer().Set("Some-Trailer", "goodbye!")
  return res, nil
}

func main() {
  mux := http.NewServeMux()
  // The generated constructors return a path and a plain net/http
  // handler.
  mux.Handle(pingpb.NewPingServiceHandler(&PingServer{}))
  http.ListenAndServe(
    ":8081",
    h2c.NewHandler(mux, &http2.Server{}),
  )
}
```

With that server running, you can make requests with any gRPC client.

You can find production-ready examples of [servers][prod-server] and
[clients][prod-client] in the API documentation.

## Status

This is the earliest of early alphas: APIs *will* break before the first stable
release.

## Support and Versioning

Connect supports:

* The [two most recent major releases][go-support-policy] of Go, with a minimum
  of Go 1.18.
* Version 3 of the protocol buffer language ([proto3][]).
* [APIv2][] of protocol buffers in Go (`google.golang.org/protobuf`).

Within those parameters, connect follows semantic versioning.

## Legal

Offered under the [MIT license][license].

[APIv2]: https://blog.golang.org/protobuf-apiv2
[docs]: https://bufconnect.com
[envoy]: https://www.envoyproxy.io/
[godoc]: https://pkg.go.dev/github.com/bufbuild/connect
[go-support-policy]: https://golang.org/doc/devel/release#policy
[grpc-gateway]: https://grpc-ecosystem.github.io/grpc-gateway/
[grpc]: https://grpc.io/
[grpc-implementations]: https://grpc.io/docs/languages/
[grpcurl]: https://github.com/fullstorydev/grpcurl
[license]: https://github.com/bufbuild/connect/blob/main/LICENSE.txt
[prod-client]: https://pkg.go.dev/github.com/bufbuild/connect#example-Client
[prod-server]: https://pkg.go.dev/github.com/bufbuild/connect#example-package
[proto3]: https://cloud.google.com/apis/design/proto3
[protobuf]: https://developers.google.com/protocol-buffers
