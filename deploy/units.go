// Package deploy holds the systemd units and example configs that ship with the
// binary, embedded so `issuewright install` needs no checkout on the target host.
package deploy

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed systemd/*.service systemd/*.timer
var units embed.FS

// Units returns unit file name → contents.
func Units() (map[string][]byte, error) {
	entries, err := fs.ReadDir(units, "systemd")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		raw, err := units.ReadFile(path.Join("systemd", e.Name()))
		if err != nil {
			return nil, err
		}
		out[e.Name()] = raw
	}
	return out, nil
}
