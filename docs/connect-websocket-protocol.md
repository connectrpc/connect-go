# Connect-over-WebSocket Protocol

Status: **Draft**, unratified.
Companion document: [Decisions for WebSocket protocol support for ConnectRPC](https://app.notion.com/p/bufbuild/Decisions-for-WebSocket-protocol-support-for-ConnectRPC-3e50f90884ac80f79f2bf7a90c1183c1?source=copy_link)
records the rationale behind decisions specified in this document. This document is the normative wire format that all implementations should be written against.

## 1. Conventions

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are to be
interpreted as described in RFC 2119.

_Sender_ and _receiver_ denote the two ends of one RPC. _Client_ is the peer
that initiated the WebSocket handshake; _server_ is the peer that accepted it.

Byte values are written in hexadecimal, and Unicode scalar values in the usual
`U+XXXX` form.

## 2. Scope

This document specifies how the [Connect protocol][connect] is carried over a
WebSocket connection ([RFC 6455][rfc6455]). It covers connection
establishment, message framing, metadata, deadlines, compression, and
termination.

It does not specify service definitions, codec behavior, or error semantics
beyond their wire encoding; those are unchanged from Connect.

## 3. Relationship to the Connect protocol

### 3.1 Normative divergences

A Connect-over-WebSocket message stream is **not** a conforming Connect
streaming body. It keeps Connect's `EndStreamResponse` schema and its error
Model. It departs from Connect in the following ways:

| #   | Divergence                                                    | Reason                                                                                                                                                                       |
| --- | ------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Messages carry a marker, not a flags byte and length          | Each frame in a WebSocket message includes a length field, so a user-specified length only restates it with a possibly erroneous value. ([§5](#5-message-markers))           |
| 2   | The frame type specifies the message encoding                 | The WebSocket protocol includes a mechanism for specifying the message type. Most messages are JSON, but binary Protobufs can be sent as well. ([§6.1](#61-text-and-binary)) |
| 3   | Client end-of-stream is an in-band message                    | WebSocket has no half-close, so request EOF cannot be a transport event                                                                                                      |
| 4   | Request metadata is an in-band message                        | The browser WebSocket API cannot set headers on a handshake                                                                                                                  |
| 5   | No `Streaming-Content-Encoding`, no message-level compression | The WebSocket layer compresses whole messages ([§10](#10-compression))                                                                                                       |
| 6   | A deadline travels only as a query parameter                  | Browsers cannot set request headers, and metadata cannot bound its own read                                                                                                  |
| 7   | One connection carries exactly one RPC                        | The procedure is selected by the handshake URI                                                                                                                               |
| 8   | The handshake requires HTTP/1.1                               | RFC 6455 defines it there; RFC 8441 is not adopted ([§4.1](#41-http-version))                                                                                                |
| 9   | A client's first message is always metadata                   | Consumed before dispatch, so request metadata is complete for interceptors ([§7.3](#73-leading-metadata-messages))                                                           |
| 10  | A deadline SHOULD be specified                                | A server waits for the opening message, so an unbounded RPC could park a connection ([§11](#11-deadlines))                                                                   |

Sending an HTTP request with a `Connection: Upgrade` header, an `Upgrade: websocket` header, and a `Sec-WebSocket-Protocol` header that specifies one or more of `connectrpc.1`, `connectrpc.1+proto`, or `connectrpc.1+json` ([§4.3](#43-subprotocol-negotiation)) indicates that a peer is agreeing to use ConnectRPC over WebSockets instead of standard ConnectRPC streaming.

### 3.2 What this binding does not borrow

The envelope header used for ConnectRPC streaming (header byte plus length) is not used for ConnectRPC over WebSockets.

### 3.3 What this binding does borrow

The `EndStreamResponse` schema ([§9](#9-end-of-response)) and the error model are both identical on the wire to Connect over
HTTP. That sharing is deliberate: an implementation of one can decode the other’s terminal message unchanged, and the two should not drift.

## 4. Connection establishment

### 4.1 HTTP version

The handshake MUST be an HTTP/1.1 request. [RFC 6455][rfc6455] defines it over
HTTP/1.1, and this document does not adopt [RFC 8441][rfc8441]'s Extended
CONNECT, which carries WebSocket over HTTP/2.

A **client** whose HTTP stack uses HTTP/2 MUST ensure the handshake goes over
HTTP/1.1. This is usually automatic: a client library that
recognizes `Connection: Upgrade` with `Upgrade: websocket` routes that request
to an HTTP/1.1 connection. 

A **server** MUST offer HTTP/1.1 on the listener that accepts handshakes. It
MAY offer HTTP/2 as well. Only the handshake requires HTTP/1.1, so RPCs that a client routes over plain HTTP still negotiate HTTP/2 on the same connection pool.

A server that offers only HTTP/2 — including h2c — cannot accept a handshake,
because there is no HTTP/1.1 connection to take over. An implementation SHOULD
report that as a server configuration fault rather than a client error.

A server that does not implement RFC 8441 MUST refuse an Extended `CONNECT`
request rather than answer it as an ordinary RPC. This holds whether or not a
later revision of this document adopts RFC 8441: a peer sending one is asking
for a handshake, and answering with anything else leaves it waiting for a
WebSocket that will never arrive.

### 4.2 Handshake URI

The RPC's procedure is the path of the handshake URI. A client MUST form it by
joining the base URL's path with the procedure path:

```
wss://api.example.com/connect.ping.v1.PingService/CumSum
```

A trailing `/` on the base path MUST NOT produce a doubled separator. The
scheme MUST be `ws` or `wss`, corresponding to the `http` or `https` scheme the
same service would be reached at over Connect HTTP.

One connection carries exactly one RPC. A peer MUST NOT begin a second RPC on a
connection.

By default, any registered ConnectRPC endpoint can accept either ConnectRPC over HTTP or ConnectRPC over WebSockets. If the `Connection: Upgrade` and `Upgrade: websocket` headers are present on the HTTP request, ConnectRPC over WebSockets MUST be used. Otherwise, ConnectRPC over HTTP MUST be used.

#### 4.2.1 Path prefix
A deployment MAY place every WebSocket path under a common
prefix, so that an upgrade is distinguishable from an ordinary RPC by URL
alone. Some load balancers need that to route WebSocket traffic differently:
sticky backends, longer idle timeouts, upgrade support enabled.

Both peers MUST agree on the value. A client forms the handshake URI as
base + prefix + procedure, and plain HTTP RPCs keep the bare procedure paths.

When a prefix is configured:

- A server MUST answer a non-upgrade request under the prefix with
  `426 Upgrade Required`.
- A server MUST NOT accept an upgrade at the bare procedure paths. 

### 4.3 Subprotocol negotiation

The codec is selected by the WebSocket subprotocol.

| Token              | Codec                        |
| ------------------ | ---------------------------- |
| `connectrpc.1`       | Protobuf JSON (base token) |
| `connectrpc.1+proto` | Protobuf binary              |
| `connectrpc.1+json`  | Protobuf JSON                |

A client MUST offer at least one of these tokens in `Sec-WebSocket-Protocol`,
and the token it offers MUST correspond to the codec it will encode with. A
client MAY offer several, in descending order of preference.

A server MAY support only some of these codecs. A server MUST select the first
offered token that it both recognizes and can serve, skipping any whose codec
it does not support, and MUST echo exactly that token in its response. Skipping
rather than failing on the first recognized token is what lets a client offer
`connectrpc.1+json, connectrpc.1+proto` and reach a server that speaks only
Protobuf.

A server MUST fail the handshake when it can serve none of the offered tokens,
and the status distinguishes the two reasons:

| Condition                                         | Status                       |
| ------------------------------------------------- | ---------------------------- |
| No offered token is one of the three above         | `400 Bad Request`            |
| A token is recognized, but its codec is unsupported | `415 Unsupported Media Type` |

The distinction is worth making because the remedies differ. A `400` says the
client is not speaking this protocol; a `415` says it is, and must offer
another encoding. A server SHOULD name the encodings it does support in the
`415` body.

A client MUST verify that the echoed token is one it offered, and MUST close
the connection if it is not. 

`Connect-Protocol-Version` is implied by the subprotocol and MUST NOT be sent
as a header.

### 4.4 Origin

A server MUST reject a cross-origin handshake with HTTP `403 Forbidden` unless
it has been configured to permit one. A handshake carrying no `Origin` header
is not from a browser and MUST NOT be rejected on origin grounds.

**Same-origin means the same host.** A server MUST compare the `Origin`
header's host — its host and port — against the request's own `Host`,
case-insensitively, and MUST NOT require the schemes to match. The scheme is
left out deliberately: a deployment terminating TLS at a proxy sees `https` in
the `Origin` and its own plaintext `Host`, and a comparison including the
scheme would reject its own pages.

A browser attaches the user's ambient credentials to a WebSocket handshake and
performs no CORS preflight, so an accept-all default would let any origin open
an authenticated stream.

## 5. Message markers

Every message begins with one marker: a single Unicode scalar value, UTF-8
encoded, saying what the rest of the message is. The payload follows it
immediately, with nothing in between.

| Marker | Name              | Direction       | Payload                                                       |
| ------ | ----------------- | --------------- | ------------------------------------------------------------- |
| `B`    | Body              | either          | an RPC message                                                |
| `M`    | Leading-Metadata  | either          | JSON metadata ([§7.3](#73-leading-metadata-messages))         |
| `S`    | Server-End-Stream | server → client | `EndStreamResponse` JSON ([§9](#9-end-of-response))           |
| `C`    | Client-End-Stream | client → server | a final body, or nothing ([§8](#8-end-of-the-request-stream)) |

A receiver MUST treat a message whose marker it does not recognize as a
protocol error, and MUST NOT guess at the payload. A receiver MUST treat a
marker valid only in the opposite direction as a protocol error: `S` from a
client, `C` from a server.

A message that is empty — no marker at all — is a protocol error.

**A protocol error ends the stream.** Wherever this document says a receiver
MUST treat something as a protocol error, that receiver MUST end the stream: a
server by sending an `S` message carrying the error and then closing
([§9](#9-end-of-response)), a client by failing the RPC and closing. A receiver
MUST NOT skip the offending message and carry on.

This applies to an unknown marker in particular, which [§5.1](#51-the-marker-space)
might otherwise seem to invite a receiver to ignore for forward compatibility.
It does not: a marker is defined before it is used, so one that arrives
unrecognized means the peer believes it is speaking a dialect this receiver
does not have, and continuing would silently misread whatever follows.

### 5.1 The marker space

The four markers above are ASCII, one byte each. They are printable
deliberately: a text frame then reads as its marker followed by its JSON in any
debugger that shows WebSocket traffic, which is where a browser client is
diagnosed.

Future markers MUST be Unicode scalar values in the Basic Multilingual Plane —
`U+0000`–`U+FFFF`, excluding the surrogate range `U+D800`–`U+DFFF`, which is
not encodable in UTF-8. That is 63,488 values, against four in use.

A receiver MUST reject a marker outside the BMP. UTF-8's leading byte gives the
length, so `0xF0`–`0xF7` is a four-byte sequence and therefore out of range on
sight, before any decoding.

The bound exists for the benefit of clients written in languages whose strings
are UTF-16. A BMP scalar is one UTF-16 code unit, so stripping the marker is an
index-1 slice; an astral one is a surrogate pair, and the same slice would
leave a lone low surrogate glued to the payload. Allowing four-byte markers
would make the obvious client implementation silently wrong, and only for
markers that do not exist yet.

New markers SHOULD be printable, for the same reason the first four are.

## 6. Message framing

Each WebSocket message MUST contain exactly one marker and its payload. A
receiver MUST NOT carry decoding state from one message into the next.

The unit is a WebSocket _message_, not a frame. A sender MAY fragment one
across continuation frames ([RFC 6455 §5.4][rfc6455-5.4]) and a receiver MUST
reassemble before interpreting the marker: a marker is not a per-frame tag, and
a fragment is not a message. Fragmentation is otherwise invisible to this
binding, which never requires or forbids it.

### 6.1 Text and binary

The WebSocket frame type says how the payload is encoded:

| Frame type | Payload encoding |
| ---------- | ---------------- |
| text       | JSON             |
| binary     | Protobuf binary  |

A message MUST be sent as a binary frame if and only if its payload is Protobuf
binary; every other message MUST be a text frame. Stated per marker:

- `M` and `S` are always JSON, so always text.
- `B` follows the codec the subprotocol selected
  ([§4.3](#43-subprotocol-negotiation)): binary for Protobuf, text for JSON.
- `C` with a final body follows the codec as `B` does. `C` alone carries no
  payload and MUST be text.
- `B` alone — the marker with no payload — is the empty Protobuf message and
  MUST be binary. In JSON an empty message is `{}`, never zero bytes, so a text
  frame containing only `B` is a protocol error.
- `M` and `S` alone are protocol errors for the same reason. Their payloads are
  JSON objects, and the empty object is `{}`. See
  [§7.3](#73-leading-metadata-messages) and [§9](#9-end-of-response).

A connection therefore mixes frame types when the codec is Protobuf: bodies
arrive binary while metadata and end-of-stream arrive text. That is intended.
The frame type is a type tag the transport supplies for free, and a receiver
knows how to parse a payload before it has looked at anything but the frame.

A text frame MUST contain valid UTF-8, which [RFC 6455][rfc6455] §8.1 requires
of every text frame and which a browser enforces. Since the marker is UTF-8 and
JSON is UTF-8, a conforming message satisfies this by construction. 

An implementation SHOULD verify the contents of a message. A peer that emits
invalid UTF-8 in a text frame has its connection closed by a browser, and a
library that does not validate will not reproduce encoding errors in its own tests.

Because the frame type names the encoding, a body can arrive in a frame type
whose codec the receiver does not support — a peer that ignored the negotiated
subprotocol, since [§4.3](#43-subprotocol-negotiation) only ever selects a
token the server can serve. A receiver MUST reject such a message as a protocol
error naming the unsupported encoding. It MUST NOT fail in any way that is
indistinguishable from a transport fault, which leaves the peer to guess at a
configuration it could have been told.

### 6.2 A worked exchange

A unary `Ping` under `connectrpc.1+json`, every message a text frame:

```
client →  text    M{}
client →  text    B{"number":"7"}
client →  text    C
server →  text    B{"number":"7"}
server →  text    S{}
server →  close 1000
```

The same call under `connectrpc.1+proto`, mixing frame types:

```
client →  text    M{}
client →  binary  B<protobuf>
client →  text    C
server →  binary  B<protobuf>
server →  text    S{}
server →  close 1000
```

The `M`, `C` and `S` messages are identical in both: metadata and end-of-stream
are JSON regardless of the codec, and a bare `C` has no payload to encode.

## 7. Metadata

Metadata reaches an application through whatever call-scoped object its
implementation exposes. Populating that object is part of receiving a message,
so unless the implementation documents otherwise, an application MUST NOT read
it concurrently with a receive on the same stream. This is a property of the
Connect implementation rather than of this binding — Connect over HTTP
populates the same object from inside its own receive — but it is stated here
because the rules below give metadata more than one arrival point, which makes
the hazard easier to reach.

### 7.0 Keys and values

Metadata is a mapping from a key to an ordered list of values, the same model
Connect uses over HTTP. In an `M` message and in `EndStreamResponse.metadata`
it is a JSON object whose values are arrays of strings.

**Keys are case-insensitive.** The canonical form is lower-case, matching
HTTP/2 and HTTP/3, where field names are lower-case on the wire. A sender
SHOULD emit lower-case keys; a receiver MUST compare and look up keys
case-insensitively, so that `acme-tenant` and `Acme-Tenant` are one key and not
two. JSON object keys are case-sensitive, so a receiver that skipped this step
would split one logical key in a way no HTTP implementation does.

**No duplicate keys.** A JSON object MUST NOT contain two keys that fold to the
same canonical form; a key's multiple values belong in its array. HTTP allows a
field to be repeated and treats the repetitions as one comma-joined value, but
that exists for a wire format with no arrays, and this one has them. There is
no `Set-Cookie`-shaped exception here.

A receiver is not required to detect a duplicate. JSON parsers differ in which
of two same-named members survives, and most expose no way to see that there
were two — so a sender that emits duplicates has produced metadata whose
content is undefined, not metadata a receiver is obliged to reject.

**Binary values are base64.** A key whose canonical form ends in `-bin` carries
arbitrary bytes, and its values MUST be base64-encoded with the standard
alphabet and no padding — the same encoding [§9](#9-end-of-response) specifies
for an error detail's `value`. A receiver MUST decode them and MUST treat a
value that is not valid base64 as a protocol error.

The encoding is required because the carrier is JSON, whose strings are
Unicode: a byte sequence that is not valid UTF-8 cannot be represented, so an
unencoded `-bin` value is corrupted in transit rather than rejected. Connect
over HTTP faces the same problem in HTTP field values and answers it the same
way.

### 7.1 Request metadata

A client sends request metadata in the Leading-Metadata message that opens
every stream ([§7.3](#73-leading-metadata-messages)). That is the only channel
a client may rely on: the browser WebSocket API cannot set request headers, so
supporting header-borne metadata would serve no client this binding targets.

A client that *can* set handshake headers SHOULD NOT put its metadata there as
well. The two would be the same keys on two channels, and the precedence below
resolves that in the handshake's favour — so a client doing both would shadow
its own `M` message with its own headers, and the `M` copy would be dead
weight until the day the two disagreed. The handshake's headers describe the
connection; the `M` message describes the RPC.

A server MUST still surface the handshake's HTTP request headers as request
metadata. A browser cannot set them, but it sends `Cookie`, `Accept-Language`
and `User-Agent` automatically, and infrastructure in front of the server adds
its own — `X-Forwarded-For`, the identity headers an authenticating proxy
injects, tracing context. Discarding them would break authentication schemes
the client has no way to reproduce in an `M` message.

**Precedence.** Where a key appears both on the handshake and in an `M`
message, **the handshake wins**. A client can therefore add metadata but never overwrite
what arrived with the connection. This is a security property, not a
convenience: a proxy is in the path of the handshake and can overwrite a header
there, but it never sees the `M`, so the opposite precedence would let any
client forge `X-Forwarded-For` or an injected identity.

One consequence worth stating: the headers a browser sets automatically are
effectively reserved. A client cannot override `Accept-Language` from an
`M`, and the attempt is ignored rather than refused.

### 7.2 Response metadata

Connect over HTTP distinguishes leading metadata (response headers, readable
before the first message) from trailing metadata (readable after the last).
WebSocket has no response header block after the handshake: the 101 response is
written before the RPC handler runs, so a server cannot know at that point what
the handler will set.

Leading response metadata is therefore carried in `M` messages,
and trailing response metadata in the `metadata` field of the `EndStreamResponse`
([§9](#9-end-of-response)).

The two are separate namespaces, not one split across two places. A key may
appear in both, and carry unrelated values in each; neither shadows the other,
and a receiver MUST surface both. This mirrors Connect over HTTP, where a
header and a trailer of the same name are likewise distinct.

A receiver MUST surface Leading-Metadata as response _headers_ and
`EndStreamResponse.metadata` as response _trailers_, preserving the distinction
its Connect HTTP counterpart would.

### 7.3 Leading-Metadata messages

An `M` message carries metadata rather than an RPC message. Its payload is a
JSON object mapping header names to arrays of strings, so it is always a text
frame:

```json
{"Acme-Tenant": ["tenant-42"], "Authorization": ["Bearer ..."]}
```

Values MUST be arrays of strings, never bare strings. An empty payload is
permitted and carries no metadata.

Keys are case-insensitive. A receiver MUST canonicalize them, so a sender need
not.

**The client's `M` message opens the stream.** A client MUST send exactly one
`M` message, and it MUST be the first message on the stream — `{}` when the
client has no metadata. The payload is a JSON object, so an `M` with no payload
at all is a protocol error ([§6.1](#61-text-and-binary)). A server MUST treat a first
message that is anything else, or a second `M` message at any
point, as a protocol error and end the stream.

Both halves of that rule are load-bearing, and neither is stylistic:

- A server consumes this message _before_ dispatching the RPC, so that request
  metadata is complete when interceptors run — an authenticating interceptor
  runs before the handler and must see the client's credentials. Consuming it
  requires knowing when it has arrived, and a server that kept reading while
  frames were metadata could only learn there were no more by blocking for a
  frame the client may never send. A client that opens a stream and waits for
  the server to push would hang forever.
- A later `M` would be merged while the handler is already running, so a
  handler reading its own request metadata from a second goroutine — an
  ordinary shape for a bidirectional RPC — would race that write.

Because the message is mandatory, a server may wait for it, and the deadline
in [§11](#11-deadlines) bounds that wait against a peer that never sends it.

**Server metadata.** A server MAY send more than one `M`, and MUST send them
all before its first `B`. A client merges them in order, each key
replacing any earlier value. Replacement, not concatenation, is the rule
throughout; see [§7.1](#71-request-metadata) for how an `M` composes with
the handshake headers on the request side.

**Server flush point.** A server that has leading metadata to send MUST emit it
before its first `B`, or before the `S` message if it sends no `B` at all. A stream that fails without producing a message is
precisely the case where the metadata is most wanted, so it MUST NOT be lost
there.

**Late metadata.** Metadata a server sets after its first `B` has already been
sent MAY be dropped by the server rather than sent. Connect over HTTP loses it
for the same reason: the header block has been flushed.

Dropping it is the server's only option, not a choice between sending late and
not sending. A server MUST NOT send an `M` after its first `B`, and a client
MUST treat one that arrives as a protocol error and end the stream
([§5](#5-message-markers)). A client that instead discarded it silently would
leave the two ends disagreeing about what the metadata is, with nothing on the
wire to show it: the server's API accepted the write, and the client's
application never sees the key. Failing makes the mistake visible at the point
it is made, and it is the server's mistake to fix.

## 8. End of the request stream

A client MUST terminate its request stream with exactly one `C` message. It MAY
carry a final body as its payload, or MAY be empty; see
[§6.1](#61-text-and-binary) for which frame type each form takes.

A server MUST treat `C` as end-of-stream and MUST deliver its payload as a
message if non-empty.

Anything the client sends afterwards MUST NOT be delivered, and a server SHOULD
read and discard it until the stream ends rather than leaving it unread. The
two are not the same: a receiver that simply stops reading lets its socket
buffer fill, and a peer that keeps sending then blocks in its own write —
turning the client's protocol mistake into a stall on the client, at a point
where the server has already decided to ignore it. Discarding costs a read per
message, and each is still bounded by the receiver's size limit
([§12](#12-size-limits)) and by the deadline
([§11.1](#111-what-the-deadline-bounds)).

A client MUST NOT send a `B` after it. A client API SHOULD report an
attempt to do so as an error rather than accepting it, because the message will
not be delivered.

A server whose stream is still open when the connection ends, and which never
saw `C`, MUST treat the RPC as canceled by the client. A server that has
already sent its `S` MUST NOT: it ended the stream itself, and a `C` that never
arrived is the ordinary consequence of that, not a cancellation
([§13.1](#131-closing-without-a-terminal-message)).

## 9. End of response

A server MUST terminate its response stream with exactly one `S` message, and
MUST NOT send more than one. Its payload is a JSON `EndStreamResponse`, so it
is always a text frame:

```json
{
  "error": {
    "code": "resource_exhausted",
    "message": "message size 5000000 is larger than configured max 4194304",
    "details": [{"type": "google.rpc.RetryInfo", "value": "CgIIBQ", "debug": {}}]
  },
  "metadata": {"Acme-Trailer": ["value"]}
}
```

`error.code` MUST be one of the Connect error codes, in the wire form Connect
over HTTP uses. A receiver MUST treat a code it does not recognize — a name
outside the set, or one a later revision adds — as `unknown`, and MUST NOT
reject the message for it: the error is still the RPC's verdict, and the
message and details still say what happened. Connect over HTTP is lenient in
the same way.

Both fields are omitted when empty; a clean finish with no trailers is `{}`.
An `S` with no payload at all is a protocol error. Connect's HTTP streaming
protocol parses the end-of-stream payload as JSON unconditionally, so a
zero-length one fails there too; this binding changes the framing around that
message, not the message.
The trailing-metadata field is named `metadata`. `error` is absent on success,
and its `code` is the Connect wire form (`resource_exhausted`, not
`CodeResourceExhausted`).

Each entry in `details` carries `type` (a fully-qualified Protobuf message
name), `value`, and an optional `debug` object. `value` MUST be base64 with the
standard alphabet and **no padding**. `debug` is a protobuf-JSON rendering of
the same bytes, provided for human readers; a receiver MUST NOT rely on it for
any decision and MUST treat it as untrusted text, since no sender is required
to emit it and none verifies it against `value`.

A client that sees the connection close without an `S` message MUST
treat the RPC as failed; the RPC did not complete.

## 10. Compression

Compression is performed by the WebSocket layer, using the
`permessage-deflate` extension ([RFC 7692][rfc7692]).

A server MUST require `no_context_takeover` in both directions, imposing the
parameter unilaterally under RFC 7692 §7.1.1 if the client did not offer it. A
compression context shared across messages leaks plaintext across trust
boundaries (the CRIME/BREACH family) whenever attacker-influenced and secret
data travel on one connection.

A server that cannot negotiate `permessage-deflate` with `no_context_takeover`
MUST leave compression disabled rather than fall back to a shared context.

A client MUST verify this rather than trust it. If the handshake response
negotiates `permessage-deflate` without `client_no_context_takeover` and
`server_no_context_takeover` both present, a client MUST fail the RPC and close
the connection. The requirement above binds the server, but the plaintext a
shared context leaks is the client's as much as the server's, and a client that
took the server's word for it would be protected only as far as its peer is
conforming. A response negotiating no extension at all is fine: nothing is
compressed, so nothing leaks.

There is no message-level compression and no marker for it.

## 11. Deadlines

A client expresses a deadline as a `connect-timeout-ms` query parameter on the
handshake URI. A sender MUST make the value
a positive integer of at most ten digits, in milliseconds.

A client is not required to send one. An RPC without a deadline is bounded by
the server's own ([Server Timeout](#server-timeout)), which every server is
expected to have, so the absence costs the server nothing to handle.

A receiver MUST reject a value that is not a base-10 integer, or that is longer
than ten characters, with `invalid_argument`. It is not required to reject a
non-positive value; Connect over HTTP does not. A value of `0` or less results in a deadline that has already passed.

The query string rather than metadata, for two reasons. A browser cannot set
request headers on a handshake, so the URI is its only channel; and a deadline
carried in an `M` message could not bound the read of that message, which is the
first thing the deadline has to cover.

### 11.1 What the deadline bounds

The deadline bounds the **whole RPC**, not the gap between messages. It starts
when the server accepts the handshake and expires once, whatever arrives in the
meantime: a stream exchanging a message every second ends at the same instant
as one that has been silent since it opened.

This is stated because the Connect protocol does not, and the ambiguity is
reachable. `Connect-Timeout-Ms` over HTTP is a whole-call deadline — a
conforming implementation computes it once, before the stream exists, and never
re-arms it — but an implementation reading only the prose could build a
per-message idle timer and believe it conformed. Two peers that disagreed would
part ways on every stream outliving the value, and only on those, which is the
worst shape a disagreement can take.

A receiver MUST NOT extend, reset, or re-arm the deadline when a message
arrives. A receiver MUST NOT treat it as an inactivity timeout.

One consequence is worth facing rather than leaving implicit: a single value
cannot both bound a silent peer tightly and let a legitimate subscription run
for days. The [server timeout](#server-timeout) below exists for the first, and
its guidance to default generously concedes the second. An implementation that
wants both needs a second, separate bound on inter-message silence; this
document does not define one, and a peer MUST NOT infer one from the deadline.

### Server Timeout
A server SHOULD impose a deadline of its own. The effective deadline is the **shorter** of the two. A client may ask for less time than the server allows, never more.

Specifying a server timeout protects the server against a client
that stops participating. A server waits for the `M` message that opens a
stream ([§7.3](#73-leading-metadata-messages)) before it dispatches the RPC; without a deadline, a peer that upgrades and then goes silent would hold that connection indefinitely.

Implementations SHOULD make the maximum configurable and SHOULD default it
generously — long-lived streams are the reason this binding exists, and a short
default would sever the subscriptions it is meant to carry.

## 12. Size limits

A receiver MAY impose a maximum message size, and MUST apply it to the
decompressed size of a whole WebSocket message — reassembled across
continuation frames, as [§6](#6-message-framing) requires. A limit applied per
frame would be no limit at all, since a sender could fragment past it.

A receiver that also wishes to bound memory against a compression bomb MUST
additionally bound the compressed bytes it reads.

A receiver that rejects a message for exceeding its limit MUST stop reading
that message rather than consume it, and MUST NOT read more of it than the
limit plus a bounded margin. One byte past the limit is enough to know the
message overran; nothing is gained by reading the rest, and reading the rest is
what the limit exists to prevent. The abandoned bytes are discarded with the
connection — a receiver MUST NOT attempt to resume the stream after rejecting
a message.

### 12.1 Reporting an oversized message

A **server** that rejects an oversized client message SHOULD send an `S`
message naming the limit before it closes, exactly as it would for any other
protocol error ([§9](#9-end-of-response)):

```json
{"error": {"code": "resource_exhausted", "message": "message exceeds the 4194304 byte limit"}}
```

A **client** that rejects an oversized server message has no equivalent. `S` is
server-only and this binding defines no client error marker
([§5](#5-message-markers)), so a client's only channel is the close frame. It
SHOULD close with status `1009`, whose reason SHOULD name the limit.

When reporting an oversized message or other protocol error, a receiver SHOULD close the TCP connection without a WebSocket closing handshake after sending the `S` message. The closing handshake requires draining whatever the peer has already queued, which would result in processing the entire oversized or invalid message.

## 13. Connection closure

| Close code | Meaning                                                                                                       |
| ---------- | ------------------------------------------------------------------------------------------------------------- |
| `1000`     | Normal completion; the `S` message carries the verdict                                                        |
| `1009`     | A message exceeded a read limit; a client's only way to say so ([§12.1](#121-reporting-an-oversized-message)) |
| `1011`     | The sender could not marshal its own `S` message                                                              |
| _(none)_   | The peer terminated without a closing handshake ([§12](#12-size-limits))                                      |

A browser surfaces an absent close message as code `1006`. A client MUST NOT
report `1006` as a transport failure without first checking whether an
`S` message arrived.

### 13.1 Closing without a terminal message

Either terminal message can be absent. They are not equally load-bearing, and
a peer MUST NOT treat the two absences alike.

**A server may close without `S`** when:

- the upgrade never completed, so there is no WebSocket and the failure is an
  HTTP status ([§4](#4-connection-establishment));
- it could not produce a verdict at all, because the code answering the RPC
  died in a way the transport cannot render as an error;
- writing `S` failed, in which case it SHOULD close with `1011`
  ([§13](#13-connection-closure)); or
- the peer had already gone, so the `S` was written to a connection nobody
  would read.

A client MUST treat a connection that closes without `S` as a failed RPC, and
MUST NOT infer success from close code `1000` ([§9](#9-end-of-response)). The
RPC did not complete, whatever the close code says.

**A client may close without `C`** when it cancels, when it rejected the
server's framing and hung up ([§12.1](#121-reporting-an-oversized-message)),
or — most often — when the server ended the stream first. That last case is
ordinary rather than exceptional: once a server has sent `S` and closed, a
client's `C` has nowhere to go, and a client that half-closes only at the end
of an RPC will frequently never deliver one.

A server therefore MUST NOT require `C` to complete an RPC, and MUST NOT report
its absence as an error once it has sent `S`. `C` bounds the request stream
while the server is still reading it; after that it carries no information.

The asymmetry is not incidental. `S` is the RPC's verdict and nothing else
conveys it, so its absence is always a failure. `C` is an end-of-input signal
standing in for a half-close WebSocket does not have, so its absence matters
only to a reader still waiting on input.

[connect]: https://connectrpc.com/docs/protocol/
[rfc6455]: https://datatracker.ietf.org/doc/html/rfc6455
[rfc6455-5.4]: https://datatracker.ietf.org/doc/html/rfc6455#section-5.4
[rfc7692]: https://datatracker.ietf.org/doc/html/rfc7692
[rfc8441]: https://datatracker.ietf.org/doc/html/rfc8441
