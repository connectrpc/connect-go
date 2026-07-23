Connect
=======

[![Build](https://github.com/connectrpc/connect-go/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/connectrpc/connect-go/actions/workflows/ci.yaml)
[![GoDoc](https://pkg.go.dev/badge/connectrpc.com/connect.svg)](https://pkg.go.dev/connectrpc.com/connect)
[![Slack](https://img.shields.io/badge/slack-buf-%23e01563)][slack]
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/8972/badge)](https://www.bestpractices.dev/projects/8972)

Connect is a slim library for building browser and gRPC-compatible HTTP APIs.
You write a short [Protocol Buffer][protobuf] schema and implement your
application logic, and Connect generates code to handle marshaling, routing,
compression, and content type negotiation. It also generates an idiomatic,
type-safe client. Handlers and clients support three protocols: gRPC, gRPC-Web,
and Connect's own protocol.

The [Connect protocol][protocol] is a simple protocol that works over HTTP/1.1
or HTTP/2. It takes the best portions of gRPC and gRPC-Web, including
streaming, and packages them into a protocol that works equally well in
browsers, monoliths, and microservices. Calling a Connect API is as easy as
using `curl`. Try it with our live demo:

```
curl \
    --header "Content-Type: application/json" \
    --data '{"sentence": "I feel happy."}' \
    https://demo.connectrpc.com/connectrpc.eliza.v1.ElizaService/Say
```

Handlers and clients also support the gRPC and gRPC-Web protocols, including
streaming, headers, trailers, and error details. gRPC-compatible [server
reflection][grpcreflect] and [health checks][grpchealth] are available as
standalone packages. Instead of cURL, we could call our API with a gRPC client:

```
go install github.com/bufbuild/buf/cmd/buf@latest
buf curl --protocol grpc \
    --data '{"sentence": "I feel happy."}' \
    https://demo.connectrpc.com/connectrpc.eliza.v1.ElizaService/Say
```

Under the hood, Connect is just [Protocol Buffers][protobuf] and the standard
library: no custom HTTP implementation, no new name resolution or load
balancing APIs, and no surprises. Everything you already know about `net/http`
still applies, and any package that works with an `http.Server`, `http.Client`,
or `http.Handler` also works with Connect.

For more on Connect, see the [announcement blog post][blog], the documentation
on [connectrpc.com][docs] (especially the [Getting Started] guide for Go), the
[demo service][examples-go], or the [protocol specification][protocol].

## A small example

Curious what all this looks like in practice? From a [Protobuf
schema](internal/proto/connect/ping/v1/ping.proto), we generate [a small RPC
package](internal/gen/connect/ping/v1/pingv1connect/ping.connect.go). Using that
package, we can build a server. This example is available at [internal/example](internal/example):

```go
package main

import (
  "context"
  "log"
  "net/http"

  "connectrpc.com/connect/v2"
  "connectrpc.com/connect/v2/connecthttp"
  pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
  "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
  "connectrpc.com/validate/v2"
)

type PingServer struct {
  pingv1connect.UnimplementedPingServiceHandler // returns errors from all methods
}

func (ps *PingServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
  return &pingv1.PingResponse{
    Number: req.Number,
  }, nil
}

func main() {
  // Register services on a *connect.Server, then mount it with connecthttp.
  // Interceptors are arguments to NewServer. Validation via Protovalidate is
  // almost always recommended.
  server := connect.NewServer(validate.NewServerInterceptor())
  pingv1connect.RegisterPingServiceHandler(server, &PingServer{})
  mux := http.NewServeMux()
  connecthttp.Mount(mux, server)
  p := new(http.Protocols)
  p.SetHTTP1(true)
  // For gRPC clients, it's convenient to support HTTP/2 without TLS.
  p.SetUnencryptedHTTP2(true)
  s := &http.Server{
    Addr:      "localhost:8080",
    Handler:   mux,
    Protocols: p,
  }
  if err := s.ListenAndServe(); err != nil {
    log.Fatalf("listen failed: %v", err)
  }
}
```

With that server running, you can make requests with any gRPC or Connect
client. To write a client using Connect:

```go
package main

import (
  "context"
  "log"
  "net/http"

  "connectrpc.com/connect/v2"
  "connectrpc.com/connect/v2/connecthttp"
  pingv1 "connectrpc.com/connect/v2/internal/gen/connect/ping/v1"
  "connectrpc.com/connect/v2/internal/gen/connect/ping/v1/pingv1connect"
)

func main() {
  client := connect.NewClient(
    connecthttp.NewTransport(http.DefaultClient, "http://localhost:8080"),
  )
  pingClient := pingv1connect.NewPingServiceClient(client)
  req := &pingv1.PingRequest{Number: 42}
  res, err := pingClient.Ping(context.Background(), req)
  if err != nil {
    log.Fatalln(err)
  }
  log.Println(res)
}
```

Of course, `http.ListenAndServe` and `http.DefaultClient` aren't fit for
production use! See Connect's [deployment docs][docs-deployment] for a guide to
configuring timeouts, connection pools, observability, and h2c.

## Migrating from v1

If you are migrating from v1 to v2, check out our [migration guide](./docs/v2-migration.md).

This project follows semantic versioning. The module `/v2` suffix is part of
the module `connectrpc.com/connect/v2`.

## Ecosystem

* [grpchealth]: gRPC-compatible health checks for connect-go
* [grpcreflect]: gRPC-compatible server reflection for connect-go
* [validate]: [Protovalidate][protovalidate] interceptor for connect-go
* [examples-go]: service powering [demo.connectrpc.com](https://demo.connectrpc.com), including bidi streaming
* [connect-es]: Type-safe APIs with Protobuf and TypeScript
* [Buf Studio]: web UI for ad-hoc RPCs
* [conformance]: Connect, gRPC, and gRPC-Web interoperability tests

## Status

This module, `connectrpc.com/connect/v2`, is in beta.
The `v2` module will be published on the `main` branch of the repository when released.

## Support and versioning

`connect-go` supports:

* The two most recent major releases of Go (the same versions of Go that continue
  to [receive security patches][go-support-policy]).
* [APIv2] of Protocol Buffers in Go (`google.golang.org/protobuf`).

Within those parameters, `connect-go` follows semantic versioning.

Module `connectrpc.com/connect` is the `v1` module. It remains stable and
supported indefinitely. The `v1` module lives on the `v1` branch.
See the [v2 guide](docs/v2-guide.md) for an overview of what changed and why.

## Legal

Offered under the [Apache 2 license][license].

[APIv2]: https://blog.golang.org/protobuf-apiv2
[Buf Studio]: https://buf.build/studio
[Getting Started]: https://connectrpc.com/docs/go/getting-started
[blog]: https://buf.build/blog/connect-a-better-grpc
[conformance]: https://github.com/connectrpc/conformance
[grpchealth]: https://github.com/connectrpc/grpchealth-go
[grpcreflect]: https://github.com/connectrpc/grpcreflect-go
[connect-es]: https://github.com/connectrpc/connect-es
[examples-go]: https://github.com/connectrpc/examples-go
[docs-deployment]: https://connectrpc.com/docs/go/deployment
[docs]: https://connectrpc.com
[go-support-policy]: https://golang.org/doc/devel/release#policy
[license]: https://github.com/connectrpc/connect-go/blob/main/LICENSE
[protobuf]: https://developers.google.com/protocol-buffers
[protocol]: https://connectrpc.com/docs/protocol
[slack]: https://buf.build/links/slack
[validate]: https://github.com/connectrpc/validate-go
[protovalidate]: https://protovalidate.com
