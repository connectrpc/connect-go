// Package codec defines the abstractions for pluggable serialization.
package codec

// A Codec can marshal structs (typically generated from a schema) to and from
// bytes.
type Codec interface {
	Marshal(any) ([]byte, error)
	Unmarshal([]byte, any) error
}
