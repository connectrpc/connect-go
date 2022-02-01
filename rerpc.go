package connect

// Version is the semantic version of the connect module.
const Version = "0.0.1"

// StreamType describes whether the client, server, neither, or both is
// streaming.
type StreamType uint8

const (
	StreamTypeUnary         StreamType = 0b00
	StreamTypeClient                   = 0b01
	StreamTypeServer                   = 0b10
	StreamTypeBidirectional            = StreamTypeClient | StreamTypeServer
)

// These constants are used in compile-time handshakes with connect's generated
// code.
const (
	SupportsCodeGenV0 = iota
)
