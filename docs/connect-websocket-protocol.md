# Connect-over-WebSocket Protocol

Status: **Draft**, unratified.
Companion document: [websocket-transport-design.md](websocket-transport-design.md)
records the rationale, the trade-offs, and the Go implementation's structure.
This document is the normative wire format a second implementation is written
against.

## 1. Conventions

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are to be
interpreted as described in RFC 2119.

*Sender* and *receiver* denote the two ends of one RPC. *Client* is the peer
that initiated the WebSocket handshake; *server* is the peer that accepted it.

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
model, and departs from Connect in the following ways:

| #   | Divergence                                   | Reason                                                                        |
| --- | -------------------------------------------- | ----------------------------------------------------------------------------- |
| 1   | Messages carry a marker, not a flags byte and length | The frame delimits the message, so a length would only restate it ([§5](#5-message-markers)) |
| 2   | The frame type selects the payload encoding  | Text carries JSON so a browser can read and debug it directly ([§6.1](#61-text-and-binary)) |
| 3   | Client end-of-stream is an in-band message   | WebSocket has no half-close, so request EOF cannot be a transport event       |
| 4   | Request metadata is an in-band message       | The browser WebSocket API cannot set headers on a handshake                   |
| 5   | No `Streaming-Content-Encoding`, no message-level compression | The WebSocket layer compresses whole messages ([§10](#10-compression))  |
| 6   | A deadline travels only as a query parameter | Browsers cannot set request headers, and metadata cannot bound its own read   |
| 7   | One connection carries exactly one RPC       | The procedure is selected by the handshake URI                                |
| 8   | The handshake requires HTTP/1.1              | RFC 6455 defines it there; RFC 8441 is not adopted ([§4.1](#41-http-version)) |
| 9   | A client's first message is always metadata  | Consumed before dispatch, so request metadata is complete for interceptors ([§7.3](#73-leading-metadata-messages)) |
| 10  | A deadline is mandatory, not optional        | A server waits for the opening message, so an unbounded RPC could park a connection ([§11](#11-deadlines)) |

The subprotocol token ([§4.3](#43-subprotocol-negotiation)) is what keeps this
honest: a peer that negotiates `connect.v2` is agreeing to this dialect, not to
the HTTP one.

### 3.2 What this binding does not borrow

An earlier draft of this document took two bits from the six that Connect
reserves in its flags byte "for future protocol extensions", and had to warn
that the allocation had never been granted — that a later assignment by Connect
would make every peer here non-conforming, silently, because a conforming
reader would interpret those bits as whatever the newer specification said.

That problem is gone rather than mitigated. This binding no longer uses
Connect's flags byte at all, so there is nothing in Connect's reserved range
for a future assignment to collide with. The marker space in
[§5.1](#51-the-marker-space) is this document's own, and extending it is this
document's to do.

What remains shared is the `EndStreamResponse` schema ([§9](#9-end-of-response))
and the error model, both of which are identical on the wire to Connect over
HTTP. That sharing is deliberate: an implementation of one can decode the other's
terminal message unchanged, and the two should not drift.

## 4. Connection establishment

### 4.1 HTTP version

The handshake MUST be an HTTP/1.1 request. [RFC 6455][rfc6455] defines it over
HTTP/1.1, and this document does not adopt [RFC 8441][rfc8441]'s Extended
CONNECT, which carries WebSocket over HTTP/2 — no implementation of this
specification supports it.

A **client** whose HTTP stack uses HTTP/2 MUST ensure the handshake goes over 
HTTP/1.1. This is usually automatic: a client library that
recognizes `Connection: Upgrade` with `Upgrade: websocket` routes that request
to an HTTP/1.1 connection. For example, Go's `net/http` does exactly this — see
`Request.requiresHTTP1` — and a browser's WebSocket API does it inherently.

A **server** MUST offer HTTP/1.1 on the listener that accepts handshakes. It
MAY offer HTTP/2 as well. Only the handshake
requires HTTP/1.1, so RPCs that a client routes over plain HTTP still negotiate
HTTP/2 on the same connection pool. 

A server that offers only HTTP/2 — including h2c — cannot accept a handshake,
because there is no HTTP/1.1 connection to take over. An implementation SHOULD
report that as a server configuration fault rather than a client error. For example,
in Go the `http.ResponseWriter` does not implement `http.Hijacker`, and the handler
answers `500` naming the requirement.

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

**Path prefix.** A deployment MAY place every WebSocket path under a common
prefix, so that an upgrade is distinguishable from an ordinary RPC by URL
alone. Some load balancers need that to route WebSocket traffic differently —
sticky backends, longer idle timeouts, upgrade support enabled.

Both ends MUST agree on the value. A client forms the handshake URI as
base + prefix + procedure, and plain HTTP RPCs keep the bare procedure paths.

Where a prefix is configured:

- A server MUST answer a non-upgrade request under the prefix with
  `426 Upgrade Required`.
- A server MUST NOT accept an upgrade at the bare procedure paths. Both halves
  are needed for the split to mean anything: a load balancer can only route on
  the prefix if WebSocket traffic never appears without it, and never appears
  under it without being an upgrade.

### 4.3 Subprotocol negotiation

The codec is selected by the WebSocket subprotocol, because a browser cannot
set arbitrary headers on the handshake.

| Token              | Codec                        |
| ------------------ | ---------------------------- |
| `connect.v2+proto` | Protobuf binary              |
| `connect.v2+json`  | Protobuf JSON                |
| `connect.v2`       | Protobuf binary (base token) |

A client MUST offer at least one of these tokens in `Sec-WebSocket-Protocol`,
and the token it offers MUST correspond to the codec it will encode with. A
client MAY offer several, in descending order of preference.

A server MUST select the first token it recognizes in the client's preference
order, and MUST echo exactly that token in its response. A server that
recognizes none of the offered tokens MUST fail the handshake with HTTP
`400 Bad Request`.

A client MUST verify that the echoed token is one it offered, and MUST close
the connection if it is not. The codec and the offered token are chosen
independently in most client APIs; without this check a client can encode with
one codec while the server decodes with another, and the failure surfaces as an
unintelligible framing error.

`Connect-Protocol-Version` is implied by the subprotocol and MUST NOT be sent
as a header.

### 4.4 Origin

A server MUST reject a cross-origin handshake with HTTP `403 Forbidden` unless
it has been configured to permit one. A handshake carrying no `Origin` header
is not from a browser and MUST NOT be rejected on origin grounds.

A browser attaches the user's ambient credentials to a WebSocket handshake and
performs no CORS preflight, so an accept-all default would let any origin open
an authenticated stream.

## 5. Message markers

Every message begins with one marker: a single Unicode scalar value, UTF-8
encoded, saying what the rest of the message is. The payload follows it
immediately, with nothing in between.

| Marker | Name                 | Direction            | Payload                          |
| ------ | -------------------- | -------------------- | -------------------------------- |
| `B`    | Body                 | either               | an RPC message                   |
| `M`    | Leading-Metadata     | either               | JSON metadata ([§7.3](#73-leading-metadata-messages)) |
| `S`    | Server-End-Stream    | server → client      | `EndStreamResponse` JSON ([§9](#9-end-of-response)) |
| `C`    | Client-End-Stream    | client → server      | a final body, or nothing ([§8](#8-end-of-the-request-stream)) |

There is **no length field**. A message carries exactly one marker and one
payload ([§6](#6-message-framing)), and the WebSocket frame already delimits
it, so a length would only restate what the transport has already said — and
restating it introduces the possibility of the two disagreeing.

A receiver MUST treat a message whose marker it does not recognize as a
protocol error, and MUST NOT guess at the payload. A receiver MUST treat a
marker valid only in the opposite direction as a protocol error: `S` from a
client, `C` from a server.

A message that is empty — no marker at all — is a protocol error.

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

### 5.2 Relationship to Connect's envelope

Connect over HTTP prefixes each message with a flags byte and a big-endian
`uint32` length. This binding shares neither.

The flags byte is a bit set, so a message can in principle carry several flags
at once; a marker is a single value and cannot. Nothing here needs the
combination — a message is a body, or metadata, or an end-of-stream, never two
of those — and an enumerated marker makes the illegal states unrepresentable
rather than merely unused.

The consequence is that Connect's flag semantics do not apply here at all.
There is no Compressed flag ([§10](#10-compression)), no reserved-bit range,
and nothing corresponding to the reserved-range concern that
[§3.2](#32-what-this-binding-does-not-borrow) describes: this binding no longer
borrows from Connect's flags byte, so there is nothing left to collide.

## 6. Message framing

Each WebSocket message MUST contain exactly one marker and its payload. A
receiver MUST NOT carry decoding state from one message into the next.

### 6.1 Text and binary

The WebSocket frame type says how the payload is encoded:

| Frame type | Payload encoding |
| ---------- | ---------------- |
| text       | JSON             |
| binary     | Protobuf binary  |

A message MUST be sent as a binary frame if and only if its payload is Protobuf
binary; every other message MUST be a text frame. Stated per marker:

- `M` and `S` are always JSON, so always text.
- `B` follows the negotiated codec: binary under `connect.v2+proto`, text under
  `connect.v2+json`.
- `C` with a final body follows the codec as `B` does. `C` alone carries no
  payload and MUST be text.
- `B` alone — the marker with no payload — is the empty Protobuf message and
  MUST be binary. In JSON an empty message is `{}`, never zero bytes, so a text
  frame containing only `B` is a protocol error.

A connection therefore mixes frame types when the codec is Protobuf: bodies
arrive binary while metadata and end-of-stream arrive text. That is intended.
The frame type is a type tag the transport supplies for free, and a receiver
knows how to parse a payload before it has looked at anything but the frame.

A text frame MUST contain valid UTF-8, which [RFC 6455][rfc6455] §8.1 requires
of every text frame and which a browser enforces. Since the marker is UTF-8 and
JSON is UTF-8, a conforming message satisfies this by construction. An
implementation SHOULD verify it on the way out regardless: a peer that emits
invalid UTF-8 in a text frame has its connection closed by a browser, and a
library that does not validate on the way in will not reproduce that failure in
its own tests.

### 6.2 A worked exchange

A unary `Ping` under `connect.v2+json`, every message a text frame:

```
client →  text    M{}
client →  text    B{"number":"7"}
client →  text    C
server →  text    B{"number":"7"}
server →  text    S{}
server →  close 1000
```

The same call under `connect.v2+proto`, mixing frame types:

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

### 7.1 Request metadata

A client sends request metadata in the Leading-Metadata message that opens
every stream ([§7.3](#73-leading-metadata-messages)). That is the only channel
a client may rely on: the browser WebSocket API cannot set request headers, so
supporting header-borne metadata would serve no client this binding targets.

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
([§9](#9-end-of-response)). A server MUST NOT send the same metadata both ways.

A receiver MUST surface Leading-Metadata as response *headers* and
`EndStreamResponse.metadata` as response *trailers*, preserving the distinction
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
`M` message, and it MUST be the first message on the stream —
empty (`{}`) when the client has no metadata. A server MUST treat a first
message that is anything else, or a second `M` message at any
point, as a protocol error and end the stream.

Both halves of that rule are load-bearing, and neither is stylistic:

- A server consumes this message *before* dispatching the RPC, so that request
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

**Late metadata.** Metadata a server sets after its first `B` has
already been sent MAY be dropped. Connect over HTTP loses it for the same
reason: the header block has been flushed.

## 8. End of the request stream

A client MUST terminate its request stream with exactly one `C` message. It MAY
carry a final body as its payload, or MAY be empty; see
[§6.1](#61-text-and-binary) for which frame type each form takes.

A server MUST treat `C` as end-of-stream, MUST deliver its payload as
a message if non-empty, and MUST ignore any further `B` on that
connection.

A client MUST NOT send a `B` after it. A client API SHOULD report an
attempt to do so as an error rather than accepting it, because the message will
not be delivered.

A server that reaches end of connection without seeing `C` MUST treat
the RPC as canceled by the client.

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

Both fields are omitted when empty; a clean finish with no trailers is `{}`.
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

There is no message-level compression and no marker for it. Connect over HTTP
has a Compressed flag on every envelope; this binding has no equivalent,
because compressing a payload underneath a layer that is already compressing it
gains nothing and costs CPU twice.

## 11. Deadlines

A client expresses a deadline as a `connect-timeout-ms` query parameter on the
handshake URI, and there is no other way to do so. A sender MUST make the value
a positive integer of at most ten digits, in milliseconds.

A receiver MUST reject a value that is not a base-10 integer, or that is longer
than ten characters, with `invalid_argument`. It is not required to reject a
non-positive value; Connect over HTTP does not, and a value of `0` or less
simply yields a deadline that has already passed. Implementations SHOULD NOT
add that check on one transport alone.

The query string rather than metadata, for two reasons. A browser cannot set
request headers on a handshake, so the URI is its only channel; and a deadline
carried in an `M` message could not bound the read of that message, which is the
first thing the deadline has to cover.

**The server's bound.** A server MUST impose a maximum of its own, and the
effective deadline is the **shorter** of the two. A client may therefore ask
for less time than the server allows, never more, and an RPC that requests no
deadline still has one.

That bound is what makes the rest of this specification safe against a peer
that stops participating. A server waits for the `M` message that opens a
stream ([§7.3](#73-leading-metadata-messages)) before it dispatches
the RPC; without a deadline, a peer that upgrades and then goes silent would
hold that connection indefinitely.

Implementations SHOULD make the maximum configurable and SHOULD default it
generously — long-lived streams are the reason this binding exists, and a short
default would sever the subscriptions it is meant to carry.

## 12. Size limits

A receiver MAY impose a maximum message size, and MUST apply it to the
decompressed size of a message. A receiver that also wishes to bound memory
against a compression bomb MUST additionally bound the compressed bytes it
reads.

A receiver that terminates a connection because of a peer's protocol error —
an oversized message included — SHOULD close the TCP connection without a
WebSocket closing handshake. The closing handshake requires draining whatever
the peer has already queued, which would defeat the limit it was just
enforcing: measured against a zero-output DEFLATE bomb, a read cut off at 96
KiB still cost 6.4 MiB once the handshake drained the rest.

A receiver MAY instead close with status `1009` when it can do so without
draining. A WebSocket library that enforces a frame-level read limit of its own
typically sends `1009` before this layer sees the message at all, so a peer
should expect that code even though this document never requires sending it.

## 13. Connection closure

| Close code | Meaning                                                                  |
| ---------- | ------------------------------------------------------------------------ |
| `1000`     | Normal completion; the `S` message carries the verdict                  |
| `1009`     | A message exceeded a read limit, usually the WebSocket library's own     |
| `1011`     | The sender could not marshal its own `S` message                        |
| *(none)*   | The peer terminated without a closing handshake ([§12](#12-size-limits)) |

A browser surfaces an absent close frame as code `1006`. A client MUST NOT
report `1006` as a transport failure without first checking whether an
`S` message arrived: if one did, its error is the RPC's real verdict.

## 14. Implementation status

`connectrpc.com/connect/v2/connectwebsocket` implements this document.

The marker format of [§5](#5-message-markers) and the text/binary split of
[§6](#6-message-framing) replaced Connect's flags byte and length prefix, which
this binding carried until the frame boundary made both redundant. Nothing
reads a length any more: a frame that disagreed with its own prefix had two
answers for one question, and the one the transport trusted was the frame's.

The opening-message rule of [§7.3](#73-leading-metadata-messages) is the most
recently added part, and the two halves of it were each forced by a failure the
other did not predict. A server that drained metadata only while frames kept
arriving deadlocked a client that opened a stream and waited to be pushed to;
that is why exactly one `M` is required rather than one or more. And a
second `M`, merged after dispatch, reintroduced a data race on the
handler's own request metadata that the drain existed to remove; that is why a
second is refused.

§7.3 is the most recently added part: the server emits a Leading-Metadata
message, both peers enforce the ordering rule, and a client surfaces the
result as response headers at the same point in the stream that its Connect
HTTP counterpart does.

An earlier draft of this document required a deadline to be validated as
positive ([§11](#11-deadlines)), which described neither the Connect protocol
nor any implementation of it. It was rewritten to match Connect over HTTP, on
the principle that a WebSocket binding should not change behavior the transport
has no bearing on.

[connect]: https://connectrpc.com/docs/protocol/
[rfc6455]: https://datatracker.ietf.org/doc/html/rfc6455
[rfc7692]: https://datatracker.ietf.org/doc/html/rfc7692
[rfc8441]: https://datatracker.ietf.org/doc/html/rfc8441
