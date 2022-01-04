package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rerpc/rerpc"
)

const (
	contextPackage = protogen.GoImportPath("context")
	rerpcPackage   = protogen.GoImportPath("github.com/rerpc/rerpc")
	httpPackage    = protogen.GoImportPath("net/http")
	protoPackage   = protogen.GoImportPath("google.golang.org/protobuf/proto")
	stringsPackage = protogen.GoImportPath("strings")
	errorsPackage  = protogen.GoImportPath("errors")
	cstreamPackage = protogen.GoImportPath("github.com/rerpc/rerpc/callstream")
	hstreamPackage = protogen.GoImportPath("github.com/rerpc/rerpc/handlerstream")
)

var (
	contextContext          = contextPackage.Ident("Context")
	contextCanceled         = contextPackage.Ident("Canceled")
	contextDeadlineExceeded = contextPackage.Ident("DeadlineExceeded")
	errorsIs                = errorsPackage.Ident("Is")
)

func deprecated(g *protogen.GeneratedFile) {
	comment(g, "// Deprecated: do not use.")
}

func generate(gen *protogen.Plugin, file *protogen.File) *protogen.GeneratedFile {
	if len(file.Services) == 0 {
		return nil
	}
	filename := file.GeneratedFilenamePrefix + "_rerpc.pb.go"
	g := gen.NewGeneratedFile(filename, file.GoImportPath)
	preamble(gen, file, g)
	content(file, g)
	return g
}

func protocVersion(gen *protogen.Plugin) string {
	v := gen.Request.GetCompilerVersion()
	if v == nil {
		return "(unknown)"
	}
	out := fmt.Sprintf("v%d.%d.%d", v.GetMajor(), v.GetMinor(), v.GetPatch())
	if s := v.GetSuffix(); s != "" {
		out += "-" + s
	}
	return out
}

func preamble(gen *protogen.Plugin, file *protogen.File, g *protogen.GeneratedFile) {
	g.P("// Code generated by protoc-gen-go-rerpc. DO NOT EDIT.")
	g.P("// versions:")
	g.P("// - protoc-gen-go-rerpc v", rerpc.Version)
	g.P("// - protoc             ", protocVersion(gen))
	if file.Proto.GetOptions().GetDeprecated() {
		comment(g, file.Desc.Path(), " is a deprecated file.")
	} else {
		g.P("// source: ", file.Desc.Path())
	}
	g.P()
	g.P("package ", file.GoPackageName)
	g.P()
}

func content(file *protogen.File, g *protogen.GeneratedFile) {
	if len(file.Services) == 0 {
		return
	}
	handshake(g)
	for _, svc := range file.Services {
		service(file, g, svc)
	}
}

func handshake(g *protogen.GeneratedFile) {
	comment(g, "This is a compile-time assertion to ensure that this generated file ",
		"and the rerpc package are compatible. If you get a compiler error that this constant ",
		"isn't defined, this code was generated with a version of rerpc newer than the one ",
		"compiled into your binary. You can fix the problem by either regenerating this code ",
		"with an older version of rerpc or updating the rerpc version compiled into your binary.")
	g.P("const _ = ", rerpcPackage.Ident("SupportsCodeGenV0"), " // requires reRPC v0.0.1 or later")
	g.P()
}

func service(file *protogen.File, g *protogen.GeneratedFile, service *protogen.Service) {
	clientName := service.GoName + "ClientReRPC"
	serverName := service.GoName + "ReRPC"

	clientInterface(g, service, clientName)
	clientImplementation(g, service, clientName)

	serverInterface(g, service, serverName)
	serverConstructor(g, service, serverName)
	serverImplementation(g, service, serverName)
}

func clientInterface(g *protogen.GeneratedFile, service *protogen.Service, name string) {
	comment(g, name, " is a client for the ", service.Desc.FullName(), " service.")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.Annotate(name, service.Location)
	g.P("type ", name, " interface {")
	for _, method := range service.Methods {
		g.Annotate(name+"."+method.GoName, method.Location)
		g.P(method.Comments.Leading, clientSignature(g, name, method))
	}
	g.P("}")
	g.P()
}

func clientSignature(g *protogen.GeneratedFile, cname string, method *protogen.Method) string {
	if method.Desc.Options().(*descriptorpb.MethodOptions).GetDeprecated() {
		deprecated(g)
	}
	if method.Desc.IsStreamingClient() && method.Desc.IsStreamingServer() {
		// bidi streaming
		return method.GoName + "(ctx " + g.QualifiedGoIdent(contextContext) + ") " +
			"*" + g.QualifiedGoIdent(cstreamPackage.Ident("Bidirectional")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]"
	}
	if method.Desc.IsStreamingClient() {
		// client streaming
		return method.GoName + "(ctx " + g.QualifiedGoIdent(contextContext) + ") " +
			"*" + g.QualifiedGoIdent(cstreamPackage.Ident("Client")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]"
	}
	if method.Desc.IsStreamingServer() {
		// server streaming
		return method.GoName + "(ctx " + g.QualifiedGoIdent(contextContext) +
			", req *" + g.QualifiedGoIdent(method.Input.GoIdent) + ") " +
			"(*" + g.QualifiedGoIdent(cstreamPackage.Ident("Server")) +
			"[" + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			", error)"
	}
	// unary
	return method.GoName + "(ctx " + g.QualifiedGoIdent(contextContext) +
		", req *" + g.QualifiedGoIdent(method.Input.GoIdent) + ") " +
		"(*" + g.QualifiedGoIdent(method.Output.GoIdent) + ", error)"
}

func clientImplementation(g *protogen.GeneratedFile, service *protogen.Service, name string) {
	// Client struct.
	callOption := rerpcPackage.Ident("CallOption")
	g.P("type ", unexport(name), " struct {")
	g.P("doer ", rerpcPackage.Ident("Doer"))
	g.P("baseURL string")
	g.P("options []", callOption)
	g.P("}")
	g.P()

	// Client constructor.
	comment(g, "New", name, " constructs a client for the ", service.Desc.FullName(),
		" service. Call options passed here apply to all calls made with this client.")
	g.P("//")
	comment(g, "The URL supplied here should be the base URL for the gRPC server ",
		"(e.g., https://api.acme.com or https://acme.com/grpc).")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.P("func New", name, " (baseURL string, doer ", rerpcPackage.Ident("Doer"),
		", opts ...", callOption, ") ", name, " {")
	g.P("return &", unexport(name), "{")
	g.P("baseURL: ", stringsPackage.Ident("TrimRight"), `(baseURL, "/"),`)
	g.P("doer: doer,")
	g.P("options: opts,")
	g.P("}")
	g.P("}")
	g.P()

	// Client method implementations.
	for _, method := range service.Methods {
		clientMethod(g, service, name, method)
	}
}

func clientMethod(g *protogen.GeneratedFile, service *protogen.Service, cname string, method *protogen.Method) {
	isStreamingClient := method.Desc.IsStreamingClient()
	isStreamingServer := method.Desc.IsStreamingServer()
	comment(g, method.GoName, " calls ", method.Desc.FullName(), ".",
		" Call options passed here apply only to this call.")
	if method.Desc.Options().(*descriptorpb.MethodOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.P("func (c *", unexport(method.Parent.GoName), "ClientReRPC) ", clientSignature(g, cname, method), " {")

	if isStreamingClient || isStreamingServer {
		g.P("ctx, call := ", rerpcPackage.Ident("NewClientStream"), "(")
		g.P("ctx,")
		g.P("c.doer,")
		if isStreamingClient && isStreamingServer {
			g.P(rerpcPackage.Ident("StreamTypeBidirectional"), ",")
		} else if isStreamingClient {
			g.P(rerpcPackage.Ident("StreamTypeClient"), ",")
		} else {
			g.P(rerpcPackage.Ident("StreamTypeServer"), ",")
		}
		g.P("c.baseURL,")
		g.P(`"`, service.Desc.ParentFile().Package(), `", // protobuf package`)
		g.P(`"`, service.Desc.Name(), `", // protobuf service`)
		g.P(`"`, method.Desc.Name(), `", // protobuf method`)
		g.P("c.options...,")
		g.P(")")

		g.P("stream := call(ctx)")
		if !isStreamingClient && isStreamingServer {
			// server streaming, we need to send the request.
			g.P("if err := stream.Send(req); err != nil {")
			g.P("_ = stream.CloseSend(err)")
			g.P("_ = stream.CloseReceive()")
			g.P("return nil, err")
			g.P("}")
			g.P("if err := stream.CloseSend(nil); err != nil {")
			g.P("_ = stream.CloseReceive()")
			g.P("return nil, err")
			g.P("}")
			g.P("return ", cstreamPackage.Ident("NewServer"), "[", method.Output.GoIdent, "]", "(stream), nil")
		} else if isStreamingClient && !isStreamingServer {
			// client streaming
			g.P("return ", cstreamPackage.Ident("NewClient"),
				"[", method.Input.GoIdent, ", ", method.Output.GoIdent, "]", "(stream)")
		} else {
			// bidi streaming
			g.P("return ", cstreamPackage.Ident("NewBidirectional"),
				"[", method.Input.GoIdent, ", ", method.Output.GoIdent, "]", "(stream)")
		}
		g.P("}")
		g.P()
		return
	}

	// TODO: do this once on client construction
	g.P("call := ", rerpcPackage.Ident("NewClientFunc"), "[", method.Input.GoIdent, ", ", method.Output.GoIdent, "](")
	g.P("c.doer,")
	g.P("c.baseURL,")
	g.P(`"`, service.Desc.ParentFile().Package(), `", // protobuf package`)
	g.P(`"`, service.Desc.Name(), `", // protobuf service`)
	g.P(`"`, method.Desc.Name(), `", // protobuf method`)
	g.P("c.options...,")
	g.P(")")
	g.P("return call(ctx, req)")
	g.P("}")
	g.P()
}

func serverInterface(g *protogen.GeneratedFile, service *protogen.Service, name string) {
	comment(g, name, " is a server for the ", service.Desc.FullName(),
		" service. To make sure that adding methods to this protobuf service doesn't break all ",
		"implementations of this interface, all implementations must embed Unimplemented",
		name, ".")
	g.P("//")
	comment(g, "By default, recent versions of grpc-go have a similar forward compatibility ",
		"requirement. See https://github.com/grpc/grpc-go/issues/3794 for a longer discussion.")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.Annotate(name, service.Location)
	g.P("type ", name, " interface {")
	for _, method := range service.Methods {
		g.Annotate(name+"."+method.GoName, method.Location)
		g.P(method.Comments.Leading, serverSignature(g, name, method))
	}
	g.P("mustEmbedUnimplemented", name, "()")
	g.P("}")
	g.P()
}

func serverSignature(g *protogen.GeneratedFile, sname string, method *protogen.Method) string {
	if method.Desc.Options().(*descriptorpb.MethodOptions).GetDeprecated() {
		deprecated(g)
	}
	if method.Desc.IsStreamingClient() && method.Desc.IsStreamingServer() {
		// bidi streaming
		return method.GoName + "(" + g.QualifiedGoIdent(contextContext) + ", " +
			"*" + g.QualifiedGoIdent(hstreamPackage.Ident("Bidirectional")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			") error"
	}
	if method.Desc.IsStreamingClient() {
		// client streaming
		return method.GoName + "(" + g.QualifiedGoIdent(contextContext) + ", " +
			"*" + g.QualifiedGoIdent(hstreamPackage.Ident("Client")) +
			"[" + g.QualifiedGoIdent(method.Input.GoIdent) + ", " + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			") error"
	}
	if method.Desc.IsStreamingServer() {
		// server streaming
		return method.GoName + "(" + g.QualifiedGoIdent(contextContext) +
			", *" + g.QualifiedGoIdent(method.Input.GoIdent) +
			", *" + g.QualifiedGoIdent(hstreamPackage.Ident("Server")) +
			"[" + g.QualifiedGoIdent(method.Output.GoIdent) + "]" +
			") error"
	}
	// unary
	return method.GoName + "(" + g.QualifiedGoIdent(contextContext) +
		", *" + g.QualifiedGoIdent(method.Input.GoIdent) + ") " +
		"(*" + g.QualifiedGoIdent(method.Output.GoIdent) + ", error)"
}

func serverConstructor(g *protogen.GeneratedFile, service *protogen.Service, name string) {
	comment(g, "New", service.GoName, "HandlerReRPC wraps each method on the service implementation",
		" in a *rerpc.Handler. The returned slice can be passed to rerpc.NewServeMux.")
	if service.Desc.Options().(*descriptorpb.ServiceOptions).GetDeprecated() {
		g.P("//")
		deprecated(g)
	}
	g.P("func New", service.GoName, "HandlerReRPC(svc ", name, ", opts ...", rerpcPackage.Ident("HandlerOption"),
		") []*", rerpcPackage.Ident("Handler"), " {")
	g.P("handlers := make([]*", rerpcPackage.Ident("Handler"), ", 0, ", len(service.Methods), ")")
	g.P()
	for _, method := range service.Methods {
		hname := unexport(string(method.Desc.Name()))

		if method.Desc.IsStreamingServer() || method.Desc.IsStreamingClient() {
			g.P(hname, " := ", rerpcPackage.Ident("NewStreamingHandler"), "(")
			if method.Desc.IsStreamingServer() && method.Desc.IsStreamingClient() {
				g.P(rerpcPackage.Ident("StreamTypeBidirectional"), ",")
			} else if method.Desc.IsStreamingServer() {
				g.P(rerpcPackage.Ident("StreamTypeServer"), ",")
			} else {
				g.P(rerpcPackage.Ident("StreamTypeClient"), ",")
			}
			g.P(`"`, service.Desc.ParentFile().Package(), `", // protobuf package`)
			g.P(`"`, service.Desc.Name(), `", // protobuf service`)
			g.P(`"`, method.Desc.Name(), `", // protobuf method`)
			g.P("func(ctx ", contextContext, ", sf ", rerpcPackage.Ident("StreamFunc"), ") {")
			g.P("stream := sf(ctx)")
			if method.Desc.IsStreamingServer() && method.Desc.IsStreamingClient() {
				// bidi streaming
				g.P("typed := ", hstreamPackage.Ident("NewBidirectional"),
					"[", method.Input.GoIdent, ", ", method.Output.GoIdent, "]", "(stream)")
			} else if method.Desc.IsStreamingClient() {
				// client streaming
				g.P("typed := ", hstreamPackage.Ident("NewClient"),
					"[", method.Input.GoIdent, ", ", method.Output.GoIdent, "]", "(stream)")
			} else {
				// server streaming
				g.P("typed := ", hstreamPackage.Ident("NewServer"),
					"[", method.Output.GoIdent, "]", "(stream)")
			}
			if method.Desc.IsStreamingServer() && !method.Desc.IsStreamingClient() {
				g.P("var req ", method.Input.GoIdent)
				g.P("if err := stream.Receive(&req); err != nil {")
				g.P("_ = stream.CloseReceive()")
				g.P("_ = stream.CloseSend(err)")
				g.P("return")
				g.P("}")
				g.P("if err := stream.CloseReceive(); err != nil {")
				g.P("_ = stream.CloseSend(err)")
				g.P("return")
				g.P("}")
				g.P("err := svc.", method.GoName, "(stream.Context(), &req, typed)")
			} else {
				g.P("err := svc.", method.GoName, "(stream.Context(), typed)")
				g.P("_ = stream.CloseReceive()")
			}
			g.P("if err != nil {")
			g.P("if _, ok := ", rerpcPackage.Ident("AsError"), "(err); !ok {")
			g.P("if ", errorsIs, "(err, ", contextCanceled, ") {")
			g.P("err = ", rerpcPackage.Ident("Wrap"), "(", rerpcPackage.Ident("CodeCanceled"), ", err)")
			g.P("}")
			g.P("if ", errorsIs, "(err, ", contextDeadlineExceeded, ") {")
			g.P("err = ", rerpcPackage.Ident("Wrap"), "(", rerpcPackage.Ident("CodeDeadlineExceeded"), ", err)")
			g.P("}")
			g.P("}")
			g.P("}")
			g.P("_ = stream.CloseSend(err)")
			g.P("},")
			g.P("opts...,")
			g.P(")")
		} else {
			g.P(hname, " := ", rerpcPackage.Ident("NewUnaryHandler"), "(")
			g.P(`"`, service.Desc.ParentFile().Package(), `", // protobuf package`)
			g.P(`"`, service.Desc.Name(), `", // protobuf service`)
			g.P(`"`, method.Desc.Name(), `", // protobuf method`)
			g.P("svc.", method.GoName, ",")
			g.P("opts...,")
			g.P(")")
		}
		g.P("handlers = append(handlers, ", hname, ")")
		g.P()
	}
	g.P("return handlers")
	g.P("}")
	g.P()
}

func serverImplementation(g *protogen.GeneratedFile, service *protogen.Service, name string) {
	g.P("var _ ", name, " = (*Unimplemented", name, ")(nil) // verify interface implementation")
	g.P()
	// Unimplemented server implementation (for forward compatibility).
	comment(g, "Unimplemented", name, " returns CodeUnimplemented from",
		" all methods. To maintain forward compatibility, all implementations",
		" of ", name, " must embed Unimplemented", name, ". ")
	g.P("type Unimplemented", name, " struct {}")
	g.P()
	for _, method := range service.Methods {
		g.P("func (Unimplemented", name, ") ", serverSignature(g, name, method), "{")
		if method.Desc.IsStreamingServer() || method.Desc.IsStreamingClient() {
			g.P("return ", rerpcPackage.Ident("Errorf"), "(", rerpcPackage.Ident("CodeUnimplemented"), `, "`, method.Desc.FullName(), ` isn't implemented")`)
		} else {
			g.P("return nil, ", rerpcPackage.Ident("Errorf"), "(", rerpcPackage.Ident("CodeUnimplemented"), `, "`, method.Desc.FullName(), ` isn't implemented")`)
		}
		g.P("}")
		g.P()
	}
	g.P("func (Unimplemented", name, ") mustEmbedUnimplemented", name, "() {}")
	g.P()
}

func unexport(s string) string { return strings.ToLower(s[:1]) + s[1:] }
