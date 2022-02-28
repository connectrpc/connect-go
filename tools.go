//go:build tools
// +build tools

package tools

import (
	// Code generators.
	_ "github.com/bufbuild/buf/cmd/buf"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
