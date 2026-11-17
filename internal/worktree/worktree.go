// Package worktree owns the git side of a run: one dedicated clone per repo,
// one worktree per issue (or per PR review), and the branch lifecycle.
//
// The clone is never a deploy checkout. Nothing here touches the repo the human
// works in.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Options configure a Manager.
type Options struct {
	// CloneDir is the agent's own full clone of the repo.
	CloneDir string
	// WorkRoot holds one directory per issue.
	WorkRoot string
	// BaseBranch is what branches are cut from and diffed against.
	BaseBranch string
	// BranchPrefix is prepended to "issue-<n>" and "review-<n>".
	BranchPrefix string
	// CloneURL, when set, is cloned with git instead of `gh repo clone`.
	CloneURL string
	// Token, when set, makes the clone go over HTTPS with that token. Without it
	// the clone uses `gh repo clone`, which follows the account's git protocol —
	// and an SSH account with no key gives "Host key verification failed" on a
	// machine that has never seen github.com.
	Token string
	// GhBin is the gh binary used as a git credential helper (default "gh").
	GhBin string
	// GitBin defaults to "git".
	GitBin string
}

// Manager is the git workstation for one repo.
type Manager struct {
	opt Options
}

// New returns a Manager, filling defaults.
func New(opt Options) *Manager {
	if opt.BaseBranch == "" {
		opt.BaseBranch = "main"
	}
	if opt.BranchPrefix == "" {
		opt.BranchPrefix = "agent/"
	}
	if opt.GitBin == "" {
		opt.GitBin = "git"
	}
	return &Manager{opt: opt}
}

// CloneDir returns the clone path.
func (m *Manager) CloneDir() string { return m.opt.CloneDir }

// BaseBranch returns the base branch.
func (m *Manager) BaseBranch() string { return m.opt.BaseBranch }

// IssueBranch is the branch name for an issue.
func (m *Manager) IssueBranch(number int) string {
	return m.opt.BranchPrefix + "issue-" + strconv.Itoa(number)
}

// ReviewPath is the read-only worktree path for a PR review.
func (m *Manager) ReviewPath(number int) string {
	return filepath.Join(m.opt.WorkRoot, "review-"+strconv.Itoa(number))
}

// IssuePath is the worktree path for an issue.
func (m *Manager) IssuePath(number int) string {
	return filepath.Join(m.opt.WorkRoot, "issue-"+strconv.Itoa(number))
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, m.opt.GitBin, full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// EnsureClone creates the dedicated clone on first use, then fetches.
func (m *Manager) EnsureClone(ctx context.Context, slug string) error {
	if _, err := os.Stat(filepath.Join(m.opt.CloneDir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(m.opt.CloneDir), 0o755); err != nil {
			return err
		}
		switch {
		case m.opt.CloneURL != "":
			if _, err := m.git(ctx, "", "clone", m.opt.CloneURL, m.opt.CloneDir); err != nil {
				return fmt.Errorf("clone %s: %w", m.opt.CloneURL, err)
			}
		case m.opt.Token != "":
			// A bot token means HTTPS. Leaving the protocol to `gh repo clone`
			// inherits whatever the account uses; over SSH on a fresh machine that
			// fails with "Host key verification failed".
			if err := m.cloneHTTPS(ctx, slug); err != nil {
				return err
			}
		default:
			cmd := exec.CommandContext(ctx, "gh", "repo", "clone", slug, m.opt.CloneDir)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("gh repo clone %s: %s", slug, strings.TrimSpace(string(out)))
			}
		}
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "fetch", "--prune", "origin"); err != nil {
		return err
	}
	return m.syncClone(ctx)
}

// syncClone advances the clone to the base branch, but only when its tree is
// clean and it is not already there. The clone is not a workspace — worktrees do
// the work — so keeping it current is how a profile edit on the base branch
// reaches the runner. A dirty clone is left alone (someone is inspecting it) and
// reported, because silently resetting would delete their work.
func (m *Manager) syncClone(ctx context.Context) error {
	status, err := m.git(ctx, m.opt.CloneDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("agent clone %s has local changes; refusing to update it (commit or discard them): %s",
			m.opt.CloneDir, firstLines(status, 3))
	}
	head, err := m.git(ctx, m.opt.CloneDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	target := "origin/" + m.opt.BaseBranch
	want, err := m.git(ctx, m.opt.CloneDir, "rev-parse", target)
	if err != nil {
		return err
	}
	have, err := m.git(ctx, m.opt.CloneDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(have) == strings.TrimSpace(want) && strings.TrimSpace(head) == m.opt.BaseBranch {
		return nil
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "checkout", "-q", m.opt.BaseBranch); err != nil {
		return fmt.Errorf("check out %s in the agent clone: %w", m.opt.BaseBranch, err)
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "reset", "-q", "--hard", target); err != nil {
		return fmt.Errorf("advance the agent clone to %s: %w", target, err)
	}
	return nil
}

// cloneHTTPS clones with the token from the environment, authenticating through
// `gh auth git-credential` rather than an SSH key or a host key.
func (m *Manager) cloneHTTPS(ctx context.Context, slug string) error {
	gh := m.opt.GhBin
	if gh == "" {
		gh = "gh"
	}
	url := "https://github.com/" + slug + ".git"
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=!" + gh + " auth git-credential",
		"clone", "--quiet", url, m.opt.CloneDir,
	}
	cmd := exec.CommandContext(ctx, m.opt.GitBin, args...)
	cmd.Env = append(os.Environ(),
		"GH_TOKEN="+m.opt.Token,
		"GITHUB_TOKEN="+m.opt.Token,
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("clone %s over https: %s", url, strings.TrimSpace(string(out)))
	}
	return nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}

// EnsureIssue prepares (or resumes) the worktree for an issue.
//
// resumed is true when there was already work on the branch — the caller then
// asks the brain to continue rather than start over.
func (m *Manager) EnsureIssue(ctx context.Context, number int) (path, branch string, resumed bool, err error) {
	path = m.IssuePath(number)
	branch = m.IssueBranch(number)
	if err := os.MkdirAll(m.opt.WorkRoot, 0o755); err != nil {
		return "", "", false, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return path, branch, true, nil
	}
	if m.branchExists(ctx, branch) {
		if _, err := m.git(ctx, m.opt.CloneDir, "worktree", "add", path, branch); err != nil {
			return "", "", false, err
		}
		return path, branch, true, nil
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "worktree", "add", "-b", branch, path,
		"origin/"+m.opt.BaseBranch); err != nil {
		return "", "", false, err
	}
	return path, branch, false, nil
}

// EnsureReview prepares a detached, read-only worktree at a PR head.
func (m *Manager) EnsureReview(ctx context.Context, number int) (string, error) {
	path := m.ReviewPath(number)
	if err := os.MkdirAll(m.opt.WorkRoot, 0o755); err != nil {
		return "", err
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "fetch", "origin", fmt.Sprintf("pull/%d/head", number)); err != nil {
		return "", err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if _, err := m.git(ctx, path, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
		return path, nil
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "worktree", "add", "--detach", path, "FETCH_HEAD"); err != nil {
		return "", err
	}
	return path, nil
}

func (m *Manager) branchExists(ctx context.Context, branch string) bool {
	_, err := m.git(ctx, m.opt.CloneDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// CommitAll commits everything in the worktree. ok is false when there was
// nothing to commit.
func (m *Manager) CommitAll(ctx context.Context, dir, message string) (ok bool, err error) {
	if _, err := m.git(ctx, dir, "add", "-A"); err != nil {
		return false, err
	}
	if err := exec.CommandContext(ctx, m.opt.GitBin, "-C", dir, "diff", "--cached", "--quiet").Run(); err == nil {
		return false, nil
	}
	_, err = m.git(ctx, dir, "commit", "-m", message)
	return err == nil, err
}

// ChangedFiles lists paths that differ from the base branch, plus any working
// tree changes.
func (m *Manager) ChangedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := m.git(ctx, dir, "diff", "--name-only", "origin/"+m.opt.BaseBranch+"...HEAD")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		files = append(files, line)
	}
	// -uall: without it git collapses a new directory to "db/", and a path-level
	// denylist or check selector would miss every file inside it.
	status, err := m.git(ctx, dir, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	return files, nil
}

// CommitSummary is the "cold start" context for a brain that cannot resume: what
// already happened on this branch.
func (m *Manager) CommitSummary(ctx context.Context, dir string, max int) (string, error) {
	if max <= 0 {
		max = 10
	}
	out, err := m.git(ctx, dir, "log", "--oneline", "-n", strconv.Itoa(max),
		"origin/"+m.opt.BaseBranch+"..HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// BehindBase reports whether the base branch has commits the branch lacks.
func (m *Manager) BehindBase(ctx context.Context, dir string) (bool, error) {
	out, err := m.git(ctx, dir, "rev-list", "--count", "HEAD..origin/"+m.opt.BaseBranch)
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	return n > 0, err
}

// Cleanup removes worktrees whose branch the caller reports as merged. The
// callback keeps the "is it safe to delete" decision out of the git layer.
func (m *Manager) Cleanup(ctx context.Context, merged func(branch string) bool) ([]string, error) {
	entries, err := os.ReadDir(m.opt.WorkRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(m.opt.WorkRoot, e.Name())
		// Detached review worktrees have no branch: always removable.
		branch := ""
		if out, err := m.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			branch = strings.TrimSpace(out)
		}
		if branch != "" && branch != "HEAD" && !merged(branch) {
			continue
		}
		if _, err := m.git(ctx, m.opt.CloneDir, "worktree", "remove", "--force", path); err != nil {
			continue
		}
		if branch != "" && branch != "HEAD" {
			_, _ = m.git(ctx, m.opt.CloneDir, "branch", "-D", branch)
		}
		removed = append(removed, path)
	}
	return removed, nil
}
