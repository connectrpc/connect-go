# Connect-over-WebSocket Protocol

Status: **Draft**, unratified. Bit allocations in [§5](#5-envelope-flags) are
provisional; see [§3.2](#32-provisional-flag-bit-allocations).
Companion document: [websocket-transport-design.md](websocket-transport-design.md)
records the rationale, the trade-offs, and the Go implementation's structure.
This document is the normative wire format a second implementation is written
against.

## 1. Conventions

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, and MAY are to be
interpreted as described in RFC 2119.

*Sender* and *receiver* denote the two ends of one RPC. *Client* is the peer
that initiated the WebSocket handshake; *server* is the peer that accepted it.

Byte values are written in hexadecimal. Bit 0 is the least significant bit.

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
streaming body. It reuses Connect's Enveloped-Message framing and its
`EndStreamResponse` schema, and departs from Connect in the following ways:

| # | Divergence                                   | Reason                                                                   |
|---|----------------------------------------------|--------------------------------------------------------------------------|
| 1 | Flag bit 2 signals end of the request stream | WebSocket has no half-close, so request EOF MUST be in-band              |
| 2 | Flag bit 3 marks an envelope as metadata     | The browser WebSocket API cannot set headers on a handshake              |
| 3 | Flag bit 0 (Compressed) MUST NOT be set      | Compression is performed by the WebSocket layer ([§10](#10-compression)) |
| 4 | `Streaming-Content-Encoding` is not carried  | No per-request header block exists to carry it                           |
| 5 | A deadline MAY travel as a query parameter   | Browsers cannot set request headers on a handshake                       |
| 6 | One connection carries exactly one RPC       | The procedure is selected by the handshake URI                           |
| 7 | The handshake requires HTTP/1.1              | RFC 6455 defines it there; RFC 8441 is not adopted ([§4.1](#41-http-version)) |

The subprotocol token ([§4.3](#43-subprotocol-negotiation)) is what keeps this
honest: a peer that negotiates `connect.v2` is agreeing to this dialect, not to
the HTTP one.

### 3.2 Provisional flag-bit allocations

Connect reserves the six most significant bits of the flags byte "for future
protocol extensions." This document assigns two of them — bit 2 and bit 3 —
**without that allocation having been granted by the Connect protocol**.

Implementations MUST treat these assignments as provisional. If Connect later
assigns either bit, this document's assignments become invalid and any peer
implementing them is non-conforming. The collision would be silent: a
conforming peer would interpret an End-Of-Client-Stream envelope as whatever
the newer specification defines.

Two resolutions are available and one SHOULD be chosen before a second
implementation is deployed:

1. Allocate bits 2 and 3 to the WebSocket binding in the Connect protocol.
2. Carry these signals as envelope payloads under a sentinel message type,
   the way `EndStreamResponse` already rides bit 1, claiming none of the
   reserved range.

## 4. Connection establishment

### 4.1 HTTP version

The handshake MUST be an HTTP/1.1 request. [RFC 6455][rfc6455] defines it over
HTTP/1.1, and this document does not adopt [RFC 8441][rfc8441]'s Extended
CONNECT, which carries WebSocket over HTTP/2 — no implementation of this
specification supports it.

A **client** whose HTTP stack may otherwise use HTTP/2 MUST ensure the
handshake goes over HTTP/1.1. This is usually automatic: a client library that
recognizes `Connection: Upgrade` with `Upgrade: websocket` routes that request
to an HTTP/1.1 connection. Go's `net/http` does exactly this — see
`Request.requiresHTTP1` — and a browser's WebSocket API does it inherently.

A **server** MUST offer HTTP/1.1 on the listener that accepts handshakes. It
MAY offer HTTP/2 as well, and doing so costs nothing: only the handshake
requires HTTP/1.1, so RPCs a client routes over plain HTTP still negotiate
HTTP/2 on the same connection pool. Against a server offering both, one client
was observed using HTTP/2 for a unary call and HTTP/1.1 for the handshake in
the same session.

A server that offers only HTTP/2 — including h2c — cannot accept a handshake,
because there is no HTTP/1.1 connection to take over. An implementation SHOULD
report that as a server configuration fault rather than a client error: in Go
the `http.ResponseWriter` does not implement `http.Hijacker`, and the handler
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

### 4.3 Subprotocol negotiation

The codec is selected by the WebSocket subprotocol, because a browser cannot
set arbitrary headers on the handshake.

| Token              | Codec                        |
|--------------------|------------------------------|
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

## 5. Envelope flags

Every message is a Connect Enveloped-Message: a five-byte prefix — one flags
byte, then the payload length as a big-endian `uint32` — followed by the
payload. The length counts the payload only.

| Bit | Mask   | Name                 | Direction            | Source                                                |
|-----|--------|----------------------|----------------------|-------------------------------------------------------|
| 0   | `0x01` | Compressed           | —                    | Connect; **prohibited here** ([§10](#10-compression)) |
| 1   | `0x02` | EndStream            | server → client      | Connect                                               |
| 2   | `0x04` | End-Of-Client-Stream | client → server      | This document (provisional)                           |
| 3   | `0x08` | Leading-Metadata     | **either direction** | This document (provisional)                           |

Bits 4-7 are reserved by the Connect protocol, which does not say how a
receiver should treat one that is set. This document does not change that: a
sender SHOULD leave them zero, and a receiver dispatches on the bits it
recognizes without rejecting an envelope merely because an unrecognized bit
accompanies them.

Concretely, a receiver MUST:

- treat flags of `0x00` as a data message;
- otherwise dispatch on the recognized flag present, ignoring any reserved bit
  set alongside it;
- treat an envelope whose flags contain no flag recognized *for its direction*
  as a protocol error. Bit 3 is the only bit valid in both directions.

This matches Connect over HTTP, where an `EndStreamResponse` envelope is
accepted on the strength of bit 1 alone and any other bit is ignored, while an
envelope carrying only reserved bits is rejected as invalid flags.

The leniency has a cost, and it is the reason [§3.2](#32-provisional-flag-bit-allocations)
matters: because a receiver ignores a bit it does not recognize, a peer that
implements a later assignment of bit 2 or bit 3 is not detected — it is
misread.

## 6. Message framing

Each WebSocket binary message MUST contain exactly one envelope. A receiver
MUST treat trailing bytes after the envelope's declared length as a protocol
error, and MUST NOT carry decoding state from one message into the next.

A peer MUST NOT send WebSocket text messages. A receiver MUST treat a text
message as a protocol error.

## 7. Metadata

### 7.1 Request metadata

A client MAY send request metadata as handshake request headers, as
Leading-Metadata envelopes ([§7.3](#73-leading-metadata-envelopes)), or both. A
browser client can use only the latter.

### 7.2 Response metadata

Connect over HTTP distinguishes leading metadata (response headers, readable
before the first message) from trailing metadata (readable after the last).
WebSocket has no response header block after the handshake: the 101 response is
written before the RPC handler runs, so a server cannot know at that point what
the handler will set.

Leading response metadata is therefore carried in Leading-Metadata envelopes,
and trailing response metadata in the `metadata` field of the `EndStreamResponse`
([§9](#9-end-of-response)). A server MUST NOT send the same metadata both ways.

A receiver MUST surface Leading-Metadata as response *headers* and
`EndStreamResponse.metadata` as response *trailers*, preserving the distinction
its Connect HTTP counterpart would.

### 7.3 Leading-Metadata envelopes

An envelope with bit 3 set carries metadata rather than an RPC message. Its
payload is a JSON object mapping header names to arrays of strings:

```json
{"Acme-Tenant": ["tenant-42"], "Authorization": ["Bearer ..."]}
```

Values MUST be arrays of strings, never bare strings. An empty payload is
permitted and carries no metadata.

Keys are case-insensitive. A receiver MUST canonicalize them, so a sender need
not.

**Ordering.** A sender MUST send every Leading-Metadata envelope before its
first data envelope in that direction. A receiver MUST treat a Leading-Metadata
envelope that arrives after a data envelope as a protocol error; without that
rule, "leading" is not a property a receiver can rely on.

**Multiplicity and precedence.** A sender MAY send more than one. For each key,
the most recent envelope replaces any earlier value, and any value carried by
the handshake request headers. Replacement, not concatenation, is the rule.

**Server flush point.** A server that has leading metadata to send MUST emit it
before its first data envelope, or before the `EndStream` envelope if it sends
no data envelope at all. A stream that fails without producing a message is
precisely the case where the metadata is most wanted, so it MUST NOT be lost
there.

**Late metadata.** Metadata a server sets after its first data envelope has
already been sent MAY be dropped. Connect over HTTP loses it for the same
reason: the header block has been flushed.

## 8. End of the request stream

A client MUST terminate its request stream with exactly one envelope with bit 2
set. That envelope MAY carry a final data message as its payload, or MAY be
empty.

A server MUST treat this envelope as end-of-stream, MUST deliver its payload as
a message if non-empty, and MUST ignore any further data envelope on that
connection.

A client MUST NOT send a data envelope after it. A client API SHOULD report an
attempt to do so as an error rather than accepting it, because the message will
not be delivered.

A server that reaches end of connection without seeing this envelope MUST treat
the RPC as canceled by the client.

## 9. End of response

A server MUST terminate its response stream with exactly one envelope with bit
1 set, and MUST NOT set that bit on any other envelope. Its payload is a JSON
`EndStreamResponse`:

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

A client that sees the connection close without an `EndStream` envelope MUST
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

Envelope flag bit 0 MUST NOT be set in either direction. A receiver MUST treat
a set bit 0 as a protocol error: a peer setting it is applying Connect-level
compression underneath a layer that is already compressing, and is speaking a
different dialect.

## 11. Deadlines

A deadline MAY be expressed as a `Connect-Timeout-Ms` handshake request header,
or as a `connect-timeout-ms` query parameter on the handshake URI. A sender
MUST make the value a positive integer of at most ten digits, in milliseconds.

A receiver MUST reject a value that is not a base-10 integer, or that is longer
than ten characters, with `invalid_argument`. It is not required to reject a
non-positive value; Connect over HTTP does not, and a value of `0` or less
simply yields a deadline that has already passed. Implementations SHOULD NOT
add that check on one transport alone.

A browser cannot set the header, so the query parameter is its only channel.

If both are present and their values differ, a server MUST fail the RPC with
`invalid_argument`. Guessing would silently lengthen or shorten a deadline the
caller believes it set.

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
|------------|--------------------------------------------------------------------------|
| `1000`     | Normal completion; the `EndStream` envelope carries the verdict          |
| `1009`     | A message exceeded a read limit, usually the WebSocket library's own     |
| `1011`     | The sender could not marshal its own `EndStream` envelope                |
| *(none)*   | The peer terminated without a closing handshake ([§12](#12-size-limits)) |

A browser surfaces an absent close frame as code `1006`. A client MUST NOT
report `1006` as a transport failure without first checking whether an
`EndStream` envelope arrived: if one did, its error is the RPC's real verdict.

## 14. Implementation status

`connectrpc.com/connect/v2/connectwebsocket` implements this document in full.

§7.3 is the most recently added part: the server emits a Leading-Metadata
envelope, both peers enforce the ordering rule, and a client surfaces the
result as response headers at the same point in the stream that its Connect
HTTP counterpart does.

Two rules in earlier drafts of this document — that reserved flag bits must be
rejected ([§5](#5-envelope-flags)), and that a deadline must be validated as
positive ([§11](#11-deadlines)) — described neither the Connect protocol nor
any implementation of it. Both have been rewritten to match Connect over HTTP,
on the principle that a WebSocket binding should not change behavior the
transport has no bearing on.

[connect]: https://connectrpc.com/docs/protocol/
[rfc6455]: https://datatracker.ietf.org/doc/html/rfc6455
[rfc7692]: https://datatracker.ietf.org/doc/html/rfc7692
[rfc8441]: https://datatracker.ietf.org/doc/html/rfc8441
