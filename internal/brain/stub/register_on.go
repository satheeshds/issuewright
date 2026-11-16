//go:build !nostub

package stub

import "github.com/satheeshds/issuewright/internal/brain"

// Registered by default so tests and dry runs work; release builds pass
// -tags nostub to leave it out of the binary.
func init() { brain.Register(New()) }
