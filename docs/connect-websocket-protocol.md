# Connect-over-WebSocket Protocol

Status: **Draft**, unratified.
Companion document: [Decisions for WebSocket protocol support for ConnectRPC](https://app.notion.com/p/Decisions-for-WebSocket-protocol-support-for-ConnectRPC-3e50f90884ac80f79f2bf7a90c1183c1?pvs=21)
records the rationale behind decisions specified in this document. This document is the normative wire format that all implementations should be written against.

## 1. Conventions

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are to be
interpreted as described in RFC 2119 and RFC 8174.

*Sender* and *receiver* denote the two ends of one RPC. *Client* is the peer
that initiated the WebSocket handshake; *server* is the peer that accepted it.

Byte values are written in hexadecimal, and Unicode scalar values in the usual
`U+XXXX` form.

## 2. Scope

This document specifies how the [Connect protocol](https://connectrpc.com/docs/protocol/) is carried over a
WebSocket connection ([RFC 6455](https://datatracker.ietf.org/doc/html/rfc6455)). It covers connection
establishment, message framing, metadata, deadlines, compression, and
termination.

It does not specify service definitions, codec behavior, or error semantics
beyond their wire encoding; those are unchanged from Connect.

## 3. Relationship to the Connect protocol

### 3.1 Normative divergences

A Connect-over-WebSocket message stream is **not** a conforming Connect
streaming body. It keeps Connect's `EndStreamResponse` schema and its error
model. It departs from Connect in the following ways:

| # | Divergence | Reason |
| --- | --- | --- |
| 1 | Messages carry a marker, not a flags byte and length | Each frame in a WebSocket message includes a length field, so a user-specified length only restates it with a possibly erroneous value. ([§5](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |
| 2 | The frame type specifies the message encoding | The WebSocket protocol includes a mechanism for specifying the message type. Most messages are JSON, but binary Protobufs can be sent as well. ([§6.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |
| 3 | Client end-of-stream is an in-band message | WebSocket has no half-close, so request EOF cannot be a transport event |
| 4 | Request metadata is an in-band message | The browser WebSocket API cannot set headers on a handshake |
| 5 | No `Streaming-Content-Encoding`, no message-level compression | The WebSocket layer compresses whole messages ([§10](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |
| 6 | A deadline travels only as a query parameter | Browsers cannot set request headers, and metadata cannot bound its own read |
| 7 | One connection carries exactly one RPC | The procedure is selected by the handshake URI |
| 8 | The handshake requires HTTP/1.1 | RFC 6455 defines it there; RFC 8441 is not adopted ([§4.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |
| 9 | Every stream opens with a metadata message, in both directions | The client's is consumed before dispatch, so request metadata is complete for interceptors |
| 10 | A server SHOULD impose its own deadline | A server waits for the opening message, so an unbounded RPC could park a connection ([§11](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |

Sending an HTTP request with a `Connection: Upgrade` header, an `Upgrade: websocket` header, and a `Sec-WebSocket-Protocol` header that specifies one of `connectrpc.1+proto`or `connectrpc.1+json` ([§4.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) indicates that a peer is agreeing to use Connect-over-WebSocket instead of standard ConnectRPC streaming.

### 3.2 What this binding does not borrow

The envelope header used for ConnectRPC streaming (header byte plus length) is not used for Connect-over-WebSocket. There are no changes to the existing ConnectRPC streaming protocol to support Connect-over-Websocket.

### 3.3 What this binding does borrow

The `EndStreamResponse` schema ([§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) and the error model are both identical on the wire to Connect-over-HTTP. That sharing is deliberate: an implementation of one can decode the other’s terminal message unchanged, and the two should not drift.

## 4. Connection establishment

### 4.1 HTTP version

The handshake MUST be an HTTP/1.1 request. [RFC 6455](https://datatracker.ietf.org/doc/html/rfc6455) defines it over
HTTP/1.1, and this document does not adopt [RFC 8441](https://datatracker.ietf.org/doc/html/rfc8441)'s Extended
CONNECT, which carries WebSocket over HTTP/2.

A **client** whose HTTP stack uses HTTP/2 MUST ensure the handshake goes over
HTTP/1.1. This is usually automatic: a client library that
recognizes `Connection: Upgrade` with `Upgrade: websocket` routes that request
to an HTTP/1.1 connection. The request MUST be a GET request. Other tokens MAY appear in the `Connection` header alongside `Upgrade` and the upgrade is still recognized. All token and header matches MUST be case-insensitive.

A **server** MUST offer HTTP/1.1 on the listener that accepts handshakes. It
MAY offer HTTP/2 as well. Only the handshake requires HTTP/1.1, so RPCs that a client routes over plain HTTP still negotiate HTTP/2 on the same connection pool. Other RPCs can still use HTTP/2 connections.

A server that offers only HTTP/2 — including h2c — cannot accept a handshake,
because there is no HTTP/1.1 connection to take over. An implementation SHOULD
report that as a server configuration fault rather than a client error.

A server MUST refuse an Extended CONNECT request for a Connect procedure rather than answer it as an HTTP RPC.

### 4.2 Handshake URI

The RPC's procedure is the path of the handshake URI with an optional WebSocket path prefix. A client MUST form it by joining the base URL's path with the optional path prefix and the procedure path:

```
wss://api.example.com/connect.ping.v1.PingService/CumSum     # no path prefix
wss://api.example.com/ws/connect.ping.v1.PingService/CumSum  # path prefix
```

A trailing `/` on the base path MUST NOT produce a doubled separator. The
scheme MUST be `ws` or `wss`, corresponding to the `http` or `https` scheme the
same service would be reached at over Connect-over-HTTP.

One connection carries exactly one RPC. A peer MUST NOT begin a second RPC on a
connection.

By default, any registered ConnectRPC endpoint can accept either Connect-over-HTTP or Connect-over-WebSocket. If the `Connection: Upgrade`, `Upgrade: websocket` headers are present on the HTTP request, along with a `Sec-WebSocket-Protocol` header that specifies a valid Connect-over-WebSocket subprotocol, Connect-over-WebSocket MUST be used. Otherwise, Connect-over-HTTP MUST be used.

If a server is configured to only accept WebSocket traffic on a specified path prefix, the behavior is changed. With a path prefix configured, the `Connection: Upgrade` and `Upgrade: websocket` headers MUST be present on any HTTP request to an endpoint with the path prefix and Connect-over-WebSocket MUST be used. For any HTTP request to an endpoint without a path prefix, Connect-over-HTTP MUST be used. It is an error to send the `Connection: Upgrade` and `Upgrade: websocket` headers to those endpoints.

#### 4.2.1 Path prefix

A deployment MAY place every WebSocket path under a common
prefix, so that an upgrade is distinguishable from an ordinary RPC by URL
alone. Some load balancers need that to route WebSocket traffic differently: upgrade support, longer idle timeouts, a connection-based balancing algorithm such as least connections, and connection draining during deployments.

Both peers MUST agree on the value. A client forms the handshake URI as
base + prefix + procedure, and plain HTTP RPCs keep the bare procedure paths.

When a prefix is configured:

- A server MUST answer a non-upgrade request under the prefix with
`426 Upgrade Required` and an `Upgrade: websocket` header.
- A server MUST NOT accept an upgrade at the bare procedure paths and SHOULD return `400 Bad Request`.

### 4.3 Subprotocol negotiation

The codec is selected by the WebSocket subprotocol.

| Token | Codec |
| --- | --- |
| `connectrpc.1+proto` | Protobuf binary |
| `connectrpc.1+json` | Protobuf JSON |

A client MUST offer exactly one of these tokens in `Sec-WebSocket-Protocol`.
A client MUST encode with the codec specified.

A server MAY support only one of these codecs. If a server recognizes and supports the specified codec, it MUST echo exactly that token in its response. 

A server MUST fail the handshake when it cannot serve the offered token. If the offered token is not one of the two above, the status should be `400 Bad Request`.

If the server recognizes the token, but does not support its codec, it MUST echo the token and report the error in-band (§4.5) with the code `unimplemented`. The error message SHOULD name the codecs that the server supports.

The distinction between the two failures is worth keeping, because the remedies differ. A `400` says that the client is not speaking this protocol. The in-band error says that it is, and that it must offer another codec. The in-band error keeps this distinction visible to browser clients, which cannot read a status code.

A client MUST verify that the echoed token is the one it offered. If it is not, it MUST fail the connection **without a Close frame**, because the WebSocket is not established yet.

`Connect-Protocol-Version` is implied by the subprotocol and MUST NOT be sent
as a header.

### 4.4 Origin

A server MUST reject a cross-origin handshake with HTTP `403 Forbidden` unless
it has been configured to permit one. A handshake carrying no `Origin` header
is not from a browser and MUST NOT be rejected on origin grounds.

**Same-origin means the same host.** A server MUST compare the host and port of the `Origin` header, exactly as written, with the `Host` header, case-insensitively. It MUST NOT require the schemes to match. The scheme is left out deliberately: a deployment terminating TLS at a proxy sees `https` in the `Origin` and its own plaintext `Host`, and a comparison including the scheme would reject its own pages.

As a result, a page that is served over plain HTTP from the same host passes this check. A deployment that serves plain HTTP on that host SHOULD use HSTS, or SHOULD configure the server to accept only `https` origins.

The comparison does not remove default ports. If a proxy adds an explicit default port to `Host` (for example, `:443`), the comparison fails, and the deployment MUST configure its permitted origins explicitly.

A browser attaches the user's ambient credentials to a WebSocket handshake and
performs no CORS preflight, so an accept-all default would let any origin open
an authenticated stream.

### 4.5 Handshake failures and browser clients

A browser does not tell a script why a handshake failed. The WHATWG WebSockets Standard requires that a script cannot distinguish a rejected handshake from an unreachable host, a refused connection, or a failed TLS handshake. In all of these cases, the close code is `1006`. Thus, a browser client cannot read the status code, the headers, or the body of a rejected handshake. Browser developer tools and non-browser clients can read them.

A browser client whose connection fails before it opens MUST report the RPC as failed with the code `unavailable`. The error message SHOULD say that the reason is not available to the client.

**Connection failures use HTTP status codes.** A server MUST reject the handshake with an HTTP status, and MUST NOT complete the upgrade, when the failure concerns the connection:

- the offered subprotocol token is not a Connect-over-WebSocket token (`400`, §4.3);
- an upgrade arrives at a bare procedure path while a prefix is configured (`400`, §4.2.1);
- the handshake is cross-origin (`403`, §4.4).

A server MUST NOT complete a cross-origin upgrade only to report the error. The WebSocket API does not apply CORS, so a page from any origin could read the error message.

**RPC failures are reported in-band.** When a server recognizes the offered token, but rejects the RPC, it SHOULD complete the upgrade, send `M{}`, send an `S` message with the error, and close with `1000`. It MUST NOT dispatch the RPC. This applies to:

- a token whose codec the server does not support (§4.3);
- an invalid `connect-timeout-ms` value (§11).

`M` and `S` are JSON text messages under both subprotocols (§6.1). Thus, a server can send them for a codec that it does not support.

## 5. Message markers

Every message begins with one marker: a single byte whose high bit is `0`.
This marker specifies the type of the message. The payload follows it immediately,
with nothing in between.

The valid markers and message types are:

| Marker | Name | Direction | Payload |
| --- | --- | --- | --- |
| `B` | Body | either | an RPC message |
| `M` | Leading-Metadata | either | JSON metadata ([§7.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |
| `S` | Server-End-Stream | server → client | `EndStreamResponse` JSON ([§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |
| `C` | Client-End-Stream | client → server | a final body, or nothing ([§8](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) |

A receiver MUST treat a message whose marker it does not recognize as a
protocol error, and MUST NOT guess at the payload. A receiver MUST treat a
marker valid only in the opposite direction as a protocol error: `S` from a
client, `C` from a server.

A message that is empty — no marker at all — is a protocol error.

**A protocol error ends the stream.** Wherever this document says a receiver MUST treat something as a protocol error, that receiver MUST end the stream: a server by sending an `S` message carrying the error and then closing with a 1002 code ([§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)), a client by failing the RPC and closing with a 3102 code. A receiver MUST NOT skip the offending message and carry on.

This applies to an unknown marker in particular, which [§5.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21) might otherwise seem to invite a receiver to ignore for forward compatibility. It does not: a marker is defined before it is used, so one that arrives unrecognized means the peer believes it is speaking a dialect this receiver
does not have, and continuing would silently misread whatever follows.

If a server detects a protocol error before the server sends its `M` message, it MUST send its `M` message before sending the `S` message.

### 5.1 The marker space

A marker is one byte in the range `0x00`–`0x7F`. The four markers listed above are the only
valid values in this revision; the other 124 are unassigned, and a receiver
MUST treat any of them as an unknown marker.

**The high bit is reserved and MUST be `0`.** A receiver MUST reject a first
byte of `0x80` or greater as a protocol error, without interpreting the rest of
the message. Nothing in this revision sets that bit, so holding it back leaves
a later one a signal it can define — a longer marker, or a different framing
entirely — that no conforming implementation of this revision can already be
emitting.

Any marker defined in a future revision of this specification SHOULD be printable ASCII whose value is less than `0x80`, for the same reason the first four are: it provides a mnemonic name for the message which is easily viewable in a browser client's developer tools. In addition, most messages are sent as text messages, so a printable character is advantageous. Keeping the marker inside `0x00`–`0x7F` makes it a valid one byte UTF-8 code sequence, required for inclusion in a text message.

## 6. Message framing

Each WebSocket message MUST contain exactly one marker and its payload. A
receiver MUST NOT carry decoding state from one message into the next.

The unit is a WebSocket *message*, not a frame. A sender MAY fragment one
across continuation frames ([RFC 6455 §5.4](https://datatracker.ietf.org/doc/html/rfc6455#section-5.4)) and a receiver MUST
reassemble before interpreting the marker: a marker is not a per-frame tag, and
a fragment is not a message. Fragmentation is otherwise invisible to this
binding, which never requires or forbids it.

### 6.1 Text and binary

The WebSocket frame type says how the payload is encoded:

| Frame type | Payload encoding |
| --- | --- |
| text | JSON |
| binary | Protobuf binary |

A message MUST be sent as a binary frame if and only if its payload is Protobuf binary; every other message MUST be a text frame. Stated per marker:

- `M` and `S` are always JSON, so always text.
- `B` follows the codec the subprotocol selected ([§4.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)): binary for Protobuf, text for JSON.
- `C` with a final body follows the codec as `B` does. `C` alone carries no payload and specifies the frame type that was negotiated during the initial handshake.
- `B` alone — the marker with no payload — is the empty Protobuf message and
MUST be binary. In JSON an empty message is `{}`, never zero bytes, so a text
frame containing only `B` is a protocol error.
- `M` and `S` alone are protocol errors for the same reason. Their payloads are
JSON objects, and the empty object is `{}`. See [§7.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21) and [§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21).

A connection therefore mixes frame types when the codec is Protobuf: bodies arrive binary while metadata and end-of-stream arrive text. That is intended. The frame type is a type tag the transport supplies for free, and a receiver knows how to parse a payload before it has looked at anything but the frame.

A text message MUST contain valid UTF-8, which [RFC 6455](https://datatracker.ietf.org/doc/html/rfc6455) §8.1 requires of every text message and which a browser enforces. A marker is a byte below`0x80` ([§5.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) and JSON is UTF-8, so a conforming message satisfies this by construction. 

An implementation MUST validate the UTF-8 encoding of every text message. A peer that emits
invalid UTF-8 in a text frame has its connection closed by a browser, and a library that does not validate will not reproduce encoding errors in its own tests. 

A browser client detects invalid UTF-8 automatically and closes the connection with a 1002 code. A non-browser client MUST treat it as a protocol error (3102). A server MUST accept either code. A server MUST close the connection with a 1002 code.

Because the frame type names the encoding, a body can arrive in a frame type whose codec does not match the encoding negotiated during the original WebSocket upgrade handshake. In this case, the receiver MUST reject such a message as a protocol error, naming the incorrect encoding. It MUST NOT fail in any way that is indistinguishable from a transport fault and MUST report the reason for failure was an invalid encoding.

### 6.2 A worked exchange

A unary `Ping` under `connectrpc.1+json`, every message a text frame:

```
client →  text    M{}
client →  text    B{"number":"7"}
client →  text    C
server →  text    M{}
server →  text    B{"number":"7"}
server →  text    S{}
server →  close 1000
```

The same call under `connectrpc.1+proto`, mixing frame types:

```
client →  text    M{}
client →  binary  B<protobuf>
client →  binary  C
server →  text    M{}
server →  binary  B<protobuf>
server →  text    S{}
server →  close 1000
```

The `M`and `S` messages are identical in both: metadata and end-of-stream are JSON regardless of the codec. A bare `C` has no payload to encode, but the frame type reflects the negotiated codec. Each side opens with its own `M` even though neither has metadata here
([§7.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)).

## 7. Metadata

Metadata reaches an application through the call-scoped object its
implementation exposes. Populating that object is part of receiving a message.
An application MUST NOT read it concurrently with a receive on the same stream.
This is a property of the Connect implementation. Connect-over-HTTP
populates the same object from inside its own receive. It is stated here
because metadata can arrive as the initial WebSocket message.

There are three mechanisms for sending metadata in Connect-over-WebSocket:
HTTP headers, query parameters, and the Leading-Metadata message.

### 7.1 Request HTTP Headers

The initial upgrade request can include headers with metadata. For browser-initiated connections, the client application is unable to add additional headers. Only headers set by the browser or proxies between the client and the server will be populated. While it is possible for standalone client applications to add their own HTTP headers to the initial upgrade request, HTTP headers SHOULD be limited to those generated by infrastructure, whether the client is a browser or a standalone application. All request headers MUST be made available to the business logic as metadata. If a non-browser client sends `Connect-Protocol-Version` or `connect-timeout-ms` as an HTTP header, the server MUST remove them from the effective headers.

The first message sent by both the client and the server MUST be a Leading-Metadata message. This message provides a way for the client and server business logic to exchange request and response headers. Browser WebSocket clients are unable to specify additional HTTP headers and WebSocket servers can add additional headers to their response, but browser client cannot read them and server handler and any interceptors have not run yet to add additional response headers.

### 7.2 Request Query Parameters

A single query parameter, `connect-timeout-ms`. This allows a browser or standalone client to specify a shorter deadline than the server provides. Details on the valid values and treatment of this query parameter are in [§11](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21).

### 7.3 Leading-Metadata Message

The message marker for a Leading-Metadata message is `M`. The body of an `M` message is a JSON object whose values are arrays of strings. As a JSON message, it MUST be sent as text. It is an error for either the client or the server to send more than one Leading-Metadata message, to send it after sending a `B`, `S`, or `C` message, or to send a `B`, `S`, or `C` message before sending an `M` message. A peer with no metadata sends `{}`; the payload is never absent, since a bare `M` is not a JSON object ([§6.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)).

A key in a Leading-Metadata message **replaces** any value the upgrade request
carried for that key, unless the key is reserved. As explained in
([§7.3.2](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)), sending a reserved key is an error that ends the stream.

The result is the RPC's *effective headers*: what the business logic and its interceptors see, and what
authentication and authorization MUST be based on.

#### 7.3.1 Rules for Keys and values in a Leading-Metadata Message

**The payload of an `M` message.** The body is a JSON object mapping header names to arrays of strings. It is always a text
frame:

```json
{"acme-tenant": ["tenant-42"], "authorization": ["Bearer ..."]}
```

Values MUST be arrays of strings, never bare strings. If there is no metadata, `{}` MUST be sent; an empty body is invalid.

**Keys MUST be valid HTTP field names**. Metadata keys are meant to be a replacement for HTTP headers and might be passed on to other services as HTTP headers.

**Values MUST be valid HTTP field values**. The values for keys whose names do not end in `-bin` MUST be valid field values without `CR`, `LF`, or `NUL`.

**Values MAY be empty.** A receiver MUST accept a key whose value is an empty array (`[]`).

**Keys are case-insensitive.** The canonical form is lower-case, matching
HTTP/2 and HTTP/3, where field names are lower-case on the wire. A sender
SHOULD emit lower-case keys; a receiver MUST compare and look up keys
case-insensitively, so that `acme-tenant` and `Acme-Tenant` are one key and not
two. JSON object keys are case-sensitive, so a receiver that skipped this step
would split one logical key in a way no HTTP implementation does.

**No duplicate keys.** A JSON object MUST NOT contain two keys that fold to the same canonical form; a key's multiple values belong in its array. While HTTP allows a field to be repeated and treats the repetitions as one comma-joined value, Connect-over-WebSocket does not allow this. There is also no `Set-Cookie` exception.

A receiver MUST treat an object that contains two keys that fold to the same canonical form, but are not identical, as a protocol error. A server MUST also treat two identical keys as a protocol error. A client SHOULD do so.

**Binary values are base64.** A key whose canonical form ends in `-bin` carries
arbitrary bytes. Its values MUST be base64-encoded with the standard
alphabet and no padding — the same encoding [§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21) specifies
for an error detail's `value`. A receiver MUST decode them and MUST treat a
value that is not valid base64 as a protocol error. While a sender MUST NOT pad base64-encoded values, a receiver MUST accept both padded and unpadded base64.

The encoding is required because the carrier is JSON, whose strings are
Unicode: a byte sequence that is not valid UTF-8 cannot be represented, so an
unencoded `-bin` value is corrupted in transit rather than rejected. Connect-over-HTTP uses the same technique to address this issue.

If any of these rules are violated, the connection MUST be closed with a protocol error code.

#### 7.3.2 Reserved header names

A key in a Leading-Metadata message sent by a client MUST NOT be any of:

1. A **forbidden request-header name** as defined by the [Fetch
standard](https://fetch.spec.whatwg.org/#forbidden-request-header): `Accept-Charset`, `Accept-Encoding`,
`Access-Control-Request-Headers`, `Access-Control-Request-Method`,
`Connection`, `Content-Length`, `Cookie`, `Cookie2`, `Date`, `DNT`,
`Expect`, `Host`, `Keep-Alive`, `Origin`, `Referer`, `Set-Cookie`, `TE`,
`Trailer`, `Transfer-Encoding`, `Upgrade`, `Via`, any name beginning
`Proxy-` or `Sec-`, and `X-HTTP-Method`, `X-HTTP-Method-Override` and
`X-Method-Override`. Fetch forbids the last three only for certain values;
this binding forbids them outright, since the method they would override has
no meaning here.
2. A name on the **server's infrastructure deny list**. This MUST default to
`Forwarded`, any `X-Forwarded-*`, and `X-Real-IP` — what a proxy in front of
the server sets, and what a client must not be able to forge. A deployment
MAY configure a different list, because which names its own infrastructure
controls is a property of that deployment.
3. A name **this protocol controls**: `Connect-Protocol-Version`
([§4.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)), as well as `connect-timeout-ms`, `Content-Type`, `Content-Encoding`, `Connect-Content-Encoding`, and `Connect-Accept-Encoding`. The `Sec-` prefix in (1) already
covers the WebSocket handshake's own headers.

A server MUST NOT allow any case-insensitive match to any of these keys.

A server MUST end the RPC with an error when a Leading-Metadata message carries
a reserved name, and MUST NOT ignore the key and continue. Ignoring it would
leave the two ends disagreeing about the effective headers with nothing on the
wire to show it: the client believes it set a value the server does not have.

**Ambient credentials and `Origin`.** The upgrade request can carry ambient
credentials — cookies, HTTP authentication, TLS client certificates — when a
page from a different origin starts the connection. A server MUST validate the upgrade request's `Origin`
([§4.4](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)).

**Middleware sees only the upgrade.** HTTP middleware in front of the server
runs once, on the handshake, and never sees a Leading-Metadata message.
Authentication and authorization for an RPC MUST therefore be performed against
the effective headers — in a Connect interceptor or in the business logic — and
not in HTTP middleware. Middleware can authorize the connection; only the RPC
layer can authorize the RPC.

### 7.4 Response metadata

Connect-over-HTTP distinguishes leading metadata (response headers, readable
before the first message) from trailing metadata (readable after the last).
WebSocket has no response header block after the handshake: the 101 response is
written before the RPC handler runs, so a server cannot know at that point what
the handler will set.

Leading response metadata is therefore carried in `M` messages,
and trailing response metadata in the `metadata` field of the `EndStreamResponse`
([§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)).

The Leading-Metadata message and the End of Server Stream message are separate namespaces. A key MAY appear in both, and carry unrelated values in each; neither shadows the other, and a receiver MUST surface both. This mirrors Connect-over-HTTP, where a header and a trailer of the same name are likewise distinct.

A receiver MUST surface Leading-Metadata as response *headers* and
`EndStreamResponse.metadata` as response *trailers*, preserving the distinction
its Connect-over-HTTP counterpart would.

**Late metadata.** The server sends `M` when the handler first sends `B` or `S` or on an explicit flush before the first `B` message or the `S` message are sent. Any headers set after `M` are considered late. Any response headers set on a server after its first `B` or `S` has already been
sent MUST be dropped by the server rather than sent. Connect-over-HTTP loses it
for the same reason: the header block has been flushed.

## 8. End of Client Stream

A client MUST terminate its request stream with exactly one `C` message unless it has already received an `S` message. It MAY carry a final body as its payload, or MAY be empty; see
[§6.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21) for which frame type each form takes.

A server MUST treat `C` as end-of-stream and MUST process its message if non-empty.

Anything the client sends afterwards MUST NOT be delivered. A message after `C` is a protocol error.

A client MUST NOT send a `B` after sending a `C`. A client API SHOULD report an attempt to do so as an error rather than accepting it, because the message will not be delivered.

A server whose connection ends before it sends `S` MUST treat the RPC as canceled by the client, whether or not it received `C`. The client can no longer receive a response, so the server MUST NOT attempt to send `S`. A server that has already sent its `S` MUST NOT treat the end of the connection as a cancellation. A `C` is not required to end a connection.

When a browser is navigated away from a page with a WebSocket connection, the browser sends a 1001 close code. This code MUST be treated by a server as a cancellation by the client.

### 8.1 Distinguishing between End of Client Stream and a dropped connection

A `C` and a connection that simply ended are different outcomes, and an implementation MUST provide a way for an RPC endpoint to distinguish between the two cases. One says the client finished sending and is waiting for an answer; the other says there could be no one left to answer.

To detect the end of the connection after `C`, a server implementation MUST continue to read from the connection until it sends `S`. After `C`, the only messages that it can receive are Close, Ping, and Pong frames; any other message is a protocol error (§8). Reads of the request stream after `C` return end-of-stream, and the end of the connection appears to the handler as a cancellation.

## 9. End of Server Stream

A server MUST terminate its response stream with exactly one `S` message. It
MUST NOT send more than one. The payload for an `S` message is a JSON `EndStreamResponse`.
This is always a text frame:

```json
{
  "error": {
    "code": "resource_exhausted",
    "message": "message size 5000000 is larger than configured max 4194304",
    "details": [{"type": "google.rpc.RetryInfo", "value": "CgIIBQ", "debug": {"retryDelay": "5s"}}]
  },
  "metadata": {"acme-trailer": ["value"]}
}
```

`error.code` MUST be one of the Connect error codes, in the wire form Connect-over-HTTP uses. A receiver MUST treat a code it does not recognize — a name
outside the set, or one a later revision adds — as `unknown`, and MUST NOT
reject the message for it. This mirrors the behavior of Connect-over-HTTP.

Both fields are omitted when empty; a close with no trailers is sent as `{}`.
An `S` with no payload at all is a protocol error. This mirrors the behavior of Connect-over-HTTP.

The trailing-metadata field is named `metadata`. `error` is absent on success,
and its `code` is the Connect wire form (`resource_exhausted`, not
`CodeResourceExhausted`).

Each entry in `details` carries `type` (a fully-qualified Protobuf message
name), `value`, and an optional `debug` object. `value` MUST be base64 with the
standard alphabet and **no padding**. `debug` is a protobuf-JSON rendering of
the same bytes, provided for human readers; a receiver MUST NOT rely on it for
any decision and MUST treat it as untrusted text, since no sender is required
to emit it and none verifies it against `value`.

The receiver MUST accept both padded an unpadded base64 for the `value` field.

A client that sees the connection close without an `S` message MUST
treat the RPC as failed.

## 10. Compression

Compression is performed by the WebSocket layer, using the
`permessage-deflate` extension ([RFC 7692](https://datatracker.ietf.org/doc/html/rfc7692)).

A server MUST require `no_context_takeover` in both directions, imposing the
parameter unilaterally under RFC 7692 §7.1.1 if the client did not offer it. A
compression context shared across messages leaks plaintext across trust
boundaries (the CRIME/BREACH family) whenever attacker-influenced and secret
data travel on one connection.

A server that cannot negotiate `permessage-deflate` with `no_context_takeover`
MUST leave compression disabled rather than fall back to a shared context.

A client MUST verify compression status. If the handshake response
negotiates `permessage-deflate` without `client_no_context_takeover` and
`server_no_context_takeover` both present, a client MUST fail the RPC and close
the connection with a 3110 code.

There is no message-level compression and no marker for it. 

A proxy can negotiate its own compression with the browser. If it uses context takeover, any attempt to establish a Connect-over-WebSockets connection will fail.

## 11. Deadlines

A client expresses a deadline as metadata passed using a `connect-timeout-ms` query parameter on the handshake URI. The value MUST be a positive integer of at most ten digits, in milliseconds.

It is OPTIONAL for a client  to send a deadline. An RPC without a deadline is bounded by
the server's own ([Server Timeout](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)), which every server SHOULD have.

The Connect-over-HTTP spec specifies that a receiver MUST reject a value that is not a base-10 integer, or that is longer than ten digits, with `invalid_argument`. As a note, the connect-go implementation does not reject non-positive values. Instead, a value of `0` or less results in a deadline that has already passed.

A server that receives an invalid `connect-timeout-ms` value reports `invalid_argument` in-band (§4.5).

There are two reasons why the deadline is specified via query string rather than using HTTP request headers Leading-Metadata. First, a browser cannot set request headers on a handshake, so the URI is its only channel. Second, a deadline carried in an `M` message cannot be applied to that initial message.

### 11.1 What the deadline bounds

The deadline bounds the **whole RPC**, not the gap between messages. It starts
when the server accepts the handshake and expires once. A stream exchanging
a message every second ends at the same instant as one that has been silent since it opened.

This is the same behavior as Connect-over-HTTP, but the Connect Protocol specification does not make this explicit.

A receiver MUST NOT extend or reset the deadline when a message
arrives. A receiver MUST NOT treat it as an inactivity timeout.

### 11.2 Server Timeout

A server SHOULD impose a deadline of its own. The effective deadline is the **shorter** of the two. A client MAY ask for less time than the server allows, never more.

Specifying a server timeout protects the server against a client
that stops participating. A server waits for the `M` message that opens a
stream ([§7.3](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)) before it dispatches the RPC; without a deadline, a peer that upgrades and then goes silent would hold that connection indefinitely.

Implementations SHOULD make the maximum configurable and SHOULD default it generously. Connect-over-WebSocket is intended for long-lived streams.

## 12. Size limits

A receiver (both client and server) MAY impose a maximum message size,
and MUST apply it to the decompressed size of a whole WebSocket message,
reassembled across continuation frames, as [§6](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21) requires. (A limit applied per
frame would be ineffective, since a sender could send multiple frames to work around it.)

A receiver with a maximum message size MUST bound both the initially
compressed bytes received as well as the uncompressed data.

A receiver that rejects a message for exceeding its limit MUST stop reading
that message rather than consume it, and MUST NOT read more of it than the
limit plus a bounded margin. Usually, one byte past the limit is enough to know the
message overran. The abandoned bytes are discarded with the
connection. A receiver MUST NOT attempt to resume the stream after rejecting
a message.

### 12.1 Reporting an oversized message

A **server** that rejects an oversized client message SHOULD send an `S`
message specifying the limit before it closes, exactly as it would for any other
protocol error ([§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)):

```json
{"error": {"code": "resource_exhausted", "message": "message exceeds the 4194304 byte limit"}}
```

A client that rejects an oversized server message has no equivalent. `S` is server-only and this binding defines no client error marker ([§5](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)), so a client's only channel is the close frame. It
SHOULD close with status 3109, whose reason SHOULD specify the limit. The reason MUST be no more than 123 UTF-8 bytes.

After a server sends the `S` message, it SHOULD send a Close frame and then stop interpreting incoming data. It MAY then close the write side of the transport. It SHOULD continue to read and discard incoming bytes, without parsing them, until the peer closes the connection or a byte limit or a time limit is reached. Only then does it close the connection. A server MUST NOT close the connection while unread data can remain before a limit is reached, because a TCP reset can destroy the `S` message and the Close frame before the peer reads them.

## 13. Connection closure

| Condition | Server sends | Client sends | Section |
| --- | --- | --- | --- |
| Normal completion | 1000 | 1000 | §9 |
| Page navigates away | — | 1001 (sent by the browser) | §8 |
| Protocol error | 1002 (after `S`) | 3102 | §5 |
| Body in the wrong frame type | 1003 (after `S`) | 3103 | §6.1 |
| Message exceeded read limit  | 1009 (after `S`) | 3109 | §12.1 |
| Compression parameters not acceptable | — | 3110 | §10 |
| Server could not marshal its `S` message | 1011 | — | §13 |
| Internal Client error | - | 3111 | §13 |
| Peer silent past the keep-alive timeout | 1011 | 3111 | §13.2 |
| Peer terminated without a closing handshake | (none) | (none) | §12 |

A browser surfaces an absent close message as code `1006`. A client MUST NOT
report `1006` as a transport failure without first checking whether an
`S` message arrived.

A receiver MUST treat a 31xx code as equivalent to the 10xx code with the same last two digits.

If there is a more specific code that applies (such as using a 1009/3109 instead of a 1002/3102), the more specific code MUST be sent.

### 13.1 Closing without a terminal message

A client MUST treat a connection that closes without `S` as a failed RPC, and MUST NOT infer success from close code `1000` ([§9](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)). The RPC did not complete, irrespective of the close code.

There is a difference between ending a WebSocket without an `S` message and ending it without a `C` message.

A server closing without sending an `S` message is always an error state. It could be caused by:

- the upgrade never completed, so there is no WebSocket and the failure is an HTTP status ([§4](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21)), which a browser client cannot read (§4.5).
- A failure in the RPC code caused it to exit without sending a message
- the peer had already closed the connection

A client MAY close without `C` due to:

- an error
- receiving an invalid message from the server ([§12.1](https://app.notion.com/p/Connect-over-WebSocket-Protocol-3e60f90884ac8096acddd799c3220bc3?pvs=21))
- or, most frequently, when the server has ended the stream first

The last case is ordinary rather than exceptional. The purpose of a `C` message is to inform
the server that the client is done sending information. If the business logic implemented
by server does not need an end-of-stream indicator, a `C` is not required.

A client MUST specify a valid close code when closing a connection. If this is due to an unexpected condition in the client, the close code MUST be 3111.

A server MUST NOT require `C` after it has sent `S` to complete an RPC, and MUST NOT report
its absence as an error once it has sent `S`.

### 13.2 Keep-alive

Many proxies and load balancers close a WebSocket that carries no traffic for
30 to 60 seconds. An endpoint MAY send Ping frames on an interval to keep such
an intermediary from closing a quiet stream. RFC 6455 already requires the
peer to answer each Ping with a Pong. A browser does this without involving the
script, and cannot send Pings of its own, so a browser client relies on the
server's Pings to keep its stream open.

An endpoint that sends Pings MAY close a connection whose peer has sent
nothing for a period of its choosing. Any frame counts: a message, a Pong, or
a Ping. A server closes such a connection with 1011, and a client with 3111.
Neither sends a terminal message, because the peer is presumed unable to read
it, so the RPC fails as described in §13.1.

An endpoint MUST NOT count time during which it has stopped reading the
connection itself, for example because its application has not taken a
message it already received. The peer's Pongs wait unread during that time,
and the silence is this endpoint's, not the peer's.

This period is not the deadline and does not replace it (§11.1). It measures
whether the peer is still there, not how long the RPC has run.

### 14. IANA Considerations

Browser clients are unable to set codes other than 1000 and values in the range 3000-4999 ([https://websockets.spec.whatwg.org/#dom-websocket-close](https://websockets.spec.whatwg.org/#dom-websocket-close)).  A browser can set values itself, but calling `close()` with an out of range value produces an `InvalidAccessError` .

The values in the range 3000-3999 MUST be registered with IANA. The codes 3102, 3103, 3109, 3110, and 3111 are referenced in this RFC. If this RFC is adopted, those values MUST be registered with IANA. See [https://www.iana.org/assignments/websocket](https://www.iana.org/assignments/websocket) for registration information. If the values cannot be registered, this RFC will move those codes to 4102, 4103, 4109, and 4110, as the 4000-4999 range does not require registration.

[connect]: https://connectrpc.com/docs/protocol/
[rfc6455]: https://datatracker.ietf.org/doc/html/rfc6455
[rfc6455-5.4]: https://datatracker.ietf.org/doc/html/rfc6455#section-5.4
[rfc7692]: https://datatracker.ietf.org/doc/html/rfc7692
[rfc8441]: https://datatracker.ietf.org/doc/html/rfc8441
[fetch-forbidden]: https://fetch.spec.whatwg.org/#forbidden-request-header
