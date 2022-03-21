connect
=======

[![Build](https://github.com/bufbuild/connect/actions/workflows/ci.yml/badge.svg?event=push?branch=main)](https://github.com/bufbuild/connect/actions/workflows/ci.yml)
[![Report Card](https://goreportcard.com/badge/github.com/bufbuild/connect)](https://goreportcard.com/report/github.com/bufbuild/connect)
[![GoDoc](https://pkg.go.dev/badge/github.com/bufbuild/connect.svg)](https://pkg.go.dev/github.com/bufbuild/connect)

Connect is a small framework for building HTTP APIs. You write a short API
definition file and implement your application logic, and connect generates
code to handle marshaling, routing, error handling, and content-type
negotiation. It also generates an idiomatic, type-safe client.

Connect is wire-compatible with the [gRPC][grpc] protocol, including streaming.
Connect servers interoperate seamlessly with generated clients in [more than a
dozen languages][grpc-implementations], command-line tools like [grpcurl][],
and proxies like [Envoy][envoy] and [gRPC-Gateway][grpc-gateway]. They also
support gRPC-Web natively, so they can serve browser traffic without a
translating proxy. Connect clients work with any gRPC or gRPC-Web server.

Under the hood, connect is just [protocol buffers][protobuf] and the standard
library: no custom HTTP implementation, no new name resolution or load
balancing APIs, and no surprises. Everything you already know about `net/http`
still applies, and any package that works with an `http.Server`, `http.Client`,
or `http.Handler` also works with connect.

For more on connect, including a walkthrough and a comparison to alternatives,
see the [docs][].

## A Small Example

Curious what all this looks like in practice? From a [protobuf
schema](internal/proto/connect/ping/v1/ping.proto), we generate [a small RPC
package](internal/gen/connect/connect/ping/v1/pingv1connect/ping.connect.go). Using that
package, we can build a server:

```go
package main

import (
  "log"
  "net/http"

  "github.com/bufbuild/connect"
  "github.com/bufbuild/connect/internal/gen/connect/connect/ping/v1/pingv1connect"
  pingv1 "github.com/bufbuild/connect/internal/gen/go/connect/ping/v1"
)

type PingServer struct {
  pingv1connect.UnimplementedPingServiceHandler // returns errors from all methods
}

func (ps *PingServer) Ping(
  ctx context.Context,
  req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
  // connect.Request and connect.Response give you direct access to headers and
  // trailers. No context-based nonsense!
  log.Println(req.Header().Get("Some-Header"))
  res := connect.NewResponse(&pingv1.PingResponse{
    // req.Msg is a strongly-typed *pingv1.PingRequest, so we can access its
    // fields without type assertions.
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
  mux.Handle(pingv1.NewPingServiceHandler(&PingServer{}))
  http.ListenAndServeTLS(":8081", "server.crt", "server.key", mux)
}
```

With that server running, you can make requests with any gRPC client. Using
connect,

```go
package main

import (
  "log"
  "net/http"

  "github.com/bufbuild/connect"
  "github.com/bufbuild/connect/internal/gen/connect/connect/ping/v1/pingv1connect"
  pingv1 "github.com/bufbuild/connect/internal/gen/go/connect/ping/v1"
)

func main() {
  client, err := pingv1connect.NewPingServiceClient(
    http.DefaultClient,
    "https://localhost:8081/",
  )
  if err != nil {
    log.Fatalln(err)
  }
  req := connect.NewRequest(&pingv1.PingRequest{
    Number: 42,
  })
  req.Header().Set("Some-Header", "hello from connect")
  res, err := client.Ping(context.Background(), req)
  if err != nil {
    log.Fatalln(err)
  }
  log.Println(res.Msg)
  log.Println(res.Header().Get("Some-Other-Header"))
  log.Println(res.Trailer().Get("Some-Trailer"))
}
```

You can find production-ready examples of [servers][prod-server] and
[clients][prod-client] in the API documentation.

## Status

Connect is in _beta_: we use it internally, but expect the Go community to
discover new patterns for working with generics in the coming months. We plan
to tag a release candidate in July 2022 and stable v1 soon after the Go 1.19
release.

## Support and Versioning

Connect supports:

* The [two most recent major releases][go-support-policy] of Go, with a minimum
  of Go 1.18.
* [APIv2][] of protocol buffers in Go (`google.golang.org/protobuf`).

Within those parameters, connect follows semantic versioning. We make no
exceptions for deprecated or experimental APIs.

## Legal

Offered under the [Apache 2 license][license].

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
