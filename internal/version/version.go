// Package version reports the build the binary was made from.
package version

import "runtime"

// Value is set by -ldflags "-X .../internal/version.Value=v0.1.0" in releases.
var Value = "dev"

// String is the full human-readable version line.
func String() string {
	return "issuewright " + Value + " (" + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"
}
