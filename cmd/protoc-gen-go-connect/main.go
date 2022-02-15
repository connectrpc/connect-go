// protoc-gen-go-connect is a plugin for the protocol buffer compiler that
// generates Go code. To use it, build this program and make it available on
// your PATH as protoc-gen-go-connect.
//
// The 'go-connect' suffix becomes part of the arguments for the protocol buffer
// compiler, so you'll invoke it like this:
//	 protoc --go-connect_out=. path/to/file.proto
//
// This generates service definitions for the protocol buffer services defined
// by file.proto. As invoked above, the output will be written to:
//	 path/to/file_connect.pb.go
// If you'd prefer to write the output elsewhere, set '--go-connect_opt' as
// described in
// https://developers.google.com/protocol-buffers/docs/reference/go-generated.
package main

import (
	"flag"
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/bufconnect/connect"
)

func main() {
	version := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()
	if *version {
		fmt.Printf("protoc-gen-go-connect %s\n", connect.Version)
		return
	}

	var flags flag.FlagSet
	// By default, generate our code into a separate package from protoc-gen-go's
	// output (typically determined by the "go_package" file option). In this
	// mode, connect's generated code imports message types from the base
	// package. This lets us safely generate code with short names, since we
	// don't need to worry about collisions with gRPC, Twirp, or whatever other
	// plugins the user might run.
	//
	// When this flag is set, behave like the gRPC and Twirp plugins and generate
	// code in the same package as the basic message types. This increases the
	// likelihood of compile-time name collisions.
	colocateGoTypes := flags.Bool(
		"colocate_go_types",
		false,
		"Generate RPC code in the same package as the basic Go types.",
	)
	protogen.Options{ParamFunc: flags.Set}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		for _, f := range gen.Files {
			if f.Generate {
				generate(gen, f, *colocateGoTypes)
			}
		}
		return nil
	})
}
