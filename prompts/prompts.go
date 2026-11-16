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
	)
	return r.Replace(tmpl)
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
