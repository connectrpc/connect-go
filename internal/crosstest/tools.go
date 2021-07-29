// +build tools

package tools

import (
	_ "github.com/twitchtv/twirp/protoc-gen-twirp"    // for code gen
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc" // for code gen
)
