// Package implement is the issue → branch → draft PR workflow.
package implement

import (
	"fmt"

	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/workflow"
	"github.com/satheeshds/issuewright/prompts"
)

// Engine implements workflow.Workflow for building an issue.
type Engine struct{}

// New returns the workflow.
func New() *Engine { return &Engine{} }

func init() { workflow.Register(New()) }

// Name implements workflow.Workflow.
func (e *Engine) Name() string { return "implement" }

// Triggers implements workflow.Workflow: assigning the issue to the bot, or
// labelling it with the profile's force label.
func (e *Engine) Triggers() []workflow.Trigger {
	return []workflow.Trigger{
		{Kind: workflow.IssueAssigned},
		{Kind: workflow.IssueLabel},
	}
}

// Perms implements workflow.Workflow. Implementing writes.
func (e *Engine) Perms() workflow.Perms {
	return workflow.Perms{Write: true, Comment: true, Label: true, Checks: true}
}

// Prompt implements workflow.Workflow.
func (e *Engine) Prompt(job workflow.Job) (brain.Spec, error) {
	if job.Number == 0 {
		return brain.Spec{}, workflow.ErrNoJob
	}
	if job.Resume {
		return brain.Spec{
			Prompt:  resumePrompt(job),
			Workdir: job.Workdir,
		}, nil
	}
	text, err := prompts.Render("implement", inputs(job))
	if err != nil {
		return brain.Spec{}, err
	}
	return brain.Spec{Prompt: text, Workdir: job.Workdir}, nil
}

// resumePrompt is what a killed run gets on the next tick: short, because the
// session (and the branch) already carry the context.
func resumePrompt(job workflow.Job) string {
	return fmt.Sprintf(`Continue and finish issue #%d of %s: "%s".

You are still in %s on branch %s. Work already on the branch:

%s

Pick up from where that stopped. Do not redo completed steps, do not rewrite
history that is already pushed. Finish the implementation, verify it with the
repo's own commands, commit what remains, and write %s (see the original
instructions in this session).

If the work is actually complete, just write %s and stop.
`, job.Number, job.Repo, job.Title, job.Workdir, job.Branch,
		orNone(job.ExistingWork), prompts.SummaryFile, prompts.SummaryFile)
}

func inputs(job workflow.Job) prompts.Inputs {
	return prompts.Inputs{
		Repo:          job.Repo,
		Number:        job.Number,
		Title:         job.Title,
		Body:          job.Body,
		Labels:        job.Labels,
		Branch:        job.Branch,
		BaseBranch:    job.BaseBranch,
		Workdir:       job.Workdir,
		Kind:          string(job.Kind),
		Rules:         job.Rules,
		ExistingWork:  job.ExistingWork,
		WriteDenylist: job.Protect.WriteDenylist,
		NeverRun:      job.Protect.NeverRun,
		SecretPaths:   job.Protect.SecretPaths,
		Notes:         job.Notes,
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}
