module connectrpc.com/connect

go 1.26.0

retract (
	v1.10.0 // module cache poisoned, use v1.10.1
	v1.9.0 // module cache poisoned, use v1.9.1
)

require (
	connectrpc.com/connect/v2 v2.0.2-0.20261020190640-2d5a85724a69
	github.com/google/go-cmp v0.7.0
	google.golang.org/protobuf v1.36.11
)
