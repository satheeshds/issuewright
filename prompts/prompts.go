// Package prompts renders the instruction a brain receives.
//
// Templates are embedded in the binary, so the box has one artifact to install.
// Everything repo-specific arrives through Inputs: this package never knows which
// repo it is talking about.
package prompts

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed workflows/*.md summary.md
var files embed.FS

// Artifact names a run writes into its worktree. They are the runner's channel
// from the agent back to GitHub, and are removed before anything is pushed.
const (
	SummaryFile = "SUMMARY.md"
	BlockedFile = "BLOCKED.md"
	ReviewFile  = "REVIEW.md"
)

// Inputs is everything a template may interpolate.
type Inputs struct {
	Repo       string
	Number     int
	Title      string
	Body       string
	Labels     []string
	Branch     string
	BaseBranch string
	Workdir    string
	Kind       string
	Rules      string
	// ExistingWork is a commit summary, used when the brain cannot resume.
	ExistingWork string

	WriteDenylist []string
	NeverRun      []string
	SecretPaths   []string
	Notes         []string

	// Comments are the review comments this run answers (respond workflow).
	Comments []JobComment
	// OwnBranch means the branch belongs to this runner, so a fix may be pushed.
	OwnBranch bool
}

// JobComment is one comment a respond run is answering.
type JobComment struct {
	ID       int
	Author   string
	Body     string
	Path     string
	Line     int
	IsReview bool
}

// Render returns the prompt for a workflow.
func Render(workflow string, in Inputs) (string, error) {
	raw, err := files.ReadFile("workflows/" + workflow + ".md")
	if err != nil {
		return "", fmt.Errorf("no prompt template for workflow %q: %w", workflow, err)
	}
	return interpolate(string(raw), in), nil
}

// Summary is the PR/comment skeleton the runner fills in.
func Summary() string {
	raw, err := files.ReadFile("summary.md")
	if err != nil {
		return ""
	}
	return string(raw)
}

func interpolate(tmpl string, in Inputs) string {
	body := in.Body
	if strings.TrimSpace(body) == "" {
		body = "(the issue body is empty — infer the task from the title, and if that is not enough, use " + BlockedFile + ")"
	}
	existing := in.ExistingWork
	if strings.TrimSpace(existing) == "" {
		existing = "(none yet)"
	}
	notes := bulletOrNone(in.Notes)
	r := strings.NewReplacer(
		"{{REPO}}", in.Repo,
		"{{NUMBER}}", fmt.Sprint(in.Number),
		"{{TITLE}}", in.Title,
		"{{BODY}}", body,
		"{{LABELS}}", strings.Join(in.Labels, ", "),
		"{{BRANCH}}", in.Branch,
		"{{BASE_BRANCH}}", in.BaseBranch,
		"{{WORKDIR}}", in.Workdir,
		"{{KIND}}", in.Kind,
		"{{RULES}}", strings.TrimSpace(in.Rules),
		"{{EXISTING_WORK}}", existing,
		"{{WRITE_DENYLIST}}", bulletOrNone(in.WriteDenylist),
		"{{NEVER_RUN}}", bulletOrNone(in.NeverRun),
		"{{SECRET_PATHS}}", bulletOrNone(in.SecretPaths),
		"{{NOTES}}", notes,
		"{{SUMMARY_FILE}}", SummaryFile,
		"{{BLOCKED_FILE}}", BlockedFile,
		"{{REVIEW_FILE}}", ReviewFile,
		"{{COMMENTS}}", renderComments(in.Comments),
		"{{OWN_BRANCH}}", ownBranchNote(in.OwnBranch),
	)
	return r.Replace(tmpl)
}

// renderComments lays the comments out so the model can quote ids back exactly.
func renderComments(comments []JobComment) string {
	if len(comments) == 0 {
		return "(no comments)"
	}
	var b strings.Builder
	for i, c := range comments {
		where := "conversation comment"
		if c.IsReview {
			where = fmt.Sprintf("inline comment on %s:%d", c.Path, c.Line)
		}
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "\n### comment %d — %s (%s)\n\n```\n%s\n```\n",
			c.ID, c.Author, where, strings.TrimSpace(c.Body))
	}
	return b.String()
}

func ownBranchNote(own bool) string {
	if own {
		return "This branch belongs to you, so you may change code and push to it."
	}
	return "This branch belongs to a human. You must not change code or push: answer with words, and put any suggested change in a fenced ```suggestion block."
}

func bulletOrNone(items []string) string {
	if len(items) == 0 {
		return "(none declared)"
	}
	lines := make([]string, 0, len(items))
	for _, i := range items {
		lines = append(lines, "- "+i)
	}
	return strings.Join(lines, "\n")
}
