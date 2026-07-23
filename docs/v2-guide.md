# connect-go v2

`connect-go` v2 is a new major version with simpler generated code, a smaller core API, and a transport abstraction in place of a hard dependency on `net/http`. This document explains why v2 exists, what changed from v1, how the runtime fits together, and how to migrate. Read it top to bottom if `connect-go` is new to you, or skip to the [migration](#migration) section and [the migration guide](v2-migration.md) if you are coming from v1.

`connect-go` v1 remains production-ready and supported. It will receive bug fixes and security patches on the v1 branch indefinitely. v2 is a new major version with a new module path, `connectrpc.com/connect/v2`, so the two coexist and you can migrate at your own pace.

## Contents

- [Why a new major version](#why-a-new-major-version)
- [Package layout](#package-layout)
- [A complete example](#a-complete-example)
- [Generated code](#generated-code)
- [The runtime model](#the-runtime-model)
- [Transports](#transports)
- [Streams](#streams)
- [Metadata](#metadata)
- [Interceptors](#interceptors)
- [Errors](#errors)
- [Codecs and compressors](#codecs-and-compressors)
- [Behavioral fixes](#behavioral-fixes)
- [Performance](#performance)
- [Migration](#migration)
- [Ecosystem](#ecosystem)
- [Versioning and support](#versioning-and-support)

## Why a new major version

`connect-go` shipped [just over four years ago](https://buf.build/blog/connect-a-better-grpc) as the first Connect library. Its core bet, a gRPC-compatible RPC framework that also speaks plain HTTP and JSON without dragging in a parallel networking stack, has held up well. However, four years of production use have also surfaced a handful of design decisions that we would make differently today, and that cannot be undone without changing exported types.

A major version is disruptive, and we do not take it lightly. The case for v2 rests on four problems that share one root cause: each fix requires breaking an exported type or signature, so none of them can land in v1 without violating its compatibility promise.

### Generics in the default API

v1 generated code wraps every message in `connect.Request[T]` or `connect.Response[T]`. The intent was type-safe access to headers and trailers without reaching into `context.Context` untyped. In practice most RPCs never touch metadata, so the wrappers add noise to every signature and a value to allocate at every call site ([#451](https://github.com/connectrpc/connect-go/issues/451), [#257](https://github.com/connectrpc/connect-go/issues/257), [#848](https://github.com/connectrpc/connect-go/issues/848), [#851](https://github.com/connectrpc/connect-go/issues/851), [discussion #421](https://github.com/connectrpc/connect-go/discussions/421)). The shape also fights the grain of the wider Go RPC world. gRPC-Go, Twirp, and others settle on roughly `func(context.Context, *Request) (*Response, error)`, which is what most Go engineers expect and what makes migration mechanical.

The wrappers carry a binary-size cost too. v1 instantiates `connect.NewClient[Req, Res]` with concrete message types, so Go's shape-based generic deduplication never kicks in. The compiler emits a separate client and method set for every RPC, plus the runtime metadata each copy needs, so the binary grows with the number of RPCs compiled in. As one example, the [Buf CLI](https://github.com/bufbuild/buf) compiles in roughly 110 RPCs, and moving it to v2 shrinks its stripped binary by roughly 10%.

v1.19.0 added a `simple` generation flag that produces the unwrapped signatures. A flag can only add a second shape, not replace the first, so the generated API is now split in two: the generic default and the simple opt-in. Almost everyone who learns about the simple flag prefers it, and running two generated shapes side by side is its own source of confusion. The clean fix is to make the simple shape the only shape, which means regenerating against a new major version.

### net/http as the core abstraction

v1 defines its public boundary in terms of `net/http`. A generated client dispatches through an `http.Client`, and a service is served as an `http.Handler`. Using the idiomatic HTTP types was a deliberate choice, and for serving over HTTP it works well. The problem is that the HTTP types are the *only* boundary, so anything that is not an HTTP round trip is either impossible or a workaround.

Two consequences motivated the redesign:

- In-process calls are painful. Testing a service through a connect-go client means standing up a loopback HTTP server (typically `httptest.Server`), which is slower and flakier than it should be under load. We hit this in `connect-go`'s own CI as coverage grew. We built an in-memory HTTP client to work around it, but kept it internal because it was a workaround, not a clean boundary, and users have asked us to export it ([#694](https://github.com/connectrpc/connect-go/issues/694), [#740](https://github.com/connectrpc/connect-go/issues/740)).
- Non-HTTP transports have nowhere to plug in. We have wanted to offer WebSocket support, which is not a plain HTTP round trip, and other RPC systems such as [PluginRPC](https://github.com/pluginrpc) run over stdin and stdout with their own code generators. None of these can reuse connect-go generated code while the boundary is `net/http`.

v2 shrinks the core package so that it no longer imports `net/http` and introduces a small [`Transport`](#transports) interface as the boundary. A new package, [`connecthttp`](#transports), implements that interface for the Connect, gRPC, and gRPC-Web protocols over `net/http`. For an HTTP client or server the caller experience barely changes: you add a `connecthttp` import for the transport and keep using `connect` for the client and server constructors. What you gain is room for in-process dispatch, test doubles, and third-party transports without regenerating code or growing the core API.

### Interceptors that cannot observe the whole call

v1's unary interceptor is `WrapUnary(UnaryFunc) UnaryFunc`, where `UnaryFunc` is `func(context.Context, AnyRequest) (AnyResponse, error)`. By definition it receives an already-decoded message, so its position in the pipeline, after decompression and unmarshaling, is fixed by its type. That position makes some ecosystem packages impossible to write correctly:

- `connectrpc.com/authn` cannot be an interceptor. An auth check that runs after unmarshaling lets an unauthenticated client trigger decompression and decode work first, so authn has to be HTTP middleware instead.
- `connectrpc.com/otelconnect` measures unary and streaming RPCs at different points in the pipeline, and on-the-wire message sizes are not exposed at all, so its latency and size metrics are skewed ([#665](https://github.com/connectrpc/connect-go/issues/665)).

Errors raised before interceptors run also bypass error masking, which a security review flagged ([#584](https://github.com/connectrpc/connect-go/issues/584)). This is not fixable in place: moving interceptors earlier changes what `UnaryFunc` receives, and adding a method to the exported `Interceptor` interface breaks every implementation. v2 replaces the interface with two function types that surround the entire call. See [Interceptors](#interceptors).

### A single overloaded package

v1 has accumulated APIs that work around the limits above. It exports roughly 180 constants, types, functions, and methods from one package, and useful internals such as the code-to-HTTP-status mapping cannot be exported without making that package larger still ([#781](https://github.com/connectrpc/connect-go/issues/781)). Stream types are concrete structs the library alone can construct, so handlers and interceptors that touch streams cannot be unit tested without a live connection ([#13](https://github.com/connectrpc/connect-go/issues/13), [#458](https://github.com/connectrpc/connect-go/issues/458), [#719](https://github.com/connectrpc/connect-go/issues/719)). Stream `Send` and `Receive` take no context, so per-call values such as loggers cannot reach them and there is no room to interrupt a blocked operation ([#264](https://github.com/connectrpc/connect-go/issues/264), [#735](https://github.com/connectrpc/connect-go/issues/735), [#823](https://github.com/connectrpc/connect-go/issues/823)).

v2 splits the module into focused packages and makes the stream types interfaces. A new major version lets us make all of these changes once, coherently, and leave v1 untouched for programs that depend on it.

## Package layout

v2 is one module, `connectrpc.com/connect/v2`, split into a transport-agnostic core and a set of focused packages. The core no longer depends on `net/http`.

| Package | Contents |
| --- | --- |
| `connectrpc.com/connect/v2` | Core types imported by generated code: `Client`, `Server`, `Transport`, streams, interceptors, `Spec`, `Method`, `CallInfo`, `Error`, `Code`, codec and compressor interfaces. |
| `connectrpc.com/connect/v2/connecthttp` | Connect, gRPC, and gRPC-Web over `net/http`: `NewTransport`, `Mount`, HTTP options, and HTTP-level call info. |
| `connectrpc.com/connect/v2/connectproto` | Protobuf binary and JSON codecs. |
| `connectrpc.com/connect/v2/connectgzip` | gzip compressor. |
| `connectrpc.com/connect/v2/connectinprocess` | In-process transport that dispatches directly to a `*connect.Server`. |

The entire core lives in a single file, [`connect.go`](https://pkg.go.dev/connectrpc.com/connect/v2), and exports roughly 25 top-level types and functions, down from about 180 identifiers in v1. Most HTTP services need exactly two of these packages: `connect` for the constructors and `connecthttp` for the transport and server binding. `connectproto` and `connectgzip` supply the defaults that `connecthttp` uses, so you rarely import them directly. `connectinprocess` is useful for testing.

## A complete example

The schema is the [`PingService`](../internal/proto/connect/ping/v1/ping.proto), which has two unary methods (`Ping`, `Fail`), a client-streaming method (`Sum`), a server-streaming method (`CountUp`), and a bidi-streaming method (`CumSum`). Generated code lives in [`pingv1connect`](../internal/gen/connect/ping/v1/pingv1connect/ping.connect.go).

The complete, runnable code is in [`internal/example`](../internal/example).

### Server

A service implementation is a plain struct whose methods take and return protobuf messages. Embedding `UnimplementedPingServiceHandler` returns `CodeUnimplemented` from any method you do not define, which keeps the implementation compiling as the schema grows. You register it on a `*connect.Server`, then hand the server to `connecthttp.Mount`, which installs one route per procedure on an `http.ServeMux`. Interceptors are passed to `connect.NewServer`; here a logging interceptor runs before any payload work. The full file at [internal/example/server/main.go](../internal/example/server/main.go) also implements the streaming methods.

```go
type pingServer struct {
	pingv1connect.UnimplementedPingServiceHandler
}

func (pingServer) Ping(ctx context.Context, req *v1.PingRequest) (*v1.PingResponse, error) {
	return &v1.PingResponse{Number: req.Number, Text: req.Text}, nil
}

// serverLoggingInterceptor logs RPCs that fail. Interceptors are passed to
// connect.NewServer and run before any payload work.
func serverLoggingInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err != nil {
			log.Printf("rpc failed: procedure=%s error=%v", spec.Procedure, err)
		}
		return err
	}
}

func main() {
	server := connect.NewServer(serverLoggingInterceptor)
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	// For gRPC clients, it is convenient to support HTTP/2 without TLS.
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Addr:      "localhost:8080",
		Handler:   mux,
		Protocols: protocols,
	}
	log.Println("listening on", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
```

Because `Mount` registers ordinary `http.Handler` values, Connect routes and plain HTTP routes such as `/healthz` share one mux, and standard middleware, timeouts, and h2c setup all apply normally.

### Client

A generated client holds a `*connect.Client`, which wraps a transport. For HTTP, the transport comes from `connecthttp.NewTransport`. Unary methods take and return messages directly, and streaming methods return a generated stream you receive from until `io.EOF`. The client below is wrapped with a logging interceptor; see the full file at [internal/example/client/main.go](../internal/example/client/main.go).

```go
// clientLoggingInterceptor logs each call before the stream is opened.
// Interceptors are passed to connect.NewClient and run in argument order.
func clientLoggingInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		log.Printf("calling %s", spec.Procedure)
		return next(ctx, spec)
	}
}

func main() {
	ctx := context.Background()
	client := connect.NewClient(
		connecthttp.NewTransport(http.DefaultClient, "http://localhost:8080"),
		clientLoggingInterceptor,
	)
	pingClient := pingv1connect.NewPingServiceClient(client)

	res, err := pingClient.Ping(ctx, &v1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		log.Fatalf("Ping: %v", err)
	}
	log.Printf("Ping: number=%d text=%q", res.Number, res.Text)

	stream, err := pingClient.CountUp(ctx, &v1.CountUpRequest{Number: 3})
	if err != nil {
		log.Fatalf("CountUp: %v", err)
	}
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatalf("CountUp.Receive: %v", err)
		}
		log.Printf("CountUp: %d", msg.Number)
	}
}
```

### Test

Because the client is no longer tightly coupled to HTTP, tests can swap the transport for `connectinprocess.New`. This dispatches RPCs directly to the server in the same process, bypassing listeners and serialization entirely. The runnable version is at [internal/example/server/main_test.go](../internal/example/server/main_test.go); run it with `go test ./server`.

```go
func newTestClient(tb testing.TB) pingv1connect.PingServiceClient {
	tb.Helper()
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, pingServer{})
	return pingv1connect.NewPingServiceClient(
		connect.NewClient(connectinprocess.New(server)),
	)
}

func TestPing(t *testing.T) {
	client := newTestClient(t)
	res, err := client.Ping(t.Context(), &v1.PingRequest{Number: 42, Text: "hello"})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.Number != 42 || res.Text != "hello" {
		t.Errorf("got Number=%d Text=%q; want 42 %q", res.Number, res.Text, "hello")
	}
}
```

## Generated code

The clearest way to see v2 is to compare the generated client and handler against v1. The snippets below keep only signatures.

v1 default output, with the generic wrappers:

```go
type PingServiceClient interface {
	Ping(context.Context, *connect.Request[v1.PingRequest]) (*connect.Response[v1.PingResponse], error)
	Sum(context.Context) *connect.ClientStreamForClient[v1.SumRequest, v1.SumResponse]
	CountUp(context.Context, *connect.Request[v1.CountUpRequest]) (*connect.ServerStreamForClient[v1.CountUpResponse], error)
	CumSum(context.Context) *connect.BidiStreamForClient[v1.CumSumRequest, v1.CumSumResponse]
}

func NewPingServiceClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) PingServiceClient

type PingServiceHandler interface {
	Ping(context.Context, *connect.Request[v1.PingRequest]) (*connect.Response[v1.PingResponse], error)
	Sum(context.Context, *connect.ClientStream[v1.SumRequest]) (*connect.Response[v1.SumResponse], error)
	CountUp(context.Context, *connect.Request[v1.CountUpRequest], *connect.ServerStream[v1.CountUpResponse]) error
	CumSum(context.Context, *connect.BidiStream[v1.CumSumRequest, v1.CumSumResponse]) error
}

func NewPingServiceHandler(svc PingServiceHandler, opts ...connect.HandlerOption) (string, http.Handler)
```

v2 output:

```go
type PingServiceClient interface {
	Ping(context.Context, *v1.PingRequest) (*v1.PingResponse, error)
	Sum(context.Context) (PingServiceSumClientStream, error)
	CountUp(context.Context, *v1.CountUpRequest) (PingServiceCountUpClientStream, error)
	CumSum(context.Context) (PingServiceCumSumClientStream, error)
}

func NewPingServiceClient(client *connect.Client) PingServiceClient

type PingServiceHandler interface {
	Ping(context.Context, *v1.PingRequest) (*v1.PingResponse, error)
	Sum(context.Context, PingServiceSumServerStream) (*v1.SumResponse, error)
	CountUp(context.Context, *v1.CountUpRequest, PingServiceCountUpServerStream) error
	CumSum(context.Context, PingServiceCumSumServerStream) error
}

func RegisterPingServiceHandler(server *connect.Server, svc PingServiceHandler)
```

Three things changed:

Unary methods take a context and the request message and return the response message. They match the v1 `simple` output exactly, and they match the shape gRPC-Go and Twirp use.

Streaming methods exchange a named stream type generated per RPC, such as `PingServiceSumServerStream` or `PingServiceCumSumClientStream`, instead of a generic instantiation. Each generated type is a thin wrapper over the [`connect.ClientStream`](#streams) or `connect.ServerStream` interface that exposes only the operations that RPC allows. A server-streaming handler stream exposes `Send` but not `Receive`; a client-streaming handler stream exposes `Receive` but not `Send`. We generate these wrappers rather than export generic stream functions to keep the core package small and to keep each stream's legal operations visible in its type. For example, the generated `Sum` client stream is:

```go
type PingServiceSumClientStream struct {
	stream connect.ClientStream
}

func (s PingServiceSumClientStream) SendHeaders() error { return s.stream.SendHeaders() }

func (s PingServiceSumClientStream) Send(req *v1.SumRequest) error {
	return s.stream.Send(req)
}

func (s PingServiceSumClientStream) CloseAndReceive() (*v1.SumResponse, error) {
	if err := s.stream.CloseSend(); err != nil {
		return nil, err
	}
	var res v1.SumResponse
	if err := s.stream.Receive(&res); err != nil {
		return nil, err
	}
	return &res, nil
}
```

The constructors define the code's boundary. A client is built from a `*connect.Client` and dispatches each method through one of its `Call` methods. A handler is registered on a `*connect.Server` through generated `connect.Method` values. Neither imports `net/http`, so the same generated code runs over any transport.

The generator keeps the binary name `protoc-gen-connect-go`. Install it from the v2 module:

```sh
go install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@latest
```

v2 generated code is always in the simple style, and the generator rejects the v1 `simple` option so a stale flag fails loudly instead of being ignored.

## The runtime model

Three core types carry an RPC: a `Client` opens it, a `Transport` delivers it, and a `Server` dispatches it on the far side. Generated code sits on top of the `Client` and `Server`; the `Transport` sits underneath.

### Client

A [`connect.Client`](https://pkg.go.dev/connectrpc.com/connect/v2#Client) bundles a `Transport` with the client-side interceptor chain. Generated service clients hold one `*connect.Client` and dispatch every RPC through one of its call methods:

```go
func (c *Client) CallUnary(ctx context.Context, spec Spec, req, res any) error
func (c *Client) CallClientStream(ctx context.Context, spec Spec) (ClientStream, error)
func (c *Client) CallServerStream(ctx context.Context, spec Spec, req any) (ClientStream, error)
```

`CallUnary` opens a stream, sends the request, closes the send side, reads one response, and closes the stream before returning so the final protocol state is checked. The streaming calls return a `ClientStream` that the caller owns: reading to `io.EOF` releases its resources, and `Close` abandons a stream early. Multiple generated clients can share one `*connect.Client`.

The interceptor chain is applied once, in `NewClient`, producing one prebuilt function per call shape rather than a closure per RPC. Per-call arguments reach the terminal functions through the context, so dispatch allocates nothing for the chain itself.

```go
func NewClient(transport Transport, interceptors ...ClientInterceptor) *Client
```

### Server

A [`connect.Server`](https://pkg.go.dev/connectrpc.com/connect/v2#Server) maps a procedure path to the function that serves it, and holds the server-side interceptor chain.

```go
func NewServer(interceptors ...ServerInterceptor) *Server
func (s *Server) Register(methods ...Method)
func (s *Server) Specs() iter.Seq[Spec]
func (s *Server) Call(ctx context.Context, procedure string, info *CallInfo, stream ServerStream) error
func (s *Server) SetUnknownHandler(fn ServerFunc)
```

Generated registration functions build one `connect.Method` per RPC and pass them to `Register`, which wraps each handler with the interceptor chain so that `Call` is a map lookup plus one function call. A server transport dispatches an incoming RPC with `Call` and enumerates registered procedures with `Specs` to install per-procedure routes. `Call` returns `CodeUnimplemented` when no method is registered for a procedure; `SetUnknownHandler` replaces that fallback for callers such as proxies. The fallback receives a `Spec` with the requested procedure, `StreamTypeBidi`, and a nil `Schema`.

### Spec and Method

A [`Spec`](https://pkg.go.dev/connectrpc.com/connect/v2#Spec) describes one procedure, and a `Method` binds a `Spec` to its handler:

```go
type Spec struct {
	StreamType       StreamType
	IdempotencyLevel IdempotencyLevel
	Schema           any    // protobuf stores a protoreflect.MethodDescriptor
	Procedure        string // "/package.Service/Method"
}

type Method struct {
	Spec    Spec
	Handler ServerFunc
}
```

`Schema` is `any` on purpose. Protobuf generated code stores a `protoreflect.MethodDescriptor`, but another schema system can store its own descriptor. Because the schema travels with the spec and stream messages are untyped (`any`), you can build a client or handler from a descriptor at runtime with no generated code at all ([#312](https://github.com/connectrpc/connect-go/issues/312), [#523](https://github.com/connectrpc/connect-go/issues/523)).

## Transports

A transport is the delivery mechanism that carries an RPC from a client to a server. In v1 this mechanism was fixed and hidden inside the generated `http.Client` dispatch and `http.Handler` serving. v2 makes it an explicit interface, supplied when you build a `*connect.Client`:

```go
type Transport interface {
	NewClientStream(ctx context.Context, spec Spec) (ClientStream, error)
}
```

The single method opens the client half of a stream for a spec. The context passed in carries the client-side `CallInfo`, which interceptors and stream operations read back through the context. The caller owns the returned stream and closes it when finished. The full contract, including context and ownership rules, is documented on [`Transport`](https://pkg.go.dev/connectrpc.com/connect/v2#Transport).

A client call flows down through the generated client to the `*connect.Client`, which runs the client interceptor chain and hands the spec to the transport. The transport produces a `ClientStream`; on the server side, the matching transport builds a `ServerStream` from its wire input and calls `Server.Call`, which runs the server interceptor chain and the registered handler. The three layers stack with generated code on top, the `connect` core in the middle, and the transport underneath. The boundary between the core and the transport is the `Transport` interface on the client side and `Server.Call` plus `Server.Specs` on the server side.

### connecthttp

[`connecthttp`](https://pkg.go.dev/connectrpc.com/connect/v2/connecthttp) is the default transport. It speaks the Connect, gRPC, and gRPC-Web protocols over `net/http`, so a v2 client behaves exactly as a v1 client did on the wire. It has two entry points:

```go
func NewTransport(httpClient connecthttp.HTTPClient, baseURL string, opts ...Option) connect.Transport
func Mount(mux connecthttp.ServeMux, server *connect.Server, opts ...Option)
```

`NewTransport` builds the client transport. `Mount` installs one `http.Handler` per registered procedure onto a mux, which is the per-procedure registration that [#924](https://github.com/connectrpc/connect-go/issues/924) asked for and lets Connect routes share a mux with sibling HTTP routes. It also installs a catch-all per service: RPC requests for unknown methods of a registered service route through `Server.Call`, failing with `CodeUnimplemented` unless a `SetUnknownHandler` fallback answers them. Non-RPC requests get a plain 404, and unknown services fall through to the mux.

Both entry points share one `Option` type that covers protocol and codec selection, compression, message-size limits, and Connect GET support. Options that do not apply to a side are ignored, so passing a client-only option to `Mount` is harmless. In v1 these were core handler and client options; in v2 they live with the transport that reads them.

```go
connecthttp.WithGRPC()                              // client only
connecthttp.WithReadMaxBytes(1024)
connecthttp.WithSendMaxBytes(2048)
connecthttp.WithCompressMinBytes(512)
connecthttp.WithHTTPGet()                            // client only
connecthttp.WithRequireConnectProtocolHeader()      // server only
connecthttp.WithCodec(connectproto.NewJSONCodec())  // register; one per call
connecthttp.WithCompressor(connectgzip.New())
```

By default `connecthttp` registers the protobuf binary and JSON codecs from `connectproto` and sends with the binary codec, so a transport built with no options is ready to use.

### connectinprocess

[`connectinprocess`](https://pkg.go.dev/connectrpc.com/connect/v2/connectinprocess) dispatches every RPC directly to a `*connect.Server` in the same process, bypassing the wire, codecs, and framing entirely.

```go
func New(handler *connect.Server, opts ...Option) connect.Transport
```

The default message-transfer strategy is `ProtoCopy`, a safe deep copy that runs `proto.Reset` followed by `proto.Merge`, so the client and server never share message state. Callers that want a different trade-off can replace it with `WithCopyFunc`.

The package is built for testing, where it replaces the loopback listener that v1 forced on stream and interceptor tests ([#13](https://github.com/connectrpc/connect-go/issues/13), [#740](https://github.com/connectrpc/connect-go/issues/740)). It is also suitable for production cases such as delegating a call from one service to another in the same binary.

### Custom transports

The `Transport` interface is open for third-party implementations. [`connectinprocess`](https://pkg.go.dev/connectrpc.com/connect/v2/connectinprocess) is an example that ships in this repository. It dispatches directly to a `*connect.Server` in the same process with minimal overhead. To confirm the interface is sufficient beyond HTTP, we also prototyped transports over WebSockets (multiplexing every RPC type over a single connection rather than one HTTP request per call) and over stdin and stdout for protocols like PluginRPC.

Third-party packages can use this interface to support legacy clients or new protocols and map them onto their existing RPC services without regenerating code.

## Streams

Streaming RPCs operate on two interfaces. The client drives a [`ClientStream`](https://pkg.go.dev/connectrpc.com/connect/v2#ClientStream), and the handler receives a `ServerStream`:

```go
type ClientStream interface {
	SendHeaders() error
	Send(msg any) error
	CloseSend() error
	Receive(msg any) error
	Close() error
}

type ServerStream interface {
	Receive(msg any) error
	SendHeaders() error
	Send(msg any) error
}
```

Stream operations take no context: the call context is bound when the transport opens the stream, so per-call values such as loggers are available without threading a context through every `Send` and `Receive`. Clean receive-side completion is reported by an error matching `io.EOF` under `errors.Is`, which separates a clean end-of-stream from a network `io.EOF` that v1 conflated ([#774](https://github.com/connectrpc/connect-go/issues/774), [#397](https://github.com/connectrpc/connect-go/issues/397)).

Ownership differs by side. A caller owns its `ClientStream`: reading to `io.EOF` releases the stream's resources, and canceling the call's context or calling `Close` abandons it early. `Close` is idempotent, so `defer stream.Close()` is always safe. A handler ends the RPC by returning, so `ServerStream` has no `Close`; the transport finalizes the response when the handler returns. Each stream allows one active send-side and one active receive-side operation at a time, and the two sides may run concurrently.

These interfaces are the reason streams are now testable in isolation. Because they are interfaces rather than concrete library structs, a test or interceptor can wrap or fake them directly, which v1 could not do ([#458](https://github.com/connectrpc/connect-go/issues/458)). Generated code defines the typed wrappers shown in [Generated code](#generated-code) over these interfaces.

A client-streaming handler receives messages until `io.EOF`, then returns its response. From the [server example](../internal/example/server/main.go):

```go
func (pingServer) Sum(ctx context.Context, stream pingv1connect.PingServiceSumServerStream) (*v1.SumResponse, error) {
	var total int64
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		total += req.Number
	}
	return &v1.SumResponse{Sum: total}, nil
}
```

## Metadata

Headers and trailers travel on a [`CallInfo`](https://pkg.go.dev/connectrpc.com/connect/v2#CallInfo) carried by the context, which replaces the v1 `Request[T]` and `Response[T]` wrappers. There is one way to read or write metadata in v2.

```go
type CallInfo struct {
	Spec             Spec         // the procedure, stream type, and schema
	PeerAddr         string       // remote peer address; empty for in-process calls
	Protocol         string       // "connect", "grpc", or "grpcweb"
	Codec            string       // "proto" or "json"
	RequestEncoding  string       // request compression; "identity" when uncompressed
	ResponseEncoding string       // response compression
	SendStats        MessageStats // byte counts for the most recent Send
	ReceiveStats     MessageStats // byte counts for the most recent Receive
}

func (c *CallInfo) RequestHeader() *Header
func (c *CallInfo) ResponseHeader() *Header
func (c *CallInfo) ResponseTrailer() *Header
```

Alongside the metadata, the transport populates the exported fields as the RPC runs, replacing the v1 `Peer` struct and giving telemetry the on-the-wire message sizes that v1 never exposed.

A client attaches a `CallInfo` to the context with `connect.NewClientContext` before the call, sets request headers on it, and reads response headers and trailers from the same value after the call returns:

```go
ctx, info := connect.NewClientContext(context.Background())
info.RequestHeader().Set("X-Request-Id", "req-123")
if _, err := client.Ping(ctx, &pingv1.PingRequest{Text: "hello"}); err != nil {
	return err
}
log.Println(info.ResponseHeader().Get("X-Server-Name"))
```

Calling `NewClientContext` is optional. A transport attaches a fresh `CallInfo` when the context does not carry one, so you only call it when you want the handle.

A handler reads its `CallInfo` from the context with `connect.CallInfoForServerContext`, which the server attaches before interceptors and the handler run. The second return value reports whether a `CallInfo` is present; inside a handler it always is:

```go
func (pingServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	info.ResponseHeader().Set("X-Server-Name", "ping-server")
	info.ResponseTrailer().Set("X-Audit-Id", "audit-123")
	return &pingv1.PingResponse{Number: req.Number, Text: req.Text}, nil
}
```

Client and server info use distinct context keys, so a handler that makes an outbound RPC does not leak its inbound metadata onto the outbound call.

For HTTP-specific details such as the HTTP method, URL, response status, and TLS state, the transport sets `CallInfo.TransportInfo` to a `*connecthttp.ClientInfo` on the client side or a `*connecthttp.ServerInfo` on the server side. The `connecthttp.ClientInfoForContext` and `connecthttp.ServerInfoForContext` helpers look them up directly. They report false under a non-HTTP transport, and the accessors are nil-safe.

```go
if httpInfo, _ := connecthttp.ServerInfoForContext(ctx); httpInfo.HTTPMethod() == http.MethodGet {
	// Serving a Connect GET request, for example with an Etag cache.
}
```

## Interceptors

v1 had one `Interceptor` interface with separate methods for unary calls, streaming clients, and streaming handlers. v2 replaces it with two function types that wrap every RPC shape uniformly:

```go
type ClientFunc func(ctx context.Context, spec Spec) (ClientStream, error)
type ServerFunc func(ctx context.Context, spec Spec, stream ServerStream) error

type ClientInterceptor func(next ClientFunc) ClientFunc
type ServerInterceptor func(next ServerFunc) ServerFunc
```

An interceptor wraps one function to produce another. It can return an error without calling `next` to short-circuit the RPC. A `ServerFunc` receives the stream, so a server interceptor wraps it before calling `next`. A `ClientFunc` returns the stream, so a client interceptor wraps the stream that `next` returns to observe messages or the end of the RPC. To see the end of every RPC, wrap both `Close` and `Receive`. A unary calls always `Close`, but a streaming caller may finish by reading to `io.EOF` without calling `Close`. Interceptors are passed to `NewClient` and `NewServer` and run in argument order, so the first interceptor wraps the outermost call.

Client and server interceptors are separate types because the two sides differ in how they drive a stream and usually want different things. An auth interceptor attaches credentials on the client and verifies them on the server. Splitting the types keeps each side's semantics clean and avoids writing one interceptor that tries to do both.

Because the chain is applied once per call shape in `NewClient` and `NewServer`, the interceptor function itself runs at construction, not per RPC. Keep per-RPC state in the function an interceptor returns or in a stream wrapper, never in the interceptor function. A returned function must also pass `next` a context derived from the one it received, since per-call state flows through the context.

First, an interceptor can reject a call using only its headers, before any payload work, which is what makes authn be able to be an interceptor in v2 rather than HTTP middleware:

```go
func newAuthInterceptor(verify func(token string) error) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			token := info.RequestHeader().Get("Authorization")
			if token == "" {
				return connect.NewError(connect.CodeUnauthenticated, "missing credentials")
			}
			if err := verify(token); err != nil {
				return connect.NewError(connect.CodeUnauthenticated, "invalid credentials")
			}
			return next(ctx, spec, stream)
		}
	}
}
```

Second, an interceptor can observe individual messages by wrapping the stream before calling `next`, which is how `connectrpc.com/validate` validates every request message. Third, panic recovery is an ordinary interceptor, because a v2 interceptor surrounds the whole call:

```go
func recoveryInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = connect.Errorf(connect.CodeInternal, "internal server error")
			}
		}()
		return next(ctx, spec, stream)
	}
}
```

v2 does not ship a `RecoveryInterceptor` or a `WithRecover` option. Recovery is a few lines with full access to the `Spec`, so the deprecation tracked in [#816](https://github.com/connectrpc/connect-go/issues/816) is resolved by the new shape rather than a dedicated API.

## Errors

v2 changes error behavior to make accidental leaks hard. A handler fails an RPC by returning an error, but only a locally authored `*connect.Error` is serialized to the wire. Any other error reaches the client as a bare code with no message — `CodeUnknown`, or `CodeCanceled` and `CodeDeadlineExceeded` for context errors — so text sent to callers is always written on purpose.

```go
func NewError(code Code, message string) *Error
func Errorf(code Code, format string, args ...any) *Error

func (e *Error) WithCause(err error) *Error            // local only, never serialized
func (e *Error) WithDetail(detail *ErrorDetail) *Error // serialized
func (e *Error) WithRemote() *Error                    // marks a peer's verdict
func CodeOf(err error) Code
```

`NewError` and `Errorf` take a message string rather than an `error`, so there is no path that serializes an arbitrary error's text by accident. To keep an underlying error for local inspection, attach it as a cause with `WithCause`. Causes are visible to `errors.Is` and `errors.As` on the server but are never sent.
When a client receives an error, it is automatically marked as remote via `WithRemote`. This ensures that if a server makes a downstream call that fails, its transport will not accidentally forward the downstream error as the server's own verdict: a remote error returned from a handler is scrubbed to `CodeInternal` with no message.

A handler shows each case:

```go
func (pingServer) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	if req.Number < 0 {
		// Public message, serialized to the wire.
		return nil, connect.Errorf(connect.CodeInvalidArgument, "number must be positive, got %d", req.Number)
	}
	if req.Text == "leak" {
		// Not a *connect.Error: the client sees CodeUnknown, no message.
		return nil, errDiskFull
	}
	if err := store(ctx, req.Number); err != nil {
		// Public message; the cause stays on the server.
		return nil, connect.NewError(connect.CodeInternal, "store failed").WithCause(err)
	}
	return &pingv1.PingResponse{Number: req.Number, Text: req.Text}, nil
}
```

A caller branches on the code with `connect.CodeOf`, or inspects the full error with `errors.As` into `*connect.Error` for the message and details. This replaces the v1 `IsWireError` and masking knobs added to patch the same leaks ([#222](https://github.com/connectrpc/connect-go/issues/222), [#420](https://github.com/connectrpc/connect-go/issues/420), [#584](https://github.com/connectrpc/connect-go/issues/584)).

Error details are a `connect.ErrorDetail`, a schema-neutral triple of the message's type name, its serialized bytes, and an optional human-readable `debug` form. This form matches the Connect wire encoding.

```go
detail, err := connectproto.NewErrorDetail(&errdetails.RetryInfo{RetryDelay: durationpb.New(time.Second)})
if err != nil {
	return nil, connect.NewError(connect.CodeInternal, "pack detail")
}
return nil, connect.NewError(connect.CodeUnavailable, "try again later").WithDetail(detail)
```

## Codecs and compressors

Both interfaces are now stream-oriented and context-aware: a codec marshals to an `io.Writer` and unmarshals from an `io.Reader`, and a compressor wraps a writer or reader rather than round-tripping whole buffers.

```go
type Codec interface {
	Name() string
	MarshalWrite(ctx context.Context, dst io.Writer, msg any) error
	UnmarshalRead(ctx context.Context, src io.Reader, msg any) error
}

type Compressor interface {
	Name() string
	Compress(dst io.Writer) (io.WriteCloser, error)
	Decompress(src io.Reader) (io.ReadCloser, error)
}
```

The codec methods take a context for per-call values. The transport enforces the decompressed-size limit by wrapping the reader returned from `Decompress`, so a decompression bomb fails before it is fully materialized, closing the v1 gap where discard limits went unenforced on oversized messages ([#620](https://github.com/connectrpc/connect-go/issues/620)).

A `StableCodec` extends `Codec` with `MarshalWriteStable`, a deterministic encoding for features such as Connect GET request URLs, plus `IsBinary` so a binary stable encoding can be text-encoded before it goes in a URL. This exports what was a private interface in v1.

`connectproto` provides the protobuf binary and JSON codecs, and `connectgzip` provides gzip. The compression token names (`identity`, `gzip`, `br`, `zstd`) are defined in the core package so transports and compressor packages agree on them, while the core package itself ships no compressor.

## Behavioral fixes

Several v1 behaviors are wrong but frozen by its compatibility promise. v2 fixes them as part of the redesign:

- Authn runs before payload work, as an interceptor rather than HTTP middleware, so unauthenticated clients cannot trigger decompression and decode ([#584](https://github.com/connectrpc/connect-go/issues/584)).
- Telemetry sees the whole call and on-the-wire sizes, so otelconnect metrics are no longer skewed ([#665](https://github.com/connectrpc/connect-go/issues/665)).
- A clean end-of-stream is distinct from a network `io.EOF` ([#774](https://github.com/connectrpc/connect-go/issues/774), [#397](https://github.com/connectrpc/connect-go/issues/397)).
- Decompression enforces a size limit, so oversized messages fail early ([#620](https://github.com/connectrpc/connect-go/issues/620)).
- Errors are never serialized unless authored as `*connect.Error` ([#584](https://github.com/connectrpc/connect-go/issues/584)).

## Performance

The v2 implementation is based on the current v1 implementation. Performance is similar, but we expect to improve this going forward once we have stabilized the transition.
Removing the generic client instantiation reduces binary size, since the compiler no longer stamps out a client and method set per RPC. Moving the Buf CLI to v2 shrinks its stripped binary by roughly 10%.

## Migration

Migration is more than an import rename, but most of it is mechanical. The [migration guide](v2-migration.md) is the authoritative reference.

A tool, `connectrpc.com/connect/v2/cmd/connect-go-v2-migrate`, automates the bulk of it:

```sh
go install connectrpc.com/connect/v2/cmd/connect-go-v2-migrate@latest
connect-go-v2-migrate
```

It is a dry run by default, printing a diff per file and a warning for anything that isn't a clear AST transform. Pass `-w` to write changes and `-json` for a machine-readable report. Because most code changes depend on the regenerated v2 stubs, migration runs in two passes:

1. Run `connect-go-v2-migrate -w`. While your v1 stubs are still in place it updates only the Buf templates (`buf.gen.yaml`) and prints the steps to switch to v2; it makes no Go source changes yet.
2. Move to v2 and regenerate: `go get -u` the v2 core (plus any generated SDKs at `@v2` and ecosystem modules), reinstall `protoc-gen-connect-go` from the v2 module, then run `buf generate`.
3. Run `connect-go-v2-migrate -w` again. With the stubs now on v2, it rewrites import paths, handler signatures, client call sites, metadata, and streaming, and reports anything that needs a manual change.
4. Run `go mod tidy`, then build and test.

The tool handles the safe rewrites: import paths, removing `Request[T]` and `Response[T]`, server registration and client construction, headers and trailers moving to `CallInfo`, error-constructor rewrites that preserve the v1 wire message, the `simple` option removal, and the streaming send and receive reshaping. It warns and leaves for you the changes that need judgment: custom interceptors rewritten to the new function types, handler stream parameter types that become the generated named type, options that changed name or signature, and ecosystem-package calls (authn, otelconnect, validate, grpcreflect, vanguard) that move to their `/v2` APIs. Each warned pattern is documented in [the migration guide](v2-migration.md).

## Ecosystem

The official Connect ecosystem packages are being updated to fully support v2. Each package will release its own `/v2` module alongside the core framework.
To align with the new architecture, most of these packages have simplified their APIs to either register directly on a `*connect.Server` or provide distinct `ClientInterceptor` and `ServerInterceptor` implementations.


| v1 | v2 |
| --- | --- |
| `connectrpc.com/validate` | `connectrpc.com/validate/v2` |
| `connectrpc.com/otelconnect` | `connectrpc.com/otelconnect/v2` |
| `connectrpc.com/authn` | `connectrpc.com/authn/v2` |
| `connectrpc.com/grpcreflect` | `connectrpc.com/grpcreflect/v2` |
| `connectrpc.com/grpchealth` | `connectrpc.com/grpchealth/v2` |
| `connectrpc.com/vanguard` | `connectrpc.com/vanguard/v2` |
| `connectrpc.com/vanguard/vanguardgrpc` | `connectrpc.com/vanguard/v2/vanguardgrpc` |


## Versioning and support

`connect-go` uses Go's semantic import versioning. The `/v2` suffix is part of the module path, `connectrpc.com/connect/v2`, so v1 and v2 are different modules and a program can depend on both during migration.

The two major versions live in the same git repository:

- v1 is on the v1 branch at module path `connectrpc.com/connect`. It will continue to receive fixes and security patches but not new features.
- v2 is on `main` at module path `connectrpc.com/connect/v2`. It follows semantic versioning within the v2 line.

As part of validating v2, we kept the v1 test suite and updated it to use the v2 APIs. We have also prototyped a branch where v1 is implemented entirely on top of the v2 core. Adopting this unified implementation after the v2 release would let both major versions share a single implementation and reduce the long-term maintenance burden, though v1's public API would remain strictly unchanged either way.
