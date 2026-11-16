//go:build nostub

package stub

// nostub builds (releases) do not register the stub driver.
func init() {}
