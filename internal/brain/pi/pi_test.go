package pi

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

// fakePi puts a `pi` script on PATH that records its args and replays canned
// JSONL. The point is the real argument contract with the real CLI.
func fakePi(t *testing.T, script string) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PI_ARGS\"\n" + script
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	t.Setenv("PI_ARGS", argsFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Driver{Bin: "pi"}, argsFile
}

func TestRunPassesHeadlessApprovedArgs(t *testing.T) {
	d, argsFile := fakePi(t, `echo '{"type":"message_end","message":{"content":[{"type":"text","text":"done!"}]},"usage":{"totalTokens":42,"cost":{"total":0.0123}}}'
exit 0
`)
	var log bytes.Buffer
	res, err := d.Run(context.Background(), brain.Spec{
		Prompt:   "implement issue 39",
		Workdir:  t.TempDir(),
		Model:    "example/model-a",
		Thinking: "high",
		Timeout:  time.Minute,
	}, &log)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "done!" {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.CostUSD != 0.0123 {
		t.Errorf("cost = %v", res.CostUSD)
	}
	if res.ExitCode != 0 || res.Driver != "pi" {
		t.Errorf("result = %+v", res)
	}

	args := readLines(t, argsFile)
	for _, want := range []string{"--print", "--mode", "json", "--approve", "--model", "example/model-a", "--thinking", "high"} {
		if !contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if args[len(args)-1] != "implement issue 39" {
		t.Errorf("prompt must be the final positional arg: %v", args)
	}
	if !strings.Contains(log.String(), `"message_end"`) {
		t.Errorf("raw JSONL was not teed to the log: %q", log.String())
	}
}

func TestRunContinueAddsResumeFlag(t *testing.T) {
	d, argsFile := fakePi(t, "exit 0\n")
	if _, err := d.Run(context.Background(), brain.Spec{
		Prompt: "continue", Workdir: t.TempDir(), Continue: true,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(readLines(t, argsFile), "-c") {
		t.Error("-c not passed when Continue is set")
	}
}

func TestRunNonZeroExitIsAnError(t *testing.T) {
	d, _ := fakePi(t, `echo '{"type":"error","errorMessage":"model refused"}'
echo "boom" >&2
exit 3
`)
	res, err := d.Run(context.Background(), brain.Spec{Prompt: "x", Workdir: t.TempDir()}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a nonzero exit must be an error")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "model refused") {
		t.Errorf("error should carry the agent's error line: %v", err)
	}
}

func TestRunTimeoutKillsTheAgent(t *testing.T) {
	d, _ := fakePi(t, "sleep 5\nexit 0\n")
	start := time.Now()
	_, err := d.Run(context.Background(), brain.Spec{
		Prompt: "x", Workdir: t.TempDir(), Timeout: 150 * time.Millisecond,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout did not kill the child: %s", elapsed)
	}
}

func TestCapsAllowResumeAndModel(t *testing.T) {
	c := New().Caps()
	if !c.Resume || !c.Model || !c.Thinking || !c.Streaming {
		t.Errorf("caps = %+v", c)
	}
}

func TestRegisteredAsPi(t *testing.T) {
	d, err := brain.Get("pi")
	if err != nil {
		t.Fatalf("pi not registered: %v", err)
	}
	if d.Name() != "pi" {
		t.Errorf("registered driver = %q", d.Name())
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// A CLI that emits agent_settled and then refuses to exit is a real failure mode
// (observed with pi on the home box): the run must complete on the settle, not
// wait out the whole timeout.
func TestRunStopsAfterAgentSettled(t *testing.T) {
	d, _ := fakePi(t, `echo '{"type":"message_end","message":{"content":[{"type":"text","text":"finished"}]}}'
echo '{"type":"agent_settled","aborted":false}'
# Behave like pi: settle the run, then linger in an event loop.
sleep 120
exit 0
`)
	start := time.Now()
	res, err := d.Run(context.Background(), brain.Spec{
		Prompt: "x", Workdir: t.TempDir(), Timeout: 10 * time.Minute,
	}, &bytes.Buffer{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a settled run must not be an error: %v", err)
	}
	if !res.Settled {
		t.Error("result should report Settled")
	}
	if res.Summary != "finished" {
		t.Errorf("summary = %q", res.Summary)
	}
	if elapsed > 20*time.Second {
		t.Errorf("settle handling took %s; it should stop the CLI within a few seconds", elapsed)
	}
}

// Without a settle event the driver must still wait for the process, and a
// timeout must be reported as one.
func TestRunWithoutSettleHonoursTimeout(t *testing.T) {
	d, _ := fakePi(t, "sleep 30\n")
	start := time.Now()
	_, err := d.Run(context.Background(), brain.Spec{
		Prompt: "x", Workdir: t.TempDir(), Timeout: 300 * time.Millisecond,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("timeout did not stop the child promptly")
	}
}
