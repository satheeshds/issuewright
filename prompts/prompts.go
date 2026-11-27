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
	// QuestionFile is the non-terminal sibling of BlockedFile: the agent has done
	// everything it is sure about and needs an answer to carry on. The runner posts
	// it as a comment, marks the conversation "asking", and waits for a human
	// instead of failing the run.
	QuestionFile = "QUESTION.md"
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

	// Comments are the comments this run answers (respond, or a conversation's
	// next turn).
	Comments []JobComment
	// OwnBranch means the branch belongs to this runner, so a fix may be pushed.
	OwnBranch bool
	// Turn is which turn of the conversation this run is: 1 is the first, a
	// number above 1 means the agent has history to continue from.
	Turn int
	// LastReview is the review this same conversation produced for an earlier
	// revision of the pull request (review conversations only).
	LastReview string
	// LastQuestion is what this agent asked before it stopped, when the turn being
	// built is the answer to it.
	LastQuestion string
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
		"{{QUESTION_FILE}}", QuestionFile,
		"{{COMMENTS}}", renderComments(in.Comments),
		"{{OWN_BRANCH}}", ownBranchNote(in.OwnBranch),
		"{{CONVERSATION_NOTE}}", conversationNote(in.Turn, in.LastQuestion),
		"{{LAST_REVIEW}}", lastReview(in.LastReview),
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

// conversationNote tells the model where it is in a conversation that outlives
// this process. A first turn says so; a later turn says the history is real and
// must not be re-derived.
func conversationNote(turn int, lastQuestion string) string {
	var b strings.Builder
	if turn <= 1 {
		b.WriteString("This is the first turn on this target. Read it carefully and decide what \"done\" means.")
	} else {
		b.WriteString("This is turn " + fmt.Sprint(turn) + " on this target, and you are the same agent that took the " +
			"earlier turns: they are in your history, including what a human told you and why you chose what you did. " +
			"Continue from them. Do not re-derive settled decisions, do not redo finished work, and do not contradict " +
			"an earlier turn without saying plainly that you changed your mind and why.")
	}
	if q := strings.TrimSpace(lastQuestion); q != "" {
		b.WriteString("\n\nIn your last turn you stopped and asked:\n\n> " +
			strings.ReplaceAll(q, "\n", "\n> ") +
			"\n\nThe comment below is the answer to that question. Treat it as the decision, and carry on from it.")
	}
	return b.String()
}

func lastReview(review string) string {
	if strings.TrimSpace(review) == "" {
		return "(you have not reviewed this pull request before)"
	}
	return strings.TrimSpace(review)
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
