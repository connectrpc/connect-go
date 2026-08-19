// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// protoc-gen-connect-go is a plugin for the Protobuf compiler that generates
// Go code. To use it, build this program and make it available on your PATH as
// protoc-gen-connect-go.
//
// The 'connect-go' suffix becomes part of the arguments for the Protobuf
// compiler. To generate the base Go types and Connect code using protoc:
//
//	protoc --go_out=gen --connect-go_out=gen path/to/file.proto
//
// With [buf], your buf.gen.yaml will look like this:
//
//	version: v2
//	plugins:
//	  - local: protoc-gen-go
//	    out: gen
//	  - local: protoc-gen-connect-go
//	    out: gen
//
// This generates service definitions for the Protobuf types and services
// defined by file.proto. If file.proto defines the foov1 Protobuf package, the
// invocations above will write output to:
//
//	gen/path/to/file.pb.go
//	gen/path/to/foov1connect/file.connect.go
//
// The generated code is configurable with the same parameters as the protoc-gen-go
// plugin, with the following additional parameters:
//
//   - package_suffix: To generate into a sub-package of the package containing the
//     base .pb.go files using the given suffix. An empty suffix denotes to
//     generate into the same package as the base pb.go files. Default is "connect".
//
// For example, to generate into the same package as the base .pb.go files:
//
//	version: v2
//	plugins:
//	  - local: protoc-gen-go
//	    out: gen
//	  - local: protoc-gen-connect-go
//	    out: gen
//	    opt: package_suffix
//
// This will generate output to:
//
//	gen/path/to/file.pb.go
//	gen/path/to/file.connect.go
//
// [buf]: https://buf.build
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

const (
	contextPackage   = protogen.GoImportPath("context")
	syncPackage      = protogen.GoImportPath("sync")
	connectPackage   = protogen.GoImportPath("connectrpc.com/connect")
	connectV2Package = protogen.GoImportPath("connectrpc.com/connect/v2")

	generatedFilenameExtension = ".connect.go"
	defaultPackageSuffix       = "connect"
	packageSuffixFlagName      = "package_suffix"

	usage = "See https://connectrpc.com/docs/go/getting-started to learn how to use this plugin.\n\nFlags:\n  -h, --help\tPrint this help and exit.\n      --version\tPrint the version and exit."

	commentWidth = 97 // leave room for "// "

	// To propagate top-level comments, we need the field number of the syntax
	// declaration and the package name in the file descriptor.
	protoSyntaxFieldNum  = 12
	protoPackageFieldNum = 2
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, connect.Version)
		os.Exit(0)
	}
	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Fprintln(os.Stdout, usage)
		os.Exit(0)
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	var flagSet flag.FlagSet
	packageSuffix := flagSet.String(
		packageSuffixFlagName,
		defaultPackageSuffix,
		"Generate files into a sub-package of the package containing the base .pb.go files using the given suffix. An empty suffix denotes to generate into the same package as the base pb.go files.",
	)
	protogen.Options{
		ParamFunc:         flagSet.Set,
		ImportRewriteFunc: importRewriteFunc,
	}.Run(
		func(plugin *protogen.Plugin) error {
			plugin.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL) | uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
			plugin.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
			plugin.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2024
			for _, file := range plugin.Files {
				if file.Generate {
					generate(plugin, file, *packageSuffix)
				}
			}
			return nil
		},
	)
}

// importRewriteFunc is a workaround for import alias naming.
// See https://github.com/golang/protobuf/issues/1205
func importRewriteFunc(path protogen.GoImportPath) protogen.GoImportPath {
	if path == connectPackage {
		return connectV2Package
	}
	return path
}

func generate(plugin *protogen.Plugin, file *protogen.File, packageSuffix string) {
	if len(file.Services) == 0 {
		return
	}

	goImportPath := file.GoImportPath
	if packageSuffix != "" {
		if !token.IsIdentifier(packageSuffix) {
			plugin.Error(fmt.Errorf("package_suffix %q is not a valid Go identifier", packageSuffix))
			return
		}
		file.GoPackageName += protogen.GoPackageName(packageSuffix)
		generatedFilenamePrefixToSlash := filepath.ToSlash(file.GeneratedFilenamePrefix)
		file.GeneratedFilenamePrefix = path.Join(
			path.Dir(generatedFilenamePrefixToSlash),
			string(file.GoPackageName),
			path.Base(generatedFilenamePrefixToSlash),
		)
		goImportPath = protogen.GoImportPath(path.Join(
			string(file.GoImportPath),
			string(file.GoPackageName),
		))
	}
	generatedFile := plugin.NewGeneratedFile(
		file.GeneratedFilenamePrefix+generatedFilenameExtension,
		goImportPath,
	)
	if packageSuffix != "" {
		generatedFile.Import(file.GoImportPath)
	}
	generatePreamble(generatedFile, file)
	generateServiceNameConstants(generatedFile, file.Services)
	for _, service := range file.Services {
		generateService(generatedFile, file, service)
	}
}

func generatePreamble(g *protogen.GeneratedFile, file *protogen.File) {
	syntaxPath := protoreflect.SourcePath{protoSyntaxFieldNum}
	syntaxLocation := file.Desc.SourceLocations().ByPath(syntaxPath)
	for _, comment := range syntaxLocation.LeadingDetachedComments {
		leadingComments(g, protogen.Comments(comment), false /* deprecated */)
	}
	g.P()
	leadingComments(g, protogen.Comments(syntaxLocation.LeadingComments), false /* deprecated */)
	g.P()

	programName := filepath.Base(os.Args[0])
	// Remove .exe suffix on Windows so that generated code is stable, regardless
	// of whether it was generated on a Windows machine or not.
	if ext := filepath.Ext(programName); strings.ToLower(ext) == ".exe" {
		programName = strings.TrimSuffix(programName, ext)
	}
	g.P("// Code generated by ", programName, ". DO NOT EDIT.")
	g.P("//")
	if file.Proto.GetOptions().GetDeprecated() {
		wrapComments(g, file.Desc.Path(), " is a deprecated file.")
	} else {
		g.P("// Source: ", file.Desc.Path())
	}
	g.P()

	pkgPath := protoreflect.SourcePath{protoPackageFieldNum}
	pkgLocation := file.Desc.SourceLocations().ByPath(pkgPath)
	for _, comment := range pkgLocation.LeadingDetachedComments {
		leadingComments(g, protogen.Comments(comment), false /* deprecated */)
	}
	g.P()
	leadingComments(g, protogen.Comments(pkgLocation.LeadingComments), false /* deprecated */)

	g.P("package ", file.GoPackageName)
	g.P()
}

func generateServiceNameConstants(g *protogen.GeneratedFile, services []*protogen.Service) {
	var numMethods int
	g.P("const (")
	for _, service := range services {
		constName := serviceNameConst(service)
		wrapComments(g, constName, " is the fully-qualified name of the ",
			service.Desc.Name(), " service.")
		g.P(constName, ` = "`, service.Desc.FullName(), `"`)
		numMethods += len(service.Methods)
	}
	g.P(")")
	g.P()

	if numMethods == 0 {
		return
	}
	wrapComments(g, "These constants are the procedure names of the RPCs defined in this package. ",
		"They're exposed at runtime as Spec.Procedure and as the final two segments of the HTTP route.")
	g.P("//")
	wrapComments(g, "Note that these are different from the fully-qualified method names used by ",
		"google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to ",
		"reflection-formatted method names, remove the leading slash and convert the ",
		"remaining slash to a period.")
	g.P("const (")
	for _, service := range services {
		for _, method := range service.Methods {
			// The runtime exposes this value as Spec.Procedure, so we should use the
			// same term here.
			wrapComments(g, procedureConstName(method), " is the procedure name of the ",
				service.Desc.Name(), "'s ", method.Desc.Name(), " RPC.")
			g.P(procedureConstName(method), ` = "`, fmt.Sprintf("/%s/%s", service.Desc.FullName(), method.Desc.Name()), `"`)
		}
	}
	g.P(")")
	g.P()
}

// generateSpecVars emits one lazily-resolved Spec per method. The Spec's
// Schema reads the file descriptor from the protoc-gen-go package.
func generateSpecVars(g *protogen.GeneratedFile, file *protogen.File, service *protogen.Service) {
	if len(service.Methods) == 0 {
		return
	}
	specType := g.QualifiedGoIdent(connectPackage.Ident("Spec"))
	onceValue := g.QualifiedGoIdent(syncPackage.Ident("OnceValue"))
	descIdent := g.QualifiedGoIdent(file.GoDescriptorIdent)
	g.P("var (")
	for _, method := range service.Methods {
		g.P(specVar(service, method), " = ", onceValue, "(func() ", specType, " {")
		g.P("return ", specType, "{")
		g.P("StreamType: ", methodStreamName(g, method), ",")
		g.P("Schema: ", descIdent, `.Services().ByName("`, service.Desc.Name(), `").Methods().ByName("`, method.Desc.Name(), `"),`)
		g.P("Procedure: ", procedureConstName(method), ",")
		if idem := methodIdempotencyName(g, method); idem != "" {
			g.P("IdempotencyLevel: ", idem, ",")
		}
		g.P("}")
		g.P("})")
	}
	g.P(")")
	g.P()
}

func generateService(g *protogen.GeneratedFile, file *protogen.File, service *protogen.Service) {
	generateSpecVars(g, file, service)
	generateClientInterface(g, service)
	generateClientConstructor(g, service)
	generatePerRPCClientStreams(g, service)
	generateServerInterface(g, service)
	generateRegisterHandler(g, service)
	generatePerRPCServerStreams(g, service)
	generateUnimplementedServerImplementation(g, service)
	// Unexported implementation types at the bottom of the file.
	generateClientImplementation(g, service)
	generateHandlerAdapter(g, service)
}

func generateClientInterface(g *protogen.GeneratedFile, service *protogen.Service) {
	wrapComments(g, service.GoName, "Client is a client for the ", service.Desc.FullName(), " service.")
	if isDeprecatedService(service) {
		g.P("//")
		deprecated(g)
	}
	g.AnnotateSymbol(service.GoName+"Client", protogen.Annotation{Location: service.Location})
	g.P("type ", service.GoName, "Client interface {")
	for _, method := range service.Methods {
		ctx := g.QualifiedGoIdent(contextPackage.Ident("Context"))
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		out := g.QualifiedGoIdent(method.Output.GoIdent)
		name := method.GoName
		streamT := clientStreamType(service, method)
		g.AnnotateSymbol(service.GoName+"Client"+"."+method.GoName, protogen.Annotation{Location: method.Location})
		leadingComments(g, method.Comments.Leading, isDeprecatedMethod(method))
		switch methodCardinality(method) {
		case connect.StreamTypeUnary:
			g.P(name, "(", ctx, ", *", input, ") (*", out, ", error)")
		case connect.StreamTypeClient, connect.StreamTypeBidi:
			g.P(name, "(", ctx, ") (", streamT, ", error)")
		case connect.StreamTypeServer:
			g.P(name, "(", ctx, ", *", input, ") (", streamT, ", error)")
		}
	}
	g.P("}")
	g.P()
}

func generateClientConstructor(g *protogen.GeneratedFile, service *protogen.Service) {
	clientType := g.QualifiedGoIdent(connectPackage.Ident("Client"))
	// Take a *connect.Client rather than a Transport so several service clients
	// can share one Client (and its transport plus interceptor chain), mirroring
	// how the generated Register*Handler shares one *connect.Server.
	wrapComments(g, "New", service.GoName, "Client constructs a client for the ", service.Desc.FullName(), " service. ",
		"Multiple service clients may share a single ", clientType, ".",
	)
	g.P("func New", service.GoName, "Client(client *", clientType, ") ", service.GoName, "Client {")
	g.P("return &", clientStruct(service), "{client: client}")
	g.P("}")
	g.P()
}

func generateServerInterface(g *protogen.GeneratedFile, service *protogen.Service) {
	wrapComments(g, service.GoName, "Handler is an implementation of the ", service.Desc.FullName(), " service.")
	if isDeprecatedService(service) {
		g.P("//")
		deprecated(g)
	}
	g.AnnotateSymbol(service.GoName+"Handler", protogen.Annotation{Location: service.Location})
	g.P("type ", service.GoName, "Handler interface {")
	for _, method := range service.Methods {
		ctx := g.QualifiedGoIdent(contextPackage.Ident("Context"))
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		out := g.QualifiedGoIdent(method.Output.GoIdent)
		name := method.GoName
		leadingComments(g, method.Comments.Leading, isDeprecatedMethod(method))
		g.AnnotateSymbol(service.GoName+"Handler"+"."+method.GoName, protogen.Annotation{Location: method.Location})
		switch methodCardinality(method) {
		case connect.StreamTypeUnary:
			g.P(name, "(", ctx, ", *", input, ") (*", out, ", error)")
		case connect.StreamTypeClient:
			g.P(name, "(", ctx, ", ", serverStreamType(service, method), ") (*", out, ", error)")
		case connect.StreamTypeServer:
			g.P(name, "(", ctx, ", *", input, ", ", serverStreamType(service, method), ") error")
		case connect.StreamTypeBidi:
			g.P(name, "(", ctx, ", ", serverStreamType(service, method), ") error")
		}
	}
	g.P("}")
	g.P()
}

func generateRegisterHandler(g *protogen.GeneratedFile, service *protogen.Service) {
	serverType := g.QualifiedGoIdent(connectPackage.Ident("Server"))
	methodType := g.QualifiedGoIdent(connectPackage.Ident("Method"))
	wrapComments(g, "Register", service.GoName, "Handler registers svc as the ", service.Desc.FullName(), " implementation on server.")
	g.P("func Register", service.GoName, "Handler(server *", serverType, ", svc ", service.GoName, "Handler) {")
	if len(service.Methods) > 0 {
		g.P("adapter := ", adapterStruct(service), "{svc: svc}")
	}
	g.P("server.Register(")
	for _, method := range service.Methods {
		g.P(methodType, "{Spec: ", specVar(service, method), "(), Handler: adapter.", adapterMethod(method), "},")
	}
	g.P(")")
	g.P("}")
	g.P()
}
func generatePerRPCClientStreams(g *protogen.GeneratedFile, service *protogen.Service) {
	streamIface := g.QualifiedGoIdent(connectPackage.Ident("ClientStream"))
	for _, method := range service.Methods {
		card := methodCardinality(method)
		if card == connect.StreamTypeUnary {
			continue
		}
		typeName := clientStreamType(service, method)
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		out := g.QualifiedGoIdent(method.Output.GoIdent)
		wrapComments(g, typeName, " is the client stream for the ", service.Desc.Name(), "'s ", method.Desc.Name(), " RPC.")
		g.P("type ", typeName, " struct {")
		g.P("stream ", streamIface)
		g.P("}")
		g.P()
		if card == connect.StreamTypeClient || card == connect.StreamTypeBidi {
			wrapComments(g, "SendHeaders opens the stream and flushes the request headers without a message. The first Send or Receive does this implicitly.")
			g.P("func (s ", typeName, ") SendHeaders() error {")
			g.P("return s.stream.SendHeaders()")
			g.P("}")
			g.P()
			wrapComments(g, "Send sends a request message to the server.")
			g.P("func (s ", typeName, ") Send(req *", input, ") error {")
			g.P("return s.stream.Send(req)")
			g.P("}")
			g.P()
		}
		if card == connect.StreamTypeBidi {
			wrapComments(g, "CloseSend closes the request side of the stream.")
			g.P("func (s ", typeName, ") CloseSend() error {")
			g.P("return s.stream.CloseSend()")
			g.P("}")
			g.P()
		}
		if card == connect.StreamTypeClient {
			wrapComments(g, "CloseAndReceive closes the request side of the stream and returns the single response message. It reads the stream to completion to release its resources.")
			g.P("func (s ", typeName, ") CloseAndReceive() (*", out, ", error) {")
			g.P("if err := s.stream.CloseSend(); err != nil {")
			g.P("return nil, err")
			g.P("}")
			g.P("var res ", out)
			g.P("if err := s.stream.Receive(&res); err != nil {")
			g.P("return nil, err")
			g.P("}")
			g.P("return &res, nil")
			g.P("}")
			g.P()
		} else {
			wrapComments(g, "Receive returns the next response message from the server.")
			g.P("func (s ", typeName, ") Receive() (*", out, ", error) {")
			g.P("var res ", out)
			g.P("if err := s.stream.Receive(&res); err != nil {")
			g.P("return nil, err")
			g.P("}")
			g.P("return &res, nil")
			g.P("}")
			g.P()
			wrapComments(g, "Close releases the stream's resources. It is idempotent and is typically deferred to clean up a stream abandoned before io.EOF.")
			g.P("func (s ", typeName, ") Close() error {")
			g.P("return s.stream.Close()")
			g.P("}")
			g.P()
		}
	}
}

func generatePerRPCServerStreams(g *protogen.GeneratedFile, service *protogen.Service) {
	streamIface := g.QualifiedGoIdent(connectPackage.Ident("ServerStream"))
	for _, method := range service.Methods {
		card := methodCardinality(method)
		if card == connect.StreamTypeUnary {
			continue
		}
		typeName := serverStreamType(service, method)
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		out := g.QualifiedGoIdent(method.Output.GoIdent)
		wrapComments(g, typeName, " is the server stream for the ", service.Desc.Name(), "'s ", method.Desc.Name(), " RPC.")
		g.P("type ", typeName, " struct {")
		g.P("stream ", streamIface)
		g.P("}")
		g.P()
		if card == connect.StreamTypeClient || card == connect.StreamTypeBidi {
			wrapComments(g, "Receive returns the next request message from the client.")
			g.P("func (s ", typeName, ") Receive() (*", input, ", error) {")
			g.P("var req ", input)
			g.P("if err := s.stream.Receive(&req); err != nil {")
			g.P("return nil, err")
			g.P("}")
			g.P("return &req, nil")
			g.P("}")
			g.P()
		}
		if card == connect.StreamTypeServer || card == connect.StreamTypeBidi {
			wrapComments(g, "SendHeaders flushes the response headers without a message. The first Send does this implicitly.")
			g.P("func (s ", typeName, ") SendHeaders() error {")
			g.P("return s.stream.SendHeaders()")
			g.P("}")
			g.P()
			wrapComments(g, "Send sends a response message to the client.")
			g.P("func (s ", typeName, ") Send(res *", out, ") error {")
			g.P("return s.stream.Send(res)")
			g.P("}")
			g.P()
		}
	}
}

func generateClientImplementation(g *protogen.GeneratedFile, service *protogen.Service) {
	clientType := g.QualifiedGoIdent(connectPackage.Ident("Client"))
	g.P("type ", clientStruct(service), " struct {")
	g.P("client *", clientType)
	g.P("}")
	g.P()
	recv := fmt.Sprintf("c *%s", clientStruct(service))
	for _, method := range service.Methods {
		ctx := g.QualifiedGoIdent(contextPackage.Ident("Context"))
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		out := g.QualifiedGoIdent(method.Output.GoIdent)
		name := method.GoName
		streamT := clientStreamType(service, method)
		spec := specVar(service, method) + "()"
		switch methodCardinality(method) {
		case connect.StreamTypeUnary:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ", req *", input, ") (*", out, ", error) {")
			g.P("var res ", out)
			g.P("if err := c.client.CallUnary(ctx, ", spec, ", req, &res); err != nil {")
			g.P("return nil, err")
			g.P("}")
			g.P("return &res, nil")
			g.P("}")
		case connect.StreamTypeClient, connect.StreamTypeBidi:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ") (", streamT, ", error) {")
			g.P("stream, err := c.client.CallClientStream(ctx, ", spec, ")")
			g.P("if err != nil {")
			g.P("return ", streamT, "{}, err")
			g.P("}")
			g.P("return ", streamT, "{stream: stream}, nil")
			g.P("}")
		case connect.StreamTypeServer:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ", req *", input, ") (", streamT, ", error) {")
			g.P("stream, err := c.client.CallServerStream(ctx, ", spec, ", req)")
			g.P("if err != nil {")
			g.P("return ", streamT, "{}, err")
			g.P("}")
			g.P("return ", streamT, "{stream: stream}, nil")
			g.P("}")
		}
		g.P()
	}
}

func generateHandlerAdapter(g *protogen.GeneratedFile, service *protogen.Service) {
	recv := "h " + adapterStruct(service)
	g.P("type ", adapterStruct(service), " struct{ svc ", service.GoName, "Handler }")
	g.P()
	for _, method := range service.Methods {
		ctx := g.QualifiedGoIdent(contextPackage.Ident("Context"))
		specType := g.QualifiedGoIdent(connectPackage.Ident("Spec"))
		handlerStream := g.QualifiedGoIdent(connectPackage.Ident("ServerStream"))
		name := adapterMethod(method)
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		streamT := serverStreamType(service, method)
		switch methodCardinality(method) {
		case connect.StreamTypeUnary:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ", _ ", specType, ", stream ", handlerStream, ") error {")
			g.P("var req ", input)
			g.P("if err := stream.Receive(&req); err != nil {")
			g.P("return err")
			g.P("}")
			g.P("res, err := h.svc.", method.GoName, "(ctx, &req)")
			g.P("if err != nil {")
			g.P("return err")
			g.P("}")
			g.P("return stream.Send(res)")
			g.P("}")
		case connect.StreamTypeClient:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ", _ ", specType, ", stream ", handlerStream, ") error {")
			g.P("res, err := h.svc.", method.GoName, "(ctx, ", streamT, "{stream: stream})")
			g.P("if err != nil {")
			g.P("return err")
			g.P("}")
			g.P("return stream.Send(res)")
			g.P("}")
		case connect.StreamTypeServer:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ", _ ", specType, ", stream ", handlerStream, ") error {")
			g.P("var req ", input)
			g.P("if err := stream.Receive(&req); err != nil {")
			g.P("return err")
			g.P("}")
			g.P("return h.svc.", method.GoName, "(ctx, &req, ", streamT, "{stream: stream})")
			g.P("}")
		case connect.StreamTypeBidi:
			g.P("func (", recv, ") ", name, "(ctx ", ctx, ", _ ", specType, ", stream ", handlerStream, ") error {")
			g.P("return h.svc.", method.GoName, "(ctx, ", streamT, "{stream: stream})")
			g.P("}")
		}
		g.P()
	}
}

func generateUnimplementedServerImplementation(g *protogen.GeneratedFile, service *protogen.Service) {
	newError := g.QualifiedGoIdent(connectPackage.Ident("NewError"))
	codeUnimplemented := g.QualifiedGoIdent(connectPackage.Ident("CodeUnimplemented"))
	typeName := "Unimplemented" + service.GoName + "Handler"
	wrapComments(g, typeName, " returns CodeUnimplemented from all methods.")
	g.P("type ", typeName, " struct{}")
	g.P()
	for _, method := range service.Methods {
		ctx := g.QualifiedGoIdent(contextPackage.Ident("Context"))
		input := g.QualifiedGoIdent(method.Input.GoIdent)
		out := g.QualifiedGoIdent(method.Output.GoIdent)
		msg := fmt.Sprintf("%q", fmt.Sprintf("%s is not implemented", method.Desc.FullName()))
		switch methodCardinality(method) {
		case connect.StreamTypeUnary:
			g.P("func (", typeName, ") ", method.GoName, "(", ctx, ", *", input, ") (*", out, ", error) {")
			g.P("return nil, ", newError, "(", codeUnimplemented, ", ", msg, ")")
		case connect.StreamTypeClient:
			g.P("func (", typeName, ") ", method.GoName, "(", ctx, ", ", serverStreamType(service, method), ") (*", out, ", error) {")
			g.P("return nil, ", newError, "(", codeUnimplemented, ", ", msg, ")")
		case connect.StreamTypeServer:
			g.P("func (", typeName, ") ", method.GoName, "(", ctx, ", *", input, ", ", serverStreamType(service, method), ") error {")
			g.P("return ", newError, "(", codeUnimplemented, ", ", msg, ")")
		case connect.StreamTypeBidi:
			g.P("func (", typeName, ") ", method.GoName, "(", ctx, ", ", serverStreamType(service, method), ") error {")
			g.P("return ", newError, "(", codeUnimplemented, ", ", msg, ")")
		}
		g.P("}")
		g.P()
	}
}

func serviceNameConst(service *protogen.Service) string {
	return service.GoName + "Name"
}

func procedureConstName(m *protogen.Method) string {
	return fmt.Sprintf("%s%sProcedure", m.Parent.GoName, m.GoName)
}

func specVar(service *protogen.Service, method *protogen.Method) string {
	return unexport(service.GoName) + method.GoName + "Spec"
}

func clientStruct(service *protogen.Service) string {
	return unexport(service.GoName) + "Client"
}

func adapterStruct(service *protogen.Service) string {
	return unexport(service.GoName) + "Handler"
}

func adapterMethod(method *protogen.Method) string {
	return unexport(method.GoName)
}

func clientStreamType(service *protogen.Service, method *protogen.Method) string {
	return service.GoName + method.GoName + "ClientStream"
}

func serverStreamType(service *protogen.Service, method *protogen.Method) string {
	return service.GoName + method.GoName + "ServerStream"
}

func isDeprecatedService(service *protogen.Service) bool {
	serviceOptions, ok := service.Desc.Options().(*descriptorpb.ServiceOptions)
	return ok && serviceOptions.GetDeprecated()
}

func isDeprecatedMethod(method *protogen.Method) bool {
	methodOptions, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
	return ok && methodOptions.GetDeprecated()
}

func methodIdempotencyName(g *protogen.GeneratedFile, method *protogen.Method) string {
	opts, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || opts == nil {
		return ""
	}
	switch opts.GetIdempotencyLevel() {
	case descriptorpb.MethodOptions_NO_SIDE_EFFECTS:
		return g.QualifiedGoIdent(connectPackage.Ident("IdempotencyNoSideEffects"))
	case descriptorpb.MethodOptions_IDEMPOTENT:
		return g.QualifiedGoIdent(connectPackage.Ident("IdempotencyIdempotent"))
	case descriptorpb.MethodOptions_IDEMPOTENCY_UNKNOWN:
		return ""
	}
	return ""
}

func methodCardinality(method *protogen.Method) connect.StreamType {
	switch {
	case method.Desc.IsStreamingClient() && method.Desc.IsStreamingServer():
		return connect.StreamTypeBidi
	case method.Desc.IsStreamingClient():
		return connect.StreamTypeClient
	case method.Desc.IsStreamingServer():
		return connect.StreamTypeServer
	}
	return connect.StreamTypeUnary
}

func methodStreamName(g *protogen.GeneratedFile, method *protogen.Method) string {
	switch methodCardinality(method) {
	case connect.StreamTypeClient:
		return g.QualifiedGoIdent(connectPackage.Ident("StreamTypeClient"))
	case connect.StreamTypeServer:
		return g.QualifiedGoIdent(connectPackage.Ident("StreamTypeServer"))
	case connect.StreamTypeBidi:
		return g.QualifiedGoIdent(connectPackage.Ident("StreamTypeBidi"))
	case connect.StreamTypeUnary:
		return g.QualifiedGoIdent(connectPackage.Ident("StreamTypeUnary"))
	}
	return g.QualifiedGoIdent(connectPackage.Ident("StreamTypeUnary"))
}

// Raggedy comments in the generated code are driving me insane. This
// word-wrapping function is ruinously inefficient, but it gets the job done.
func wrapComments(g *protogen.GeneratedFile, elems ...any) {
	text := &bytes.Buffer{}
	for _, el := range elems {
		switch el := el.(type) {
		case protogen.GoIdent:
			fmt.Fprint(text, g.QualifiedGoIdent(el))
		default:
			fmt.Fprint(text, el)
		}
	}
	words := strings.Fields(text.String())
	text.Reset()
	var pos int
	for _, word := range words {
		numRunes := utf8.RuneCountInString(word)
		if pos > 0 && pos+numRunes+1 > commentWidth {
			g.P("// ", text.String())
			text.Reset()
			pos = 0
		}
		if pos > 0 {
			text.WriteRune(' ')
			pos++
		}
		text.WriteString(word)
		pos += numRunes
	}
	if text.Len() > 0 {
		g.P("// ", text.String())
	}
}

func leadingComments(g *protogen.GeneratedFile, comments protogen.Comments, isDeprecated bool) {
	if comments.String() != "" {
		g.P(strings.TrimSpace(comments.String()))
	}
	if isDeprecated {
		if comments.String() != "" {
			g.P("//")
		}
		deprecated(g)
	}
}

func deprecated(g *protogen.GeneratedFile) {
	g.P("// Deprecated: do not use.")
}

func unexport(s string) string {
	lowercased := strings.ToLower(s[:1]) + s[1:]
	switch lowercased {
	// https://go.dev/ref/spec#Keywords
	case "break", "default", "func", "interface", "select",
		"case", "defer", "go", "map", "struct",
		"chan", "else", "goto", "package", "switch",
		"const", "fallthrough", "if", "range", "type",
		"continue", "for", "import", "return", "var":
		return "_" + lowercased
	default:
		return lowercased
	}
}
