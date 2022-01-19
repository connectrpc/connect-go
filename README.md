reRPC
=====

[![Build](https://github.com/rerpc/rerpc/actions/workflows/test.yml/badge.svg?event=push)](https://github.com/rerpc/rerpc/actions/workflows/test.yml)
[![Report Card](https://goreportcard.com/badge/github.com/rerpc/rerpc)](https://goreportcard.com/report/github.com/rerpc/rerpc)
[![GoDoc](https://pkg.go.dev/badge/github.com/rerpc/rerpc.svg)](https://pkg.go.dev/github.com/rerpc/rerpc)

reRPC is a small framework for building HTTP APIs. You write a short API
definition file and implement your application logic, and reRPC generates code
to handle marshaling, routing, error handling, and content-type negotiation. It
also generates an idiomatic, type-safe client.

reRPC is wire-compatible with the [gRPC][grpc] protocol, including streaming.
reRPC servers interoperate seamlessly with generated clients in [more than a
dozen languages][grpc-implementations], command-line tools like [grpcurl][],
and proxies like [Envoy][envoy] and [gRPC-Gateway][grpc-gateway].

Under the hood, reRPC is just [protocol buffers][protobuf] and the standard
library: no custom HTTP implementation, no new name resolution or load
balancing APIs, and no surprises. Everything you already know about `net/http`
still applies, and any package that works with an `http.Server`, `http.Client`,
or `http.Handler` also works with reRPC.

For more on reRPC, including a walkthrough and a comparison to alternatives,
see the [docs][].

## A Small Example

Curious what all this looks like in practice? Here's a small h2c server:

```go
package main

import (
  "net/http"

  "golang.org/x/net/http2"
  "golang.org/x/net/http2/h2c"

  pingpb "github.com/rerpc/rerpc/internal/ping/v1test" // generated
)

type PingServer struct {
  pingpb.UnimplementedPingServiceServer // returns errors from all methods
}

func (ps *PingServer) Ping(ctx context.Context, req *pingpb.PingRequest) (*pingpb.PingResponse, error) {
  return &pingpb.PingResponse{Number: 42}, nil
}

func main() {
  mux, err := rerpc.NewServeMux(
    rerpc.NewNotFoundHandler(),            // fallback
    pingpb.NewPingService(&PingServer{}),  // our logic
  )
  if err != nil {
    panic(err)
  }
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

reRPC supports:

* The [two most recent major releases][go-support-policy] of Go, with a minimum
  of Go 1.18.
* Version 3 of the protocol buffer language ([proto3][]).
* [APIv2][] of protocol buffers in Go (`google.golang.org/protobuf`).

Within those parameters, reRPC follows semantic versioning.

## Legal

Offered under the [MIT license][license].

[APIv2]: https://blog.golang.org/protobuf-apiv2
[docs]: https://rerpc.github.io
[envoy]: https://www.envoyproxy.io/
[godoc]: https://pkg.go.dev/github.com/rerpc/rerpc
[go-support-policy]: https://golang.org/doc/devel/release#policy
[grpc-gateway]: https://grpc-ecosystem.github.io/grpc-gateway/
[grpc]: https://grpc.io/
[grpc-implementations]: https://grpc.io/docs/languages/
[grpcurl]: https://github.com/fullstorydev/grpcurl
[license]: https://github.com/rerpc/rerpc/blob/main/LICENSE.txt
[prod-client]: https://pkg.go.dev/github.com/rerpc/rerpc#example-Client
[prod-server]: https://pkg.go.dev/github.com/rerpc/rerpc#example-package
[proto3]: https://cloud.google.com/apis/design/proto3
[protobuf]: https://developers.google.com/protocol-buffers
[twirp]: https://twitchtv.github.io/twirp/
[twirp-implementations]: https://github.com/twitchtv/twirp#implementations-in-other-languages
