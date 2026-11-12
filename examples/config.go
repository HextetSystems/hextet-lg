// Package examples provides the example configuration embedded in release binaries.
package examples

import _ "embed"

//go:embed config.example.yaml
var example []byte

// Example returns the documented sample configuration.
func Example() []byte {
	return append([]byte(nil), example...)
}
