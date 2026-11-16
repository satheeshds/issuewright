package deploy

import (
	"strings"
	"testing"
)

func TestUnitsEmbedded(t *testing.T) {
	units, err := Units()
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	svc, ok := units["issuewright@.service"]
	if !ok {
		t.Fatalf("service unit missing: %v", keys(units))
	}
	timer, ok := units["issuewright@.timer"]
	if !ok {
		t.Fatalf("timer unit missing: %v", keys(units))
	}
	// The night-window contract: a missed tick must fire on wake.
	if !strings.Contains(string(timer), "Persistent=true") {
		t.Error("timer must be persistent")
	}
	if !strings.Contains(string(timer), "issuewright@%i.service") {
		t.Error("timer must drive the template service")
	}
	for _, want := range []string{"ExecStart=/usr/local/bin/issuewright tick --repo %i", "EnvironmentFile=/srv/agent/agent.env", "Type=oneshot"} {
		if !strings.Contains(string(svc), want) {
			t.Errorf("service unit missing %q", want)
		}
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
