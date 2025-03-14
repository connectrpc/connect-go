module connectrpc.com/connect

go 1.22

retract (
	v1.10.0 // module cache poisoned, use v1.10.1
	v1.9.0 // module cache poisoned, use v1.9.1
)

require (
	github.com/google/go-cmp v0.5.9
	golang.org/x/net v0.33.0
	google.golang.org/protobuf v1.34.2
)

require golang.org/x/text v0.21.0 // indirect
