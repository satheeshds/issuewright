package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The reference profile in examples/ is documentation, and documentation rots.
// This test makes the example either true or a failing build.
func TestExampleProfileIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "agent.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var p Profile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	p.applyDefaults()
	if err := p.Validate(); err != nil {
		t.Fatalf("examples/agent.yaml is invalid: %v", err)
	}
	if len(p.Checks) == 0 {
		t.Error("the example should demonstrate checks")
	}
	drift := 0
	for _, c := range p.Checks {
		if len(c.Drift) > 0 {
			drift++
		}
	}
	if drift == 0 {
		t.Error("the example should demonstrate a drift gate")
	}
	if len(p.Protect.NeverRun) == 0 || len(p.Protect.WriteDenylist) == 0 {
		t.Error("the example should demonstrate both hard-rule lists")
	}
	if _, ok := p.Workflows["review"]; !ok {
		t.Error("the example should show review enabled")
	}
}

func TestExampleRulesFileExists(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", "examples", "rules.md")); err != nil {
		t.Fatalf("examples/rules.md: %v", err)
	}
}
