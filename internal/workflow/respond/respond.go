// Package respond is the review-comment workflow: it answers comments on a pull
// request — a mention of the bot, or a reply in a thread the bot opened — and, if
// the comment asks for a change and the branch is the runner's own, makes it.
//
// It is deliberately not part of `review`: answering has different permissions
// (a comment thread is not a code review) and a different prompt.
package respond

import (
	"fmt"

	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/workflow"
	"github.com/satheeshds/issuewright/prompts"
)

// Engine implements workflow.Workflow.
type Engine struct{}

// New returns the workflow.
func New() *Engine { return &Engine{} }

func init() { workflow.Register(New()) }

// Name implements workflow.Workflow.
func (e *Engine) Name() string { return "respond" }

// Triggers implements workflow.Workflow.
func (e *Engine) Triggers() []workflow.Trigger {
	return []workflow.Trigger{{Kind: workflow.ReviewComment}}
}

// Perms implements workflow.Workflow: answering is a comment, nothing more.
func (e *Engine) Perms() workflow.Perms {
	return workflow.Perms{Comment: true, Checks: true}
}

// PermsFor narrows Write to the case that needs it: the comment asks for a change
// and the pull request's branch is one this runner created. A human's branch is
// never pushed to, whatever the comment says.
func (e *Engine) PermsFor(job workflow.Job) workflow.Perms {
	p := e.Perms()
	if job.OwnBranch {
		p.Write = true
	}
	return p
}

// Prompt implements workflow.Workflow.
func (e *Engine) Prompt(job workflow.Job) (brain.Spec, error) {
	if job.Number == 0 || len(job.Comments) == 0 {
		return brain.Spec{}, workflow.ErrNoJob
	}
	comments := make([]prompts.JobComment, 0, len(job.Comments))
	for _, c := range job.Comments {
		comments = append(comments, prompts.JobComment{
			ID: c.ID, Author: c.Author, Body: c.Body, Path: c.Path, Line: c.Line, IsReview: c.IsReview,
		})
	}
	text, err := prompts.Render("respond", prompts.Inputs{
		Repo:        job.Repo,
		Number:      job.Number,
		Title:       job.Title,
		Body:        job.Body,
		Branch:      job.Branch,
		BaseBranch:  job.BaseBranch,
		Workdir:     job.Workdir,
		Kind:        string(job.Kind),
		Rules:       job.Rules,
		SecretPaths: job.Protect.SecretPaths,
		Notes:       job.Notes,
		Comments:    comments,
		OwnBranch:   job.OwnBranch,
	})
	if err != nil {
		return brain.Spec{}, fmt.Errorf("respond prompt: %w", err)
	}
	return brain.Spec{Prompt: text, Workdir: job.Workdir}, nil
}
