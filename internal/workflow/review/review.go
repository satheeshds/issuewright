// Package review is the pull-request → review-comment workflow.
//
// It exists as a real second workflow for a reason stated in the design: a plugin
// interface with one implementation is a guess. Review is the one that proves
// Perms works — it cannot write, and the engine enforces that rather than asking.
package review

import (
	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/workflow"
	"github.com/satheeshds/issuewright/prompts"
)

// Engine implements workflow.Workflow for reviewing a pull request.
type Engine struct{}

// New returns the workflow.
func New() *Engine { return &Engine{} }

func init() { workflow.Register(New()) }

// Name implements workflow.Workflow.
func (e *Engine) Name() string { return "review" }

// Triggers implements workflow.Workflow: a PR opened, or a comment mentioning the
// bot on an existing PR.
func (e *Engine) Triggers() []workflow.Trigger {
	return []workflow.Trigger{
		{Kind: workflow.PROpened},
		{Kind: workflow.CommentMention},
	}
}

// Perms implements workflow.Workflow. Note what is missing: Write.
func (e *Engine) Perms() workflow.Perms {
	return workflow.Perms{Write: false, Comment: true, Label: false, Checks: true}
}

// Prompt implements workflow.Workflow.
func (e *Engine) Prompt(job workflow.Job) (brain.Spec, error) {
	if job.Number == 0 {
		return brain.Spec{}, workflow.ErrNoJob
	}
	text, err := prompts.Render("review", prompts.Inputs{
		Repo:        job.Repo,
		Number:      job.Number,
		Title:       job.Title,
		Body:        job.Body,
		Labels:      job.Labels,
		Branch:      job.Branch,
		BaseBranch:  job.BaseBranch,
		Workdir:     job.Workdir,
		Kind:        string(job.Kind),
		Rules:       job.Rules,
		SecretPaths: job.Protect.SecretPaths,
		Notes:       job.Notes,
	})
	if err != nil {
		return brain.Spec{}, err
	}
	return brain.Spec{Prompt: text, Workdir: job.Workdir}, nil
}
