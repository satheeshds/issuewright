// Package stub is a deterministic brain used by tests and by `--brain stub`.
//
// It makes the whole pipeline — worktree, prompt, publish, checks — runnable
// without a model, a network, or a token. Release builds exclude it with
// `-tags nostub`; see register_off.go.
package stub

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satheeshds/issuewright/internal/brain"
)

// Driver writes a deterministic change into the worktree.
type Driver struct {
	// File is the path (relative to the workdir) the stub writes.
	File string
	// Content overrides the file body.
	Content string
	// Fail makes every run fail, to exercise the failure path.
	Fail bool
}

// New returns the default stub.
func New() *Driver { return &Driver{File: "issuewright-stub.txt"} }

// Name implements brain.Driver.
func (d *Driver) Name() string { return "stub" }

// Caps implements brain.Driver: deterministic, so it can "resume" trivially and
// takes no model settings.
func (d *Driver) Caps() brain.Caps { return brain.Caps{Resume: true} }

// Run implements brain.Driver.
func (d *Driver) Run(ctx context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	res := brain.Result{Driver: "stub"}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	if d.Fail || os.Getenv("STUB_FAIL") == "1" {
		res.ExitCode = 1
		fmt.Fprintln(log, `{"type":"error","errorMessage":"stub failure requested"}`)
		return res, fmt.Errorf("stub brain: failure requested")
	}
	if os.Getenv("STUB_NOOP") == "1" {
		res.Summary = "stub: nothing to do"
		fmt.Fprintln(log, `{"type":"message_end","message":{"content":[{"type":"text","text":"stub: nothing to do"}]}}`)
		return res, nil
	}

	file := d.File
	if file == "" {
		file = "issuewright-stub.txt"
	}
	content := d.Content
	if content == "" {
		content = "written by the issuewright stub brain\n\nprompt bytes: " + fmt.Sprint(len(spec.Prompt)) + "\n"
	}
	target := filepath.Join(spec.Workdir, file)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return res, err
	}

	res.Summary = "stub: wrote " + file
	fmt.Fprintf(log, "{\"type\":\"message_end\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", res.Summary)
	return res, nil
}
