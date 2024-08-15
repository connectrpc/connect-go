module connectrpc.com/connect

go 1.21

retract (
	v1.10.0 // module cache poisoned, use v1.10.1
	v1.9.0 // module cache poisoned, use v1.9.1
)

require (
	github.com/google/go-cmp v0.5.9
	golang.org/x/net v0.23.0
	google.golang.org/protobuf v1.34.2
)

require golang.org/x/text v0.14.0 // indirect
