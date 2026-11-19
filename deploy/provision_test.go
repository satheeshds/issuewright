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

func TestProvisionInstallsBrainsForTheServiceUser(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot")
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}
	for _, want := range []string{
		"toolchains for the agent user",
		"install @earendil-works/pi-coding-agent for agent",
		"install opencode-ai for agent",
		"pi install https://github.com/monotykamary/pi-opencode-go-provider",
		"none copied",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("toolchain section missing %q:\n%s", want, out)
		}
	}
	// Host tools are reported but never installed behind the operator's back.
	if strings.Contains(out, "apt-get install") || strings.Contains(out, "tarball") {
		t.Error("host toolchains should be reported, not installed silently")
	}
}

func TestProvisionCopiesPiCredentialsOnlyOnRequest(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot", "--copy-pi-auth-from", "nosuchuser98765")
	if code == 0 {
		t.Fatalf("an unknown user must fail:\n%s", out)
	}
	if !strings.Contains(out, "no such user") {
		t.Errorf("error should name the missing user:\n%s", out)
	}

	if _, code := runScript(t, "--copy-pi-auth-from"); code != 1 {
		t.Errorf("a flag without a value should be a usage error, got %d", code)
	}
}

func TestProvisionNeverPrintsTheToken(t *testing.T) {
	root := t.TempDir()
	secret := "github_pat_DO_NOT_PRINT_ME"
	if err := os.WriteFile(filepath.Join(root, "agent.env"), []byte("GH_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot", "--root", root)
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}
	if strings.Contains(out, secret) {
		t.Error("the provisioning output leaked GH_TOKEN")
	}
	if !strings.Contains(out, "would clone into") {
		t.Errorf("with a token present it should plan the clone:\n%s", out)
	}
}

func TestProvisionWithoutTokenExplainsTheClone(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot")
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "no GH_TOKEN") || !strings.Contains(out, "git clone https://github.com/acme/demo.git") {
		t.Errorf("the clone step must say what it needs:\n%s", out)
	}
}

// `sudo -u agent pi ...` fails because sudo resets PATH to secure_path, which
// does not include /srv/agent/bin. The script must both fix that for interactive
// shells and print a command that actually works.
func TestProvisionMakesPiReachableForTheServiceUser(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot")
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "shell PATH for agent") || !strings.Contains(out, ".bashrc") {
		t.Errorf("the service user's shell PATH is not handled:\n%s", out)
	}
	if !strings.Contains(out, "env PATH=/srv/agent/bin:/usr/local/bin:/usr/bin:/bin pi auth login") {
		t.Errorf("the printed login command would still fail with 'command not found':\n%s", out)
	}
	if strings.Contains(out, "(plain  fails") {
		t.Error("the instructions lost their wording to shell expansion")
	}
}

// A check that runs `go test` or `flutter test` fails with 127 unless the
// service PATH reaches the toolchains. The agent noticed this itself on a live
// run: "go: command not found" while verifying a docs change.
func TestProvisionWritesAToolchainPath(t *testing.T) {
	out, code := runScript(t, "--slug", "acme/demo", "--bot", "bot", "--root", "/tmp/iw-root")
	if code != 0 {
		t.Fatalf("dry run failed (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "PATH for the agent:") {
		t.Fatalf("no PATH reported:\n%s", out)
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "PATH for the agent:") {
			line = l
		}
	}
	for _, want := range []string{"/usr/local/bin", "/usr/bin", "/tmp/iw-root/bin"} {
		if !strings.Contains(line, want) {
			t.Errorf("PATH %q missing %q", strings.TrimSpace(line), want)
		}
	}
	if !strings.Contains(out, "would set PATH=") {
		t.Errorf("the PATH is not written into agent.env:\n%s", out)
	}
}
