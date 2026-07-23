# Connect v1 to v2 migration guide

This document will walk you through all you need to know to migrate from v1 to v2.
connect-go v2 improves and simplifies some common APIs.

To get started we will first install the migration tool.
This will help automate most of the mechanical translations.
Then go through examples, and any decisions that might show up and require oversight.

> [!IMPORTANT]
>
> connect-go v1 remains supported. The v1 branch will receive fixes and security updates, so you can migrate at your own pace.

## Running the migration tool

We provide the tool `connect-go-v2-migrate` which takes care of plugin updates and
most code changes. First, install the tool:

```sh
go install connectrpc.com/connect/v2/cmd/connect-go-v2-migrate@latest
```

Then run it from your module directory, or pass paths as arguments:

```sh
connect-go-v2-migrate
```

The tool is safe to run. By default it is a dry run that prints a diff for each
file it would change, plus warnings for anything that needs manual work. Pass
`-w` to write the changes to disk, and `-json` for a machine-readable report.

Most code changes depend on the re-generated v2 code, so the migration runs in
two passes:

1. Run `connect-go-v2-migrate -w` to update `buf.gen.yaml` and apply any code changes that don't depend on generated code.
2. Re-generate your code with the v2 plugin (see [Re-generate code](#re-generate-code)).
3. Run `connect-go-v2-migrate -w` again to finish the changes that bind to the v2 generated code. This will update client calls and handler signatures. 
4. Finally, run `go mod tidy` and then build and test addressing any manual operations warned from the report.

The project won't compile between steps 1 and 3. Work through the sequence,
then fix any remaining warnings the tool printed. Services may be migrated one at a time to reduce the changes, both v1 and v2 libraries can be imported in the same module.

The sections below show each change. Changes are marked:

- ✅ `connect-go-v2-migrate` handles this.
- ⚠️ The tool prints a warning. Update the code by hand.

## Update buf.gen.yaml

The v2 generator keeps the plugin name `protoc-gen-connect-go`. The required
change depends on how `buf.gen.yaml` declares the plugin.

For remote plugins, the reference is pinned to the v2 release:

```diff
version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt: paths=source_relative
- - remote: buf.build/connectrpc/go:v1.18.1
+ - remote: buf.build/connectrpc/go:v2.0.0
    out: gen
    opt: paths=source_relative
```

✅ `connect-go-v2-migrate` handles this.

For local plugins (`local: protoc-gen-connect-go`), the `buf.gen.yaml` entry
stays the same because the v1 and v2 plugins share the binary name.
Reinstalling the binary from the v2 module switches generation to v2:

```sh
go install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@latest
```

If the plugin runs through go.mod (`local: [go, tool, protoc-gen-connect-go]`),
update the tool dependency instead:

```sh
go get -tool connectrpc.com/connect/v2/cmd/protoc-gen-connect-go
go mod tidy
```

⚠️ The tool can't install binaries or change go.mod, so it prints the command
to run.

v2 generated code is always in the simple style, and the generator rejects the
v1 `simple` option:

```diff
version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
    opt:
      - paths=source_relative
-     - simple=true
```

✅ `connect-go-v2-migrate` handles this, for both the list form and the inline
form (`opt: paths=source_relative,simple=true`).

## Re-generate code

With `buf.gen.yaml` updated and the plugin installed, re-generate:

```sh
buf generate
```

The migration tool never edits generated code, so this step is yours. After
re-generating, run `connect-go-v2-migrate -w` again to migrate the code that
depends on the generated packages.

## Update dependencies

The tool rewrites import paths in your code but does not edit go.mod. After
the final tool run, resolve the new modules:

```sh
go mod tidy
```

The core module and the ecosystem packages move to `/v2` module paths:

| v1 | v2 |
| --- | --- |
| `connectrpc.com/connect` | `connectrpc.com/connect/v2` |
| `connectrpc.com/validate` | `connectrpc.com/validate/v2` |
| `connectrpc.com/otelconnect` | `connectrpc.com/otelconnect/v2` |
| `connectrpc.com/authn` | `connectrpc.com/authn/v2` |
| `connectrpc.com/grpcreflect` | `connectrpc.com/grpcreflect/v2` |
| `connectrpc.com/grpchealth` | `connectrpc.com/grpchealth/v2` |
| `connectrpc.com/vanguard` | `connectrpc.com/vanguard/v2` |

The ecosystem modules release separately, following the core module. If
`go mod tidy` can't resolve a `/v2` module yet, check the package's
repository for its v2 release status. The API changes in each package are
covered in [Ecosystem packages](#ecosystem-packages).

v2 also splits the runtime into subpackages of the same module. The core
package no longer depends on `net/http`:

| Package | Contents |
| --- | --- |
| `connectrpc.com/connect/v2` | Core types, imported by generated code |
| `connectrpc.com/connect/v2/connecthttp` | `net/http` server and client bindings |
| `connectrpc.com/connect/v2/connectproto` | Protobuf codecs |
| `connectrpc.com/connect/v2/connectgzip` | Gzip compression |
| `connectrpc.com/connect/v2/connectinprocess` | In-process transport |

## Update your application code

### Requests and responses

v1 wrapped every message in `connect.Request[T]` or `connect.Response[T]`.
v2 generated code passes protobuf messages directly:

```go
// v1
func (s *pingServer) Ping(
	ctx context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(&pingv1.PingResponse{Text: req.Msg.Text}), nil
}
```

```go
// v2
func (s *pingServer) Ping(
	ctx context.Context,
	req *pingv1.PingRequest,
) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{Text: req.Text}, nil
}
```

Client calls change the same way: no `connect.NewRequest` wrapper, and no
`.Msg` field access on the response.

✅ `connect-go-v2-migrate` handles this.

### Server construction

v1 generated a constructor returning a path and an `http.Handler`. In v2,
register services on a `*connect.Server` and mount it with `connecthttp`:

```go
// v1
mux := http.NewServeMux()
mux.Handle(pingv1connect.NewPingServiceHandler(
	&pingServer{},
	connect.WithInterceptors(validate.NewInterceptor()),
))
```

```go
// v2
mux := http.NewServeMux()
server := connect.NewServer(validate.NewServerInterceptor())
pingv1connect.RegisterPingServiceHandler(server, &pingServer{})
connecthttp.Mount(mux, server)
```

Interceptors move from `connect.WithInterceptors(...)` to arguments of
`connect.NewServer`. Other handler options move to `connecthttp.Mount`.

In v2, interceptors apply to every service on a server; there is no
per-handler option. Consecutive `mux.Handle(...)` calls with identical
options share one server. Handlers with different interceptors keep their v1
behavior by registering on separate servers mounted on the same mux.

✅ `connect-go-v2-migrate` handles the inline `mux.Handle(...)` shape, grouping
consecutive handlers with identical options onto one server.
⚠️ Handlers with differing options get one server per group; the tool warns
so you can review whether they should share one. Other shapes, like assigning
the path and handler to variables first, are also warned and need a manual
update.

### Client construction

v1 clients took an HTTP client and a base URL. v2 clients take a
`*connect.Client`, which wraps a transport:

```go
// v1
client := pingv1connect.NewPingServiceClient(http.DefaultClient, "http://localhost:8080")
```

```go
// v2
client := pingv1connect.NewPingServiceClient(connect.NewClient(
	connecthttp.NewTransport(http.DefaultClient, "http://localhost:8080"),
))
```

Interceptors move to `connect.NewClient`. Other client options move to
`connecthttp.NewTransport`.

✅ `connect-go-v2-migrate` handles this.

### Headers and trailers

With the wrapper types gone, metadata moves to a `*connect.CallInfo` carried
on the context. It exposes `RequestHeader()`, `ResponseHeader()`, and
`ResponseTrailer()`.

Clients attach the info to the context before the call and read response
metadata from it after:

```go
// v1
req := connect.NewRequest(&pingv1.PingRequest{Text: "hello"})
req.Header().Set("X-Request-Id", requestID)
res, err := client.Ping(ctx, req)
```

```go
// v2
ctx, info := connect.NewClientContext(ctx)
info.RequestHeader().Set("X-Request-Id", requestID)
res, err := client.Ping(ctx, &pingv1.PingRequest{Text: "hello"})
```

Handlers get the info from the handler's context:

```go
// v1
func (s *pingServer) Ping(
	ctx context.Context,
	req *connect.Request[pingv1.PingRequest],
) (*connect.Response[pingv1.PingResponse], error) {
	token := req.Header().Get("Authorization")
	res := connect.NewResponse(&pingv1.PingResponse{})
	res.Header().Set("X-Server-Name", "ping-server")
	return res, nil
}
```

```go
// v2
func (s *pingServer) Ping(
	ctx context.Context,
	req *pingv1.PingRequest,
) (*pingv1.PingResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	token := info.RequestHeader().Get("Authorization")
	res := &pingv1.PingResponse{}
	info.ResponseHeader().Set("X-Server-Name", "ping-server")
	return res, nil
}
```

The v1 `connect.CallInfoForHandlerContext(ctx)` is renamed to
`connect.CallInfoForServerContext(ctx)`; both return `(CallInfo, bool)`, so
existing comma-ok call sites carry over unchanged.

✅ `connect-go-v2-migrate` moves metadata access to the `CallInfo`:

- A handler's request-header read (`req.Header()`) and response
  header/trailer set (`res.Header()`/`res.Trailer()` on a `connect.NewResponse`
  holder) become `info.*` on a seeded
  `info, _ := connect.CallInfoForServerContext(ctx)`.
- A client's request header set on a `connect.NewRequest` and response
  header/trailer read (`res.Header()`/`res.Trailer()`) become
  `connect.NewClientContext(ctx)` plus `info.*`.
- `connect.CallInfoForHandlerContext` renames to `connect.CallInfoForServerContext`.

⚠️ A function that both sets a client request header and reads a client response
header, or that touches several response holders, is warned rather than seeding
one shared context.

### Errors

`connect.NewError` takes a message string instead of an `error`. In v2 a
plain `error` returned from a handler is never serialized to the wire, so
internal details can't leak by accident.

```go
// v1
return nil, connect.NewError(connect.CodeInternal, err)
```

```go
// v2
return nil, connect.NewError(connect.CodeInternal, err.Error())
```

v1 sent the error's full string to the client, so the tool rewrites to
`err.Error()` to preserve what callers see today. Common argument shapes
collapse to simpler forms with the same wire message:

```go
connect.NewError(code, errors.New("nope"))     // -> connect.NewError(code, "nope")
connect.NewError(code, fmt.Errorf("x %s", a))  // -> connect.Errorf(code, "x %s", a)
connect.NewError(code, nil)                    // -> connect.NewError(code, "")
```

To hide the underlying error from clients, attach it as a cause instead. The
cause is visible to `errors.Is` and `errors.As` on the server but never sent
to the client:

```go
return nil, connect.NewError(connect.CodeInternal, "something went wrong").WithCause(err)
```

Error details keep the v1 shape with the constructor moved to `connectproto`.
`AddDetail` becomes the cloning `WithDetail` builder, and `ErrorDetail.Value()`
becomes `connectproto.UnmarshalErrorDetail`:

```go
// v1
cErr := connect.NewError(connect.CodeInternal, err)
if detail, derr := connect.NewErrorDetail(info); derr == nil {
	cErr.AddDetail(detail)
}
```

```go
// v2
cErr := connect.NewError(connect.CodeInternal, err.Error())
if detail, derr := connectproto.NewErrorDetail(info); derr == nil {
	cErr = cErr.WithDetail(detail)
}
```

The wire-error helpers became `*connect.Error` methods:

| v1 | v2 |
| --- | --- |
| `connect.NewWireError(code, err)` | `connect.NewError(code, msg).WithRemote()` |
| `connect.IsWireError(err)` | `errors.As(err, &cerr)` then `cerr.IsRemote()` |

Error codes (`connect.CodeNotFound`, `connect.CodeOf`, and friends) are
unchanged.

✅ `connect-go-v2-migrate` handles the `NewError` rewrite and retargets the
`NewErrorDetail`/`AddDetail` guard to `connectproto`, preserving the v1 wire
message. Switching to `WithCause` is a behavior change to make by hand, and
⚠️ the wire-error helpers and other `ErrorDetail` uses are warned for a manual
update.

### Options moved to connecthttp

HTTP-specific options moved from the core package to `connecthttp`. Most keep
their name and signature and only change package:

```go
// v1
connect.WithReadMaxBytes(1024)
connect.WithSendMaxBytes(2048)
connect.WithCompressMinBytes(512)
connect.WithRequireConnectProtocolHeader()
connect.WithSendGzip()
connect.WithHTTPGet()
connect.WithHTTPGetMaxURLSize(8192, true)
connect.WithProtoJSON()
connect.WithGRPC()
connect.WithGRPCWeb()
connect.WithCodec(codec)
connect.WithSendCompression("gzip")
```

```go
// v2
connecthttp.WithReadMaxBytes(1024)
connecthttp.WithSendMaxBytes(2048)
connecthttp.WithCompressMinBytes(512)
connecthttp.WithRequireConnectProtocolHeader()
connecthttp.WithSendGzip()
connecthttp.WithHTTPGet()
connecthttp.WithHTTPGetMaxURLSize(8192, true)
connecthttp.WithProtoJSON()
connecthttp.WithGRPC()
connecthttp.WithGRPCWeb()
connecthttp.WithCodec(codec)
connecthttp.WithSendCompression("gzip")
```

The `ErrorWriter` type, `NewErrorWriter`, and `IsNotModifiedError` move to
`connecthttp` the same way, keeping their signatures.

✅ `connect-go-v2-migrate` handles these.

Options whose signature changed are warned with the v2 replacement:

| v1 | v2 |
| --- | --- |
| `connect.WithCompression(name, dec, comp)` | `connecthttp.WithCompressor(connect.Compressor)` (register a `connectgzip`-style compressor) |
| `connect.WithAcceptCompression(name, dec, comp)` | `connecthttp.WithCompressor(...)` to register, then `connecthttp.WithAcceptCompression(name)` to advertise; `connecthttp.WithNoCompression()` to disable |
| `connect.WithConditionalHandlerOptions(fn)` | `connecthttp.WithConditionalOptions(func(connect.Spec) []connecthttp.Option)` (callback signature changed) |
| `connect.NewNotModifiedError(header)` | `connecthttp.NewNotModifiedError()` (no header argument) |

⚠️ Update these by hand.

### Interceptors

v1 had one `Interceptor` interface with separate methods for unary calls,
streaming clients, and streaming handlers. v2 replaces it with two function
types, `connect.ClientInterceptor` and `connect.ServerInterceptor`, that wrap
every RPC type uniformly:

```go
func NewServerInterceptor(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			err := next(ctx, spec, stream)
			logger.InfoContext(ctx, "rpc completed",
				slog.String("procedure", spec.Procedure),
				slog.Any("error", err),
			)
			return err
		}
	}
}
```

The tool maps ecosystem interceptor constructors where it can:

- ✅ `validate.NewInterceptor()` becomes `validate.NewServerInterceptor()` or
  `validate.NewClientInterceptor()`, depending on where it's used.
- ⚠️ `otelconnect.NewInterceptor()` becomes
  `otelconnect.NewServerInterceptor()` or `otelconnect.NewClientInterceptor()`,
  which return an error. Assign the interceptor before constructing the server
  or client.
- ⚠️ Custom interceptors must be rewritten by hand to the new function types.
  The tool warns at each one.

### Streaming

Streaming generated code defines a named stream type per RPC (for example,
`PingServiceCumSumClientStream`) instead of the v1 generics. Stream `Send`, `Receive`,
and `SendHeaders` keep their v1 shape and take no `context.Context`; the call's
context is bound when the stream is opened.

Stream metadata moves off the stream and onto the `CallInfo`, since the
generated stream types no longer carry headers. A handler stream's
`RequestHeader()`/`ResponseHeader()`/`ResponseTrailer()` become `info.*` on a
seeded `info, _ := connect.CallInfoForServerContext(ctx)`; a client stream's
seed a `connect.NewClientContext(ctx)` and read from the returned `info`.

✅ `connect-go-v2-migrate` handles both.

The v1 receive loop, `Receive() bool` with `Msg()` and a trailing `Err()`
check, becomes a `Receive()` call returning the message and an error. `io.EOF`
marks the end of the stream:

```go
// v1
for stream.Receive() {
	total += stream.Msg().Number
}
if err := stream.Err(); err != nil {
	return nil, err
}
```

```go
// v2
for {
	msg, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			break
		}
		return nil, err
	}
	total += msg.Number
}
```

✅ `connect-go-v2-migrate` handles this.

On the client side, stream constructors return an error and the close methods
are renamed:

| v1 | v2 |
| --- | --- |
| `stream := client.CumSum(ctx)` | `stream, err := client.CumSum(ctx)` |
| `stream.CloseRequest()` | `stream.CloseSend()` |
| `stream.CloseResponse()` | `stream.Close()` |
| `res, err := stream.CloseAndReceive()` | `res, err := stream.CloseAndReceive()` (returns the bare message) |

✅ `connect-go-v2-migrate` handles this.

Handler stream parameters change the same way. A v1 handler takes a generic
like `*connect.BidiStream[Req, Res]`, while v2 passes a named type generated
per RPC (like `PingServiceCumSumServerStream`):

```go
// v1
func (s *pingServer) CumSum(ctx context.Context, stream *connect.BidiStream[pingv1.CumSumRequest, pingv1.CumSumResponse]) error

// v2
func (s *pingServer) CumSum(ctx context.Context, stream pingv1connect.PingServiceCumSumServerStream) error
```

✅ `connect-go-v2-migrate` resolves the parameter type to the generated stream
type by matching the handler to its RPC.
⚠️ When several services share an RPC name and message types, the match is
ambiguous; the tool warns so you can pick the right generated type by hand.

### Helper packages

Some shared helpers need a new sibling API rather than an in-place rewrite,
for example a helper that returns a v1 `UnaryInterceptorFunc` becoming a v2
`ServerInterceptor`. ⚠️ This is project-specific: add the v2 helper alongside
the v1 one, then switch call sites to it by hand.

## Ecosystem packages

Each ecosystem package releases its own `/v2` module. Beyond the import path,
most also reshape their API to register on a `*connect.Server`, so
interceptors and alternative transports cover them. The sections below show
each change.

### validate

`validate.NewInterceptor` splits into server and client forms:

```go
// v1
connect.WithInterceptors(validate.NewInterceptor())
```

```go
// v2
connect.NewServer(validate.NewServerInterceptor())
```

✅ `connect-go-v2-migrate` handles this, picking the server or client form from
where the interceptor is used.

### otelconnect

`otelconnect.NewInterceptor` also splits into `NewServerInterceptor` and
`NewClientInterceptor`. Both still return an error, so assign them before
constructing the server or client:

```go
// v1
interceptor, err := otelconnect.NewInterceptor()
```

```go
// v2
serverInterceptor, err := otelconnect.NewServerInterceptor()
```

⚠️ The tool can't tell the server side from the client side at the
assignment, so it warns and leaves the call for you to pick the right form.

### authn

Authentication moves from HTTP middleware to a server interceptor, and
`AuthFunc` no longer receives an `*http.Request`:

```go
// v1
type AuthFunc func(ctx context.Context, req *http.Request) (any, error)

handler := authn.NewMiddleware(authenticate).Wrap(mux)
```

```go
// v2
type AuthFunc func(ctx context.Context, spec connect.Spec, req *connect.Header) (any, error)

server := connect.NewServer(authn.NewServerInterceptor(authenticate))
```

`authn.GetInfo` is unchanged. An `AuthFunc` that read headers from the
request ports directly to `connect.Header`. HTTP-level details move to the
transport: read TLS state and the peer address with
`connecthttp.ServerInfoForContext(ctx)`.

⚠️ Port the `AuthFunc` body and replace the middleware by hand. The tool
warns at each `authn.NewMiddleware` call.

### grpchealth

The health service registers on the server instead of wrapping a mux:

```go
// v1
mux.Handle(grpchealth.NewHandler(checker))
```

```go
// v2
grpchealth.Register(server, checker)
```

✅ `connect-go-v2-migrate` handles the inline `mux.Handle(...)` shape. A health
handler registered alongside your services with the same options joins their
server; with different options it gets its own server, keeping its v1
interceptor behavior.

v2 also adds `grpchealth.NewClient` for calling health checks; v1 had no
client.

### grpcreflect

Reflection registers on the server with one call. `Register` serves both the
v1 and v1alpha reflection APIs, and by default describes the services
registered on the server, so the static service list usually disappears:

```go
// v1
reflector := grpcreflect.NewStaticReflector("acme.user.v1.UserService")
mux.Handle(grpcreflect.NewHandlerV1(reflector))
mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
```

```go
// v2
grpcreflect.Register(server)
```

To expose a different set of services, for example when proxying, pass
`grpcreflect.WithNamer`.

⚠️ The tool warns at each `NewHandlerV1`, `NewHandlerV1Alpha`, and
`NewStaticReflector` call; collapse them to one `Register` by hand.

The reflection client changes like the generated clients:

```go
// v1
client := grpcreflect.NewClient(http.DefaultClient, "http://localhost:8080")
```

```go
// v2
client := grpcreflect.NewClient(connect.NewClient(
	connecthttp.NewTransport(http.DefaultClient, "http://localhost:8080"),
))
```

✅ `connect-go-v2-migrate` handles this.

### vanguard

REST transcoding mounts the server's REST routes directly; the
`Transcoder` and `Service` types are gone. Methods registered on the server
whose descriptors carry a `google.api.http` annotation become REST endpoints:

```go
// v1
services := []*vanguard.Service{
	vanguard.NewService(pingv1connect.PingServiceName, handler),
}
transcoder, err := vanguard.NewTranscoder(services)
mux.Handle("/", transcoder)
```

```go
// v2
err := vanguard.Mount(mux, server)
```

For gRPC servers, `vanguardgrpc.NewTranscoder(grpcServer)` becomes
`vanguardgrpc.NewServiceRegistrar(server)`, which registers gRPC service
implementations on a `*connect.Server`.

⚠️ Update vanguard by hand. The tool warns at each v1 call site.

## Testing with the in-process transport

v2 clients are not tied to HTTP, so tests no longer need a listener or an
`httptest.Server`. The `connectinprocess` package connects a client directly
to a server in the same process:

```go
func TestPingService(t *testing.T) {
	server := connect.NewServer()
	pingv1connect.RegisterPingServiceHandler(server, &pingServer{})

	client := connect.NewClient(connectinprocess.New(server))
	pingClient := pingv1connect.NewPingServiceClient(client)

	res, err := pingClient.Ping(t.Context(), &pingv1.PingRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got, want := res.GetText(), "hello"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
}
```

This is not a required migration step, but in-process tests are faster and
less flaky than tests over a loopback listener.

## Getting help

If your migration hits a case not covered here, or the tool produces a result
you didn't expect, please [open an issue](https://github.com/connectrpc/connect-go/issues)
or ask in [Slack](https://buf.build/links/slack).
