package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/satheeshds/issuewright/internal/config"
)

// Options configure a Runner.
type Options struct {
	// Dir is the worktree the commands run in.
	Dir string
	// Log receives command output as it happens (the run log).
	Log io.Writer
	// Timeout bounds each command; zero means 30 minutes.
	Timeout time.Duration
	// Path is the PATH the commands run with, defaulting to the process's. Kept so a
	// "tool not found" failure can name it.
	Path string
	// GitBin defaults to "git".
	GitBin string
}

// Runner executes check commands and applies drift rules.
type Runner struct {
	opt Options
}

// New returns a Runner.
func New(opt Options) *Runner {
	if opt.Timeout == 0 {
		opt.Timeout = 30 * time.Minute
	}
	if opt.GitBin == "" {
		opt.GitBin = "git"
	}
	if opt.Path == "" {
		opt.Path = os.Getenv("PATH")
	}
	return &Runner{opt: opt}
}

// Select returns the checks that apply to the changed paths.
func Select(prof *config.Profile, changed []string) []config.Check {
	if prof == nil {
		return nil
	}
	var out []config.Check
	for _, c := range prof.Checks {
		for _, path := range changed {
			if MatchAny(c.Paths, path) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// Result is the outcome of one check.
type Result struct {
	Name     string
	Commands []string
	OK       bool
	// Advisory failures are reported in the PR body but do not block the run.
	Advisory bool
	// Failures holds one entry per reason the check failed.
	Failures []string
	Duration time.Duration
}

// RunCheck executes a check's commands in order, stopping at the first failure,
// then applies the drift gate.
func (r *Runner) RunCheck(ctx context.Context, c config.Check) Result {
	start := time.Now()
	res := Result{Name: c.Name, Commands: c.Run, Advisory: c.Advisory}

	for _, cmd := range c.Run {
		output, err := r.shell(ctx, cmd)
		if err != nil {
			failure := fmt.Sprintf("%s: %v%s", cmd, err, tail(output))
			if tool, ok := missingTool(output); ok {
				// `sh: 1: go: not found` is not a failing test, it is a broken PATH on
				// the machine running the check. Say that, with the PATH in question.
				failure += fmt.Sprintf("\n    %s is not on this run's PATH (%s).", tool, r.opt.Path)
				failure += fmt.Sprintf("\n    Add its directory to PATH in the service environment" +
					" (e.g. /srv/agent/agent.env) — a PATH there replaces the unit's, so include the rest.")
			}
			res.Failures = append(res.Failures, failure)
			res.Duration = time.Since(start)
			return res
		}
	}

	if len(c.Drift) > 0 {
		dirty, err := r.dirtyFiles(ctx, c.Drift)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("drift check: %v", err))
			res.Duration = time.Since(start)
			return res
		}
		if len(dirty) > 0 {
			res.Failures = append(res.Failures, fmt.Sprintf(
				"these generated files still differ after %q: %s — they were edited by hand instead of regenerated",
				strings.Join(c.Run, " && "), strings.Join(dirty, ", ")))
			res.Duration = time.Since(start)
			return res
		}
	}

	res.OK = true
	res.Duration = time.Since(start)
	return res
}

func (r *Runner) shell(ctx context.Context, command string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, r.opt.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = r.opt.Dir
	// Replace PATH rather than appending a second one: a duplicate leaves the child
	// with the original, which is how a "not found" test was passing for the wrong
	// reason.
	cmd.Env = environWithPath(r.opt.Path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	var buf bytes.Buffer
	out := io.Writer(&buf)
	if r.opt.Log != nil {
		out = io.MultiWriter(&buf, r.opt.Log)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	// writeOnly keeps io.Copy from using ReadFrom on a shared buffer.
	cmd.Stdout = writeOnly{out}
	cmd.Stderr = writeOnly{out}

	if err := cmd.Run(); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return buf.String(), fmt.Errorf("timed out after %s", r.opt.Timeout)
		}
		return buf.String(), err
	}
	return buf.String(), nil
}

type writeOnly struct{ w io.Writer }

func (w writeOnly) Write(p []byte) (int, error) { return w.w.Write(p) }

// dirtyFiles lists uncommitted changes matching the patterns.
func (r *Runner) dirtyFiles(ctx context.Context, patterns []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, r.opt.GitBin, "-C", r.opt.Dir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var dirty []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if MatchAny(patterns, path) {
			dirty = append(dirty, path)
		}
	}
	return dirty, nil
}

// RunAll runs every selected check.
func (r *Runner) RunAll(ctx context.Context, selected []config.Check) []Result {
	results := make([]Result, 0, len(selected))
	for _, c := range selected {
		results = append(results, r.RunCheck(ctx, c))
	}
	return results
}

// Failed reports whether any blocking check failed. Advisory checks are reported
// but never fail a run.
func Failed(results []Result) bool {
	for _, r := range results {
		if !r.OK && !r.Advisory {
			return true
		}
	}
	return false
}

// Summary renders check results for the PR body or a comment.
func Summary(results []Result) string {
	if len(results) == 0 {
		return "no checks matched the changed paths"
	}
	var b strings.Builder
	for _, r := range results {
		switch {
		case r.OK:
			fmt.Fprintf(&b, "- ✅ `%s` — %s\n", r.Name, strings.Join(r.Commands, " && "))
		case r.Advisory:
			fmt.Fprintf(&b, "- ⚠️ `%s` (advisory — the repo's own baseline fails this; it does not block)\n", r.Name)
			for _, f := range r.Failures {
				fmt.Fprintf(&b, "  - %s\n", f)
			}
		default:
			fmt.Fprintf(&b, "- ❌ `%s`\n", r.Name)
			for _, f := range r.Failures {
				fmt.Fprintf(&b, "  - %s\n", f)
			}
		}
	}
	return b.String()
}

// ErrNoShell is returned when the machine has no shell, which would make every
// check silently "pass".
var ErrNoShell = errors.New("checks: no /bin/sh available")

// environWithPath returns the current environment with PATH replaced.
func environWithPath(path string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+path)
}

// missingTool reads `sh: 1: go: not found` (or the bash spelling) out of a command's
// output. Only that shape counts: a test that prints "not found" is a real failure.
func missingTool(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{": not found", ": command not found"} {
			if i := strings.Index(line, marker); i > 0 {
				prefix := line[:i]
				if j := strings.LastIndex(prefix, ": "); j >= 0 {
					prefix = prefix[j+2:]
				}
				tool := strings.TrimSpace(prefix)
				if tool != "" && !strings.Contains(tool, " ") {
					return tool, true
				}
			}
		}
	}
	return "", false
}

// tail keeps the last few lines of command output for a comment or PR body;
// the full output is already in the run log.
func tail(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return "\n" + strings.Join(lines, "\n")
}
