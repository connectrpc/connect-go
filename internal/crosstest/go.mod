module github.com/akshayjshah/rerpc/internal/crosstest

go 1.16

require (
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.1.0
	google.golang.org/protobuf v1.27.1 // indirect
)

replace github.com/akshayjshah/rerpc => ../..
