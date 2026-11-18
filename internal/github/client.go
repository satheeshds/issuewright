// Package github wraps the `gh` CLI. Everything issuewright knows about GitHub
// goes through this interface, so tests can run the whole pipeline against a
// fake `gh` script on PATH or an in-memory Client.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Issue is the subset of a GitHub issue issuewright needs.
type Issue struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	URL       string   `json:"url"`
	Labels    []string `json:"-"`
	Assignees []string `json:"-"`
	State     string   `json:"state"`
}

// PR is the subset of a pull request issuewright needs.
type PR struct {
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	URL        string   `json:"url"`
	HeadRef    string   `json:"headRefName"`
	BaseRef    string   `json:"baseRefName"`
	Draft      bool     `json:"isDraft"`
	State      string   `json:"state"`
	Author     string   `json:"-"`
	CheckState string   `json:"-"`
	Labels     []string `json:"-"`
}

// Client is the whole GitHub surface. Adding a method here is adding it to every
// implementation, which is the point: the engine never shells out itself.
type Client interface {
	ListAssignedIssues(ctx context.Context, slug, login string) ([]Issue, error)
	GetIssue(ctx context.Context, slug string, number int) (Issue, error)
	ListReviewRequestedPRs(ctx context.Context, slug, login string) ([]PR, error)
	GetPR(ctx context.Context, slug string, number int) (PR, error)
	FindPRByHead(ctx context.Context, slug, head string) (PR, bool, error)

	CreateDraftPR(ctx context.Context, slug, head, base, title, body string) (PR, error)
	UpdatePRBody(ctx context.Context, slug string, number int, body string) error
	ReviewPR(ctx context.Context, slug string, number int, body string) error

	Comment(ctx context.Context, slug string, number int, body string) error
	AddLabel(ctx context.Context, slug string, number int, label string) error
	RemoveLabel(ctx context.Context, slug string, number int, label string) error
	EnsureLabel(ctx context.Context, slug, label string) error
	Unassign(ctx context.Context, slug string, number int, login string) error

	PushBranch(ctx context.Context, dir, branch string) error
}

// CLI is the production Client: `gh` for GitHub, `git` for the push.
type CLI struct {
	GhBin  string
	GitBin string
	// Token, when set, is exported as GH_TOKEN for every gh call. Empty means the
	// machine's own `gh auth login` identity is used.
	Token string
	// Dir, when set, is the working directory for git operations.
	Dir string
}

// NewCLI returns a Client that shells out. An empty ghBin means "gh".
func NewCLI(ghBin string) *CLI {
	if ghBin == "" {
		ghBin = "gh"
	}
	return &CLI{GhBin: ghBin, GitBin: "git"}
}

func (c *CLI) env() []string {
	env := os.Environ()
	if c.Token != "" {
		env = append(env, "GH_TOKEN="+c.Token, "GITHUB_TOKEN="+c.Token)
	}
	return env
}

func (c *CLI) gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.GhBin, args...)
	cmd.Env = c.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// --- reads -----------------------------------------------------------------

type issueJSON struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
}

func (i issueJSON) toIssue() Issue {
	out := Issue{Number: i.Number, Title: i.Title, Body: i.Body, URL: i.URL, State: i.State}
	for _, l := range i.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, a := range i.Assignees {
		out.Assignees = append(out.Assignees, a.Login)
	}
	return out
}

const issueFields = "number,title,body,url,state,labels,assignees"

func (c *CLI) ListAssignedIssues(ctx context.Context, slug, login string) ([]Issue, error) {
	out, err := c.gh(ctx, "issue", "list", "--repo", slug, "--assignee", login,
		"--state", "open", "--limit", "100", "--json", issueFields)
	if err != nil {
		return nil, err
	}
	var raw []issueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		issues = append(issues, r.toIssue())
	}
	return issues, nil
}

func (c *CLI) GetIssue(ctx context.Context, slug string, number int) (Issue, error) {
	out, err := c.gh(ctx, "issue", "view", strconv.Itoa(number), "--repo", slug, "--json", issueFields)
	if err != nil {
		return Issue{}, err
	}
	var raw issueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return Issue{}, fmt.Errorf("parse issue %d: %w", number, err)
	}
	return raw.toIssue(), nil
}

type prJSON struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
	State       string `json:"state"`
}

func (p prJSON) toPR() PR {
	return PR{Number: p.Number, Title: p.Title, Body: p.Body, URL: p.URL,
		HeadRef: p.HeadRefName, BaseRef: p.BaseRefName, Draft: p.IsDraft, State: p.State}
}

const prFields = "number,title,body,url,headRefName,baseRefName,isDraft,state"

func (c *CLI) ListReviewRequestedPRs(ctx context.Context, slug, login string) ([]PR, error) {
	out, err := c.gh(ctx, "pr", "list", "--repo", slug, "--state", "open",
		"--search", "review-requested:"+login, "--limit", "50", "--json", prFields)
	if err != nil {
		return nil, err
	}
	var raw []prJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse pr list: %w", err)
	}
	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, r.toPR())
	}
	return prs, nil
}

func (c *CLI) GetPR(ctx context.Context, slug string, number int) (PR, error) {
	out, err := c.gh(ctx, "pr", "view", strconv.Itoa(number), "--repo", slug, "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var raw prJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return PR{}, fmt.Errorf("parse pr %d: %w", number, err)
	}
	return raw.toPR(), nil
}

func (c *CLI) FindPRByHead(ctx context.Context, slug, head string) (PR, bool, error) {
	out, err := c.gh(ctx, "pr", "list", "--repo", slug, "--head", head,
		"--state", "all", "--limit", "1", "--json", prFields)
	if err != nil {
		return PR{}, false, err
	}
	var raw []prJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return PR{}, false, fmt.Errorf("parse pr list: %w", err)
	}
	if len(raw) == 0 {
		return PR{}, false, nil
	}
	return raw[0].toPR(), true, nil
}

// --- writes ----------------------------------------------------------------

func (c *CLI) CreateDraftPR(ctx context.Context, slug, head, base, title, body string) (PR, error) {
	out, err := c.gh(ctx, "pr", "create", "--repo", slug, "--draft",
		"--head", head, "--base", base, "--title", title, "--body", body)
	if err != nil {
		return PR{}, err
	}
	url := strings.TrimSpace(string(out))
	number := 0
	if parts := strings.Split(strings.TrimSuffix(url, "/"), "/"); len(parts) > 0 {
		number, _ = strconv.Atoi(parts[len(parts)-1])
	}
	return PR{Number: number, URL: url, HeadRef: head, BaseRef: base, Draft: true}, nil
}

func (c *CLI) UpdatePRBody(ctx context.Context, slug string, number int, body string) error {
	_, err := c.gh(ctx, "pr", "edit", strconv.Itoa(number), "--repo", slug, "--body", body)
	return err
}

func (c *CLI) ReviewPR(ctx context.Context, slug string, number int, body string) error {
	_, err := c.gh(ctx, "pr", "review", strconv.Itoa(number), "--repo", slug,
		"--comment", "--body", body)
	return err
}

func (c *CLI) Comment(ctx context.Context, slug string, number int, body string) error {
	_, err := c.gh(ctx, "issue", "comment", strconv.Itoa(number), "--repo", slug, "--body", body)
	return err
}

func (c *CLI) AddLabel(ctx context.Context, slug string, number int, label string) error {
	_, err := c.gh(ctx, "issue", "edit", strconv.Itoa(number), "--repo", slug, "--add-label", label)
	return ignoreAlready(err)
}

func (c *CLI) RemoveLabel(ctx context.Context, slug string, number int, label string) error {
	_, err := c.gh(ctx, "issue", "edit", strconv.Itoa(number), "--repo", slug, "--remove-label", label)
	return ignoreMissing(err)
}

func (c *CLI) EnsureLabel(ctx context.Context, slug, label string) error {
	_, err := c.gh(ctx, "label", "create", label, "--repo", slug,
		"--color", "0e8a16", "--description", "managed by issuewright", "--force")
	return err
}

func (c *CLI) Unassign(ctx context.Context, slug string, number int, login string) error {
	_, err := c.gh(ctx, "issue", "edit", strconv.Itoa(number), "--repo", slug,
		"--remove-assignee", login)
	return ignoreMissing(err)
}

// PushBranch publishes a branch. Kept here (with the other publish operations)
// so a workflow's permissions are checked in one place.
func (c *CLI) PushBranch(ctx context.Context, dir, branch string) error {
	bin := c.GitBin
	if bin == "" {
		bin = "git"
	}
	// Same credential plumbing as the clone: git needs a helper to use GH_TOKEN.
	args := []string{"-C", dir}
	if c.Token != "" {
		args = append(args,
			"-c", "credential.helper=",
			"-c", "credential.https://github.com.helper=!gh auth git-credential")
	}
	args = append(args, "push", "--set-upstream", "origin", branch)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = c.env()
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push %s: %s", branch, strings.TrimSpace(string(out)))
	}
	return nil
}

func ignoreAlready(err error) error {
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func ignoreMissing(err error) error {
	if err != nil && (strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not have")) {
		return nil
	}
	return err
}

// ErrNotFound is returned by implementations when a numbered object is absent.
var ErrNotFound = errors.New("not found")
