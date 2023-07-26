module connectrpc.com/connect

go 1.19

retract (
	v1.10.0 // module cache poisoned, use v1.10.1
	v1.9.0 // module cache poisoned, use v1.9.1
)

require (
	github.com/google/go-cmp v0.5.9
	google.golang.org/protobuf v1.31.0
)
