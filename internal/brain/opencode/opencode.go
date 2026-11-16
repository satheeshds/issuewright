// Package opencode is the driver for the OpenCode CLI (`opencode run`).
//
// It exists for two reasons: to give a second brain when a model or quota suits
// it better, and to prove the brain seam is real rather than a wrapper around
// one binary.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/satheeshds/issuewright/internal/brain"
)

// Driver runs `opencode run`.
type Driver struct {
	// Bin defaults to "opencode".
	Bin string
	// Agent, when set, is passed as --agent (e.g. "build").
	Agent string
	// ExtraArgs are appended before the message.
	ExtraArgs []string
}

// New returns the default driver.
func New() *Driver { return &Driver{Bin: "opencode"} }

func init() { brain.Register(New()) }

// Name implements brain.Driver.
func (d *Driver) Name() string { return "opencode" }

// Caps implements brain.Driver.
//
// opencode resumes sessions and takes a model, but its thinking level is a
// display flag rather than a reasoning budget, so Thinking is false: the runner
// then knows not to claim a thinking level was applied.
func (d *Driver) Caps() brain.Caps {
	return brain.Caps{Resume: true, Model: true, Thinking: false, Streaming: false}
}

// Run implements brain.Driver.
func (d *Driver) Run(ctx context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	bin := d.Bin
	if bin == "" {
		bin = "opencode"
	}
	args := []string{"run", "--auto"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Continue {
		args = append(args, "--continue")
	}
	if d.Agent != "" {
		args = append(args, "--agent", d.Agent)
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second

	var out strings.Builder
	w := io.MultiWriter(log, &out)
	cmd.Stdout = w
	cmd.Stderr = writeOnly{log}

	res := brain.Result{Driver: "opencode", Model: spec.Model}
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Summary = strings.TrimSpace(out.String())

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("opencode: %w", err)
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("opencode: timed out after %s", spec.Timeout)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("opencode: exit %d", res.ExitCode)
	}
	return res, nil
}

// writeOnly keeps io.Copy from using a destination's ReadFrom/WriteTo
// optimisation, which can reset a caller's buffer under a concurrent writer.
type writeOnly struct{ w io.Writer }

func (w writeOnly) Write(p []byte) (int, error) {
	if w.w == nil {
		return len(p), nil
	}
	return w.w.Write(p)
}
