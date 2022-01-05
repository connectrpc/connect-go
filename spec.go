package rerpc

// Specification is a description of a client call or a handler invocation.
type Specification struct {
	Type      StreamType
	Procedure string // e.g., "acme.foo.v1.FooService/Bar"

	// Exactly one of IsClient and IsServer will be true.
	IsClient bool
	IsServer bool
}
