# protoc-gen-connect-go options

This module provides proto extensions for customising per-method timeouts on
[ConnectRPC Go](https://github.com/connectrpc/connect-go) service handlers.

## Usage

Add this module as a dependency in your `buf.yaml`:

```yaml
version: v2
deps:
  - buf.build/connectrpc/connect-go
```

Then import the annotations in your proto file and annotate your RPC methods:

```proto
syntax = "proto3";

import "connectrpc/go/options/v1/annotations.proto";

service GreetService {
  rpc Greet(GreetRequest) returns (GreetResponse) {
    option (connectrpc.go.options.v1.timeouts) = {
      read_ms: 3000    // 3 seconds
      write_ms: 10000  // 10 seconds
    };
  }

  // For streaming RPCs, you can set -1 to disable timeouts entirely.
  rpc Subscribe(SubscribeRequest) returns (stream SubscribeResponse) {
    option (connectrpc.go.options.v1.timeouts) = {
      read_ms: -1
      write_ms: -1
    };
  }
}
```

## Timeout values

| Value | Meaning                                       |
| ----- | --------------------------------------------- |
| `0`   | Use the server-wide default timeout (if any)  |
| `> 0` | Timeout in milliseconds                       |
| `< 0` | No timeouts (recommended for streaming RPCs)  |

## Code generation

These options are consumed by `protoc-gen-connect-go` during code generation.
No additional runtime dependencies are required.
