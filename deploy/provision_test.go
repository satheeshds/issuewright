package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/config"
)

// runScript executes provision.sh with args and returns stdout+stderr and exit code.
func runScript(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"provision.sh"}, args...)...)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run provision.sh: %v", err)
	}
	return string(out), code
}

func TestProvisionScriptIsExecutableAndValid(t *testing.T) {
	st, err := os.Stat("provision.sh")
	if err != nil {
		t.Fatalf("provision.sh: %v", err)
	}
	if st.Mode()&0o111 == 0 {
		t.Error("provision.sh must be executable")
	}
	if out, err := exec.Command("bash", "-n", "provision.sh").CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestProvisionHelpAndUsageErrors(t *testing.T) {
	if out, code := runScript(t, "--help"); code != 0 || !strings.Contains(out, "--apply") {
		t.Errorf("--help: code %d\n%s", code, out)
	}
	if out, code := runScript(t, "--nonsense"); code != 1 || !strings.Contains(out, "unknown argument") {
		t.Errorf("unknown arg: code %d\n%s", code, out)
	}
}

// The important test: what the script would write must be a config the binary
// actually accepts. Documentation that does not parse is a lie.
func TestProvisionEmitsAValidConfig(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "demo-bot[bot]", "--root", "/tmp/iw-root")
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}

	block := extractBlock(t, out, "would write /tmp/iw-root/config.toml")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v\n%s", err, block)
	}
	if len(cfg.Repos) != 1 {
		t.Fatalf("repos = %+v", cfg.Repos)
	}
	r := cfg.Repos[0]
	if r.Name != "demo" || r.Slug != "acme/demo" || r.Bot != "demo-bot[bot]" {
		t.Errorf("repo block = %+v", r)
	}
	if r.Path != "/tmp/iw-root/repos/demo" {
		t.Errorf("path = %q", r.Path)
	}
	if cfg.Agent.Brain != "pi" || cfg.Agent.Thinking != "high" {
		t.Errorf("agent defaults = %+v", cfg.Agent)
	}
}

func TestProvisionEmitsAUsableEnvFile(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot")
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}
	block := extractBlock(t, out, "would write /srv/agent/agent.env")
	if !strings.Contains(block, "GH_TOKEN=") {
		t.Errorf("agent.env should document GH_TOKEN:\n%s", block)
	}
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		// Everything must be a comment: a provisioned env file must never carry
		// a live secret, and must never define one that looks real.
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			t.Errorf("agent.env has a live line: %q", line)
		}
	}
}

func TestProvisionRefusesApplyWithoutSlug(t *testing.T) {
	out, code := runScript(t, "--apply")
	if code == 0 {
		t.Fatalf("--apply without --slug should fail:\n%s", out)
	}
	if !strings.Contains(out, "sudo") && !strings.Contains(out, "--slug") {
		t.Errorf("error should say what is missing:\n%s", out)
	}
}

// extractBlock pulls the indented payload printed after a dry-run marker.
func extractBlock(t *testing.T, out, marker string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, marker) {
			start = i + 1
			break
		}
	}
	if start < 0 || start >= len(lines) {
		t.Fatalf("marker %q not found in output:\n%s", marker, out)
	}
	var block []string
	for _, l := range lines[start:] {
		if !strings.HasPrefix(l, "      ") {
			break
		}
		block = append(block, strings.TrimPrefix(l, "      "))
	}
	if len(block) == 0 {
		t.Fatalf("no payload after %q", marker)
	}
	return strings.Join(block, "\n") + "\n"
}
