// +build tools

package tools

import (
	// Code generators.
	_ "github.com/bufbuild/buf/cmd/buf"
	_ "github.com/twitchtv/twirp/protoc-gen-twirp"
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
