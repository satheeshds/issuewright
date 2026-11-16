package execbrain

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satheeshds/issuewright/internal/brain"
)

func TestRunExpandsTemplate(t *testing.T) {
	dir := t.TempDir()
	d := &Driver{Command: "printf 'cwd=%s' \"$(pwd)\" > out.txt && printf %s {{PROMPT}} >> out.txt"}
	work := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	res, err := d.Run(context.Background(), brain.Spec{
		Prompt: "it's a prompt", Workdir: work, Timeout: time.Minute,
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Driver != "exec" || res.ExitCode != 0 {
		t.Errorf("result = %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(work, "out.txt"))
	if err != nil {
		t.Fatalf("command did not run in the workdir: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "cwd="+work) {
		t.Errorf("workdir not used: %q", got)
	}
	// The quote in the prompt must survive shell quoting.
	if !strings.Contains(got, "it's a prompt") {
		t.Errorf("prompt not substituted safely: %q", got)
	}
}

func TestRunPromptFile(t *testing.T) {
	work := t.TempDir()
	d := &Driver{Command: "cat {{PROMPT_FILE}} > got.txt"}
	big := strings.Repeat("x", 5000)
	if _, err := d.Run(context.Background(), brain.Spec{Prompt: big, Workdir: work}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(work, "got.txt"))
	if err != nil {
		t.Fatalf("prompt file missing: %v", err)
	}
	if string(raw) != big {
		t.Errorf("prompt file content mangled (%d bytes)", len(raw))
	}
}

func TestRequiresTemplateWithPrompt(t *testing.T) {
	d := &Driver{}
	if _, err := d.Run(context.Background(), brain.Spec{Workdir: t.TempDir()}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "AGENT_EXEC_COMMAND") {
		t.Fatalf("missing template error = %v", err)
	}

	t.Setenv("AGENT_EXEC_COMMAND", "true")
	if _, err := (&Driver{}).Run(context.Background(), brain.Spec{Workdir: t.TempDir()}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "{{PROMPT}}") {
		t.Fatalf("template without prompt error = %v", err)
	}
}

func TestEnvTemplateAndFailure(t *testing.T) {
	t.Setenv("AGENT_EXEC_COMMAND", "printf '{{PROMPT}}'; exit 9")
	res, err := (&Driver{}).Run(context.Background(), brain.Spec{Prompt: "p", Workdir: t.TempDir()}, &bytes.Buffer{})
	if err == nil || res.ExitCode != 9 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestTimeout(t *testing.T) {
	d := &Driver{Command: "sleep 5; : {{PROMPT}}"}
	start := time.Now()
	_, err := d.Run(context.Background(), brain.Spec{
		Prompt: "p", Workdir: t.TempDir(), Timeout: 150 * time.Millisecond,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("timeout did not kill the process group")
	}
}

func TestCapsClaimNothing(t *testing.T) {
	if c := New().Caps(); c != (brain.Caps{}) {
		t.Errorf("exec brain must not claim capabilities it cannot honour: %+v", c)
	}
}

func TestRegistered(t *testing.T) {
	if _, err := brain.Get("exec"); err != nil {
		t.Fatalf("exec brain not registered: %v", err)
	}
}
