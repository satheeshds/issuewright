// Package pi is the driver for the Pi coding agent CLI.
//
// Pi is run headless: one prompt in, a JSONL event stream out, a nonzero exit
// when the run failed or was aborted. `--approve` is what makes the repo's own
// AGENTS.md and .pi rules take effect in a non-interactive run.
package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/satheeshds/issuewright/internal/brain"
)

// Driver runs `pi`.
type Driver struct {
	// Bin defaults to "pi".
	Bin string
	// ExtraArgs are appended before the prompt (escape hatch, not config).
	ExtraArgs []string
}

// New returns the default driver.
func New() *Driver { return &Driver{Bin: "pi"} }

func init() { brain.Register(New()) }

// Name implements brain.Driver.
func (d *Driver) Name() string { return "pi" }

// Caps implements brain.Driver. Pi exposes all four capabilities, and it can name
// a conversation: `--session-id <id>` opens the directory's session with that id,
// creating it when it does not exist yet. That is what lets one issue keep one
// agent across ticks.
func (d *Driver) Caps() brain.Caps {
	return brain.Caps{Resume: true, Conversation: true, Model: true, Thinking: true, Streaming: true}
}

// Run implements brain.Driver.
func (d *Driver) Run(ctx context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	bin := d.Bin
	if bin == "" {
		bin = "pi"
	}
	args := []string{"--print", "--mode", "json", "--approve"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Thinking != "" {
		args = append(args, "--thinking", spec.Thinking)
	}
	// A named conversation beats "the latest session here": it survives a
	// second worktree, a fork, and a human opening pi in the same directory.
	if spec.Conversation != "" {
		args = append(args, "--session-id", spec.Conversation)
	} else if spec.Continue {
		args = append(args, "-c")
	}
	args = append(args, d.ExtraArgs...)
	args = append(args, spec.Prompt)

	runCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	start := time.Now()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = spec.Workdir
	cmd.Env = append(os.Environ(), "CI=1")
	// Pi spawns tool subprocesses. Without a process group, cancelling on
	// timeout kills only the agent and leaves the tools holding the pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Never block forever on a descendant that outlived its parent.
	cmd.WaitDelay = 10 * time.Second

	tap := newEventTap(log)
	cmd.Stdout = tap
	// writeOnly matters: handing a *bytes.Buffer straight to exec lets io.Copy
	// pick bytes.Buffer.ReadFrom, which resets the buffer under the stdout tap.
	cmd.Stderr = writeOnly{log}

	res := brain.Result{Driver: "pi", Model: spec.Model, Thinking: spec.Thinking}

	// `agent_settled` means the run is over, but the CLI may keep its event loop
	// alive afterwards (observed on the box: the process sits in epoll with no
	// children). Waiting for exit would burn the whole run timeout, so treat
	// settled as completion and stop the process ourselves, with a short grace
	// period for the final flush.
	done := make(chan struct{})
	err := cmd.Start()
	if err == nil {
		go func() {
			select {
			case <-tap.settled:
				select {
				case <-done:
				case <-time.After(settleGrace):
					if cmd.Process != nil {
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
					}
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						if cmd.Process != nil {
							_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
						}
					}
				}
			case <-done:
			}
		}()
		err = cmd.Wait()
	}
	close(done)
	res.Duration = time.Since(start)
	res.Settled = tap.settledSeen.Load()
	res.CostUSD = tap.cost
	res.Summary = tap.lastText

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("pi: %w", err)
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("pi: timed out after %s", spec.Timeout)
	}
	// A settled run that we terminated ourselves is a success, not a signal death.
	if res.Settled {
		res.ExitCode = 0
		return res, nil
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("pi: exit %d (%s)", res.ExitCode, firstLine(tap.lastError))
	}
	return res, nil
}

// writeOnly hides the destination's optimised methods (ReadFrom/WriteTo) from
// io.Copy, so a caller-supplied writer sees exactly the bytes the child wrote.
type writeOnly struct{ w io.Writer }

func (w writeOnly) Write(p []byte) (int, error) {
	if w.w == nil {
		return len(p), nil
	}
	return w.w.Write(p)
}

// eventTap writes the raw JSONL through to the run log while extracting the two
// things the runner needs: the final assistant text and the reported cost.
// settleGrace is how long pi gets to flush after agent_settled before we stop it.
const settleGrace = 3 * time.Second

type eventTap struct {
	out    io.Writer
	cost   float64
	tokens int
	// settled is signalled once agent_settled arrives, which is the documented
	// end of automatic work for a run.
	settled     chan struct{}
	settledOnce sync.Once
	settledSeen atomic.Bool
	// lastText is the final assistant message text seen.
	lastText string
	// lastError is the last error event's message, for a useful failure line.
	lastError string
	buf       []byte
}

func newEventTap(out io.Writer) *eventTap {
	if out == nil {
		out = io.Discard
	}
	return &eventTap{out: out, settled: make(chan struct{})}
}

func (t *eventTap) Write(p []byte) (int, error) {
	n, err := t.out.Write(p)
	t.buf = append(t.buf, p...)
	for {
		i := indexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := t.buf[:i]
		t.buf = t.buf[i+1:]
		t.consume(line)
	}
	return n, err
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

type event struct {
	Type    string `json:"type"`
	Usage   *usage `json:"usage"`
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	ErrorMessage string `json:"errorMessage"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type usage struct {
	TotalTokens int `json:"totalTokens"`
	Cost        struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func (t *eventTap) consume(line []byte) {
	line = trimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev event
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	if ev.Usage != nil {
		if ev.Usage.Cost.Total > 0 {
			t.cost = ev.Usage.Cost.Total
		}
		if ev.Usage.TotalTokens > 0 {
			t.tokens = ev.Usage.TotalTokens
		}
	}
	if ev.Message != nil {
		var b strings.Builder
		for _, c := range ev.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		if b.Len() > 0 {
			t.lastText = b.String()
		}
	}
	if ev.Type == "agent_settled" {
		t.settledSeen.Store(true)
		t.settledOnce.Do(func() { close(t.settled) })
	}
	if ev.ErrorMessage != "" {
		t.lastError = ev.ErrorMessage
	}
	if ev.Error != nil && ev.Error.Message != "" {
		t.lastError = ev.Error.Message
	}
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
