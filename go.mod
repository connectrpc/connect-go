module connectrpc.com/connect

go 1.19

retract (
	v1.10.0 // module cache poisoned, use v1.10.1
	v1.9.0 // module cache poisoned, use v1.9.1
)

require (
	github.com/google/go-cmp v0.5.9
	golang.org/x/net v0.17.0
	google.golang.org/protobuf v1.31.0
)

require golang.org/x/text v0.13.0 // indirect
