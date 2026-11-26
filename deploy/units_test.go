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

// The status page ships as a third unit; it must be loopback-only and must not be
// able to write anything (read-only by design).
func TestWebUnitIsReadOnlyAndLoopback(t *testing.T) {
	units, err := Units()
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := units["issuewright-web.service"]
	if !ok {
		t.Fatalf("web unit missing: %v", keys(units))
	}
	body := string(svc)
	for _, want := range []string{
		"ExecStart=/usr/local/bin/issuewright serve",
		"--bind 127.0.0.1:3030",
		"User=agent",
		"EnvironmentFile=-/srv/agent/token.env",
		"Restart=on-failure",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("web unit missing %q", want)
		}
	}
	if strings.Contains(body, "0.0.0.0") {
		t.Error("the unit must not bind a public interface by default")
	}
	if _, hasTimer := units["issuewright-web.timer"]; hasTimer {
		t.Error("the web unit needs no timer")
	}
}
