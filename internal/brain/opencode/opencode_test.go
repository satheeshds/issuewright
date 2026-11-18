package opencode

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

func fakeOpenCode(t *testing.T, script string) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OC_ARGS\"\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	t.Setenv("OC_ARGS", argsFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Driver{Bin: "opencode"}, argsFile
}

func TestRunInvokesOpenCodeRun(t *testing.T) {
	d, argsFile := fakeOpenCode(t, "echo all done\nexit 0\n")
	var log bytes.Buffer
	res, err := d.Run(context.Background(), brain.Spec{
		Prompt:  "implement #39",
		Workdir: t.TempDir(),
		Model:   "example/model-a",
		Timeout: time.Minute,
	}, &log)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Driver != "opencode" || res.ExitCode != 0 {
		t.Errorf("result = %+v", res)
	}
	if res.Summary != "all done" {
		t.Errorf("summary = %q", res.Summary)
	}
	args := strings.Split(strings.TrimSpace(readFile(t, argsFile)), "\n")
	if len(args) < 3 || args[0] != "run" || args[1] != "--auto" {
		t.Fatalf("args = %v", args)
	}
	if !contains(args, "--model") || !contains(args, "example/model-a") {
		t.Errorf("model not passed: %v", args)
	}
	if args[len(args)-1] != "implement #39" {
		t.Errorf("prompt must be last: %v", args)
	}
	if !strings.Contains(log.String(), "all done") {
		t.Errorf("log did not capture output: %q", log.String())
	}
}

func TestRunContinueAndAgent(t *testing.T) {
	d, argsFile := fakeOpenCode(t, "exit 0\n")
	d.Agent = "build"
	if _, err := d.Run(context.Background(), brain.Spec{Prompt: "x", Workdir: t.TempDir(), Continue: true}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := strings.Split(strings.TrimSpace(readFile(t, argsFile)), "\n")
	if !contains(args, "--continue") || !contains(args, "--agent") {
		t.Errorf("args = %v", args)
	}
}

func TestRunFailureAndTimeout(t *testing.T) {
	d, _ := fakeOpenCode(t, "echo nope >&2\nexit 7\n")
	res, err := d.Run(context.Background(), brain.Spec{Prompt: "x", Workdir: t.TempDir()}, &bytes.Buffer{})
	if err == nil || res.ExitCode != 7 {
		t.Fatalf("err=%v res=%+v", err, res)
	}

	slow, _ := fakeOpenCode(t, "sleep 5\nexit 0\n")
	if _, err := slow.Run(context.Background(), brain.Spec{
		Prompt: "x", Workdir: t.TempDir(), Timeout: 150 * time.Millisecond,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestCapsDoNotClaimThinking(t *testing.T) {
	c := New().Caps()
	if c.Thinking {
		t.Error("opencode has no reasoning budget flag; Caps.Thinking must be false")
	}
	if !c.Resume || !c.Model {
		t.Errorf("caps = %+v", c)
	}
}

func TestRegistered(t *testing.T) {
	d, err := brain.Get("opencode")
	if err != nil || d.Name() != "opencode" {
		t.Fatalf("opencode not registered: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
