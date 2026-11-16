// Package execbrain runs any command as a brain.
//
// It is the escape hatch promised by the design: a CLI that does not deserve its
// own driver yet (or ever) can still be used by pointing a command template at it.
// The template is configuration, not code:
//
//	AGENT_EXEC_COMMAND='mytool --cwd {{WORKDIR}} --task {{PROMPT}}'
//
// The driver is deliberately dumb: no resume, no model, no thinking level. If a
// CLI needs those, it has earned a real driver package.
package execbrain

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

// Driver runs a configured shell command.
type Driver struct {
	// Command is the template. Reads AGENT_EXEC_COMMAND when empty.
	Command string
}

// New returns a driver bound to the environment's template.
func New() *Driver { return &Driver{} }

func init() { brain.Register(New()) }

// Name implements brain.Driver.
func (d *Driver) Name() string { return "exec" }

// Caps implements brain.Driver: a bare command understands none of it.
func (d *Driver) Caps() brain.Caps { return brain.Caps{} }

// template resolves the command to run.
func (d *Driver) template() (string, error) {
	tmpl := d.Command
	if tmpl == "" {
		tmpl = os.Getenv("AGENT_EXEC_COMMAND")
	}
	if strings.TrimSpace(tmpl) == "" {
		return "", errors.New("exec brain: set AGENT_EXEC_COMMAND (use {{PROMPT}} and {{WORKDIR}})")
	}
	if !strings.Contains(tmpl, "{{PROMPT}}") && !strings.Contains(tmpl, "{{PROMPT_FILE}}") {
		return "", errors.New("exec brain: the command template must include {{PROMPT}} or {{PROMPT_FILE}}")
	}
	return tmpl, nil
}

// Expand substitutes the placeholders. Exported for tests and for `issuewright
// status --verbose`, which shows exactly what would run.
func Expand(tmpl string, spec brain.Spec, promptFile string) string {
	r := strings.NewReplacer(
		"{{PROMPT}}", shellQuote(spec.Prompt),
		"{{PROMPT_FILE}}", shellQuote(promptFile),
		"{{WORKDIR}}", shellQuote(spec.Workdir),
		"{{MODEL}}", shellQuote(spec.Model),
	)
	return r.Replace(tmpl)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Run implements brain.Driver.
func (d *Driver) Run(ctx context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	tmpl, err := d.template()
	if err != nil {
		return brain.Result{Driver: "exec"}, err
	}
	// A 10 KB prompt as one shell argument is fragile; offer a file too.
	promptFile := ""
	if strings.Contains(tmpl, "{{PROMPT_FILE}}") {
		f, err := os.CreateTemp("", "issuewright-prompt-*.md")
		if err != nil {
			return brain.Result{Driver: "exec"}, err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(spec.Prompt); err != nil {
			f.Close()
			return brain.Result{Driver: "exec"}, err
		}
		if err := f.Close(); err != nil {
			return brain.Result{Driver: "exec"}, err
		}
		promptFile = f.Name()
	}
	command := Expand(tmpl, spec, promptFile)

	runCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	start := time.Now()
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = spec.Workdir
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdout = log
	cmd.Stderr = log

	res := brain.Result{Driver: "exec", Model: spec.Model}
	err = cmd.Run()
	res.Duration = time.Since(start)

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("exec brain: %w", err)
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("exec brain: timed out after %s", spec.Timeout)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("exec brain: exit %d", res.ExitCode)
	}
	return res, nil
}
