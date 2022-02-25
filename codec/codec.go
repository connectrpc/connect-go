// Package codec defines the abstractions for pluggable serialization.
// Explain like I'm 2 again why we need this package, something with chicken or egg right?
package codec

// A Codec can marshal structs (typically generated from a schema) to and from
// bytes.
type Codec interface {
	Marshal(any) ([]byte, error)
	Unmarshal([]byte, any) error
}
