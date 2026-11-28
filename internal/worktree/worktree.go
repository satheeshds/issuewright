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

// onAnyRemote reports whether a commit is reachable from a remote-tracking ref.
func (m *Manager) onAnyRemote(ctx context.Context, dir, head string) bool {
	if head == "" {
		return false
	}
	out, err := m.git(ctx, dir, "for-each-ref", "--count=1", "--contains", head, "refs/remotes/")
	return err == nil && strings.TrimSpace(out) != ""
}

// ReviewPath is the read-only worktree path for a PR review.
func (m *Manager) ReviewPath(number int) string {
	return filepath.Join(m.opt.WorkRoot, "review-"+strconv.Itoa(number))
}

// IssuePath is the worktree path for an issue.
func (m *Manager) IssuePath(number int) string {
	return filepath.Join(m.opt.WorkRoot, "issue-"+strconv.Itoa(number))
}

// credArgs makes git authenticate with the bot token. git has no idea what
// GH_TOKEN is: it needs a credential helper, and `gh auth git-credential` is the
// one that reads that variable. Every git call gets it, not just the clone —
// otherwise the first `git fetch` in the clone fails with "could not read
// Username for 'https://github.com'".
func (m *Manager) credArgs() []string {
	if m.opt.Token == "" {
		return nil
	}
	gh := m.opt.GhBin
	if gh == "" {
		gh = "gh"
	}
	return []string{
		"-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=!" + gh + " auth git-credential",
	}
}

func (m *Manager) gitEnv() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if m.opt.Token != "" {
		env = append(env, "GH_TOKEN="+m.opt.Token, "GITHUB_TOKEN="+m.opt.Token)
	}
	return env
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, m.credArgs()...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, m.opt.GitBin, full...)
	cmd.Env = m.gitEnv()
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
	// Make plain `git` in the clone work too, so a human debugging by hand sees
	// the same authentication the runner does.
	if m.opt.Token != "" {
		_, _ = m.git(ctx, m.opt.CloneDir, "config",
			"credential.https://github.com.helper",
			"!"+m.ghBin()+" auth git-credential")
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "fetch", "--prune", "origin"); err != nil {
		if m.opt.Token == "" {
			return fmt.Errorf("%w (no token configured: set token_env for this repo, or run `gh auth login` as the agent user)", err)
		}
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
func (m *Manager) ghBin() string {
	if m.opt.GhBin == "" {
		return "gh"
	}
	return m.opt.GhBin
}

func (m *Manager) cloneHTTPS(ctx context.Context, slug string) error {
	gh := m.ghBin()
	url := "https://github.com/" + slug + ".git"
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.https://github.com.helper=!" + gh + " auth git-credential",
		"clone", "--quiet", url, m.opt.CloneDir,
	}
	cmd := exec.CommandContext(ctx, m.opt.GitBin, args...)
	cmd.Env = m.gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("clone %s over https: %s", url, strings.TrimSpace(string(out)))
	}
	return nil
}

// shortSHA is the seven characters humans quote.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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

// EnsureReview prepares a detached worktree at a PR head.
//
// The fetched commit is resolved in the clone and named by sha, because FETCH_HEAD
// is per-worktree: naming it after the clone fetched leaves the worktree unable to
// resolve it, and git then reads the word as a path ("--detach does not take a path
// argument"). That bug was invisible while every review worktree was deleted at the
// end of each tick.
func (m *Manager) EnsureReview(ctx context.Context, number int) (string, error) {
	path := m.ReviewPath(number)
	if err := os.MkdirAll(m.opt.WorkRoot, 0o755); err != nil {
		return "", err
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "fetch", "origin", fmt.Sprintf("pull/%d/head", number)); err != nil {
		return "", err
	}
	head, err := m.git(ctx, m.opt.CloneDir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("pull/%d/head: %w", number, err)
	}
	head = strings.TrimSpace(head)
	if _, statErr := os.Stat(path); statErr == nil {
		// Rewinding to the new head is normal for a review of an older revision — but
		// only when nothing unpublished is sitting there. A commit made in this
		// worktree is on no remote, and checking out would leave it behind, so refuse
		// and name it instead.
		if cur, _ := m.HeadSHA(ctx, path); cur != "" && cur != head && !m.onAnyRemote(ctx, path, cur) {
			return "", fmt.Errorf("review worktree %s is at %s, which is on no remote: it holds unpublished work. "+
				"Recover it with `git -C %s show %s` and remove the worktree once you have it",
				path, shortSHA(cur), path, shortSHA(cur))
		}
		if _, err := m.git(ctx, path, "checkout", "--detach", head); err != nil {
			// A dirty review worktree is unpublished work from an earlier run. Do not
			// reset it away: say what is there and stop, so a human can look.
			return "", fmt.Errorf("review worktree %s has changes that were never published (%w); "+
				"inspect it with `git -C %s diff` and remove it once you have the patch", path, err, path)
		}
		return path, nil
	}
	if _, err := m.git(ctx, m.opt.CloneDir, "worktree", "add", "--detach", path, head); err != nil {
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

// LineRange is an inclusive range of new-side line numbers in a file's diff.
type LineRange struct {
	Start int
	End   int
}

// DiffHunks maps file → the new-side line ranges the diff actually touches.
// Inline review comments are only accepted on those lines; anything else has to
// be posted as a file-level comment instead of failing the whole review.
func (m *Manager) DiffHunks(ctx context.Context, dir string) (map[string][]LineRange, error) {
	out, err := m.git(ctx, dir, "diff", "--unified=0", "--no-color", "origin/"+m.opt.BaseBranch+"...HEAD")
	if err != nil {
		return nil, err
	}
	hunks := map[string][]LineRange{}
	file := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			file = strings.TrimPrefix(line, "+++ b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(line, "@@ ") && file != "":
			// @@ -a,b +c,d @@
			plus := ""
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "+") {
					plus = strings.TrimPrefix(field, "+")
					break
				}
			}
			if plus == "" {
				continue
			}
			start, count := 1, 1
			if i := strings.IndexByte(plus, ','); i >= 0 {
				start, _ = strconv.Atoi(plus[:i])
				count, _ = strconv.Atoi(plus[i+1:])
			} else {
				start, _ = strconv.Atoi(plus)
			}
			if count == 0 {
				count = 1 // pure deletion: anchor the comment on the line after it
			}
			hunks[file] = append(hunks[file], LineRange{Start: start, End: start + count - 1})
		}
	}
	return hunks, nil
}

// WorkingDiff is the uncommitted diff in a worktree, used when a run produced
// changes it may not push: the patch is handed to the human instead.
func (m *Manager) WorkingDiff(ctx context.Context, dir string) (string, error) {
	out, err := m.git(ctx, dir, "diff", "--no-color", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	return out, nil
}

// DiffAgainst is WorkingDiff plus every commit made since ref.
//
// The distinction matters: an agent that commits its own work leaves a clean tree,
// so a plain working diff says "no changes" and the work is lost. Diffing against
// the ref the run started from shows both halves.
func (m *Manager) DiffAgainst(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" {
		return m.WorkingDiff(ctx, dir)
	}
	out, err := m.git(ctx, dir, "diff", "--no-color", "--no-ext-diff", ref)
	if err != nil {
		return "", err
	}
	return out, nil
}

// HeadSHA is the commit a worktree is at.
func (m *Manager) HeadSHA(ctx context.Context, dir string) (string, error) {
	out, err := m.git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// IsCommit reports whether a revision resolves to a commit in this clone, and
// whether that commit is in HEAD's history.
//
// Both halves are what makes "Fixed in b9ba799" checkable: an invented id does not
// resolve at all, and a real commit on some other branch is not an ancestor of the
// worktree's HEAD.
func (m *Manager) IsCommit(ctx context.Context, dir, rev string) (exists, inHistory bool) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return false, false
	}
	if _, err := m.git(ctx, dir, "cat-file", "-e", rev+"^{commit}"); err != nil {
		return false, false
	}
	if _, err := m.git(ctx, dir, "merge-base", "--is-ancestor", rev, "HEAD"); err != nil {
		return true, false
	}
	return true, true
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

// Candidate is one worktree about to be judged for removal.
type Candidate struct {
	Path   string
	Name   string // the directory name: "issue-12", "review-58", …
	Branch string // "" when HEAD is detached, which a review worktree always is
	Head   string // the commit the worktree is at
}

// CleanupResult is what Cleanup did and what it deliberately left alone.
type CleanupResult struct {
	Removed []string
	// Kept lists worktrees that survived with a reason, so a nightly leak does not
	// look like a healthy box.
	Kept []KeptWorktree
}

// KeptWorktree is a worktree Cleanup refused to remove, and why.
type KeptWorktree struct {
	Path   string
	Reason string
}

// Cleanup removes finished worktrees and refuses to destroy anything that holds
// work.
//
// The first version treated a detached worktree as always removable, on the theory
// that a review has nothing to keep. A live incident proved otherwise: an agent
// committed a fix in review-58, the push silently pushed nothing, and the next tick
// deleted the directory — the commit survived only as an unreachable object. So the
// rule is now the other way round: nothing is removed without a positive answer
// from the caller *and* a clean, fully pushed worktree.
func (m *Manager) Cleanup(ctx context.Context, done func(Candidate) bool) (CleanupResult, error) {
	var res CleanupResult
	entries, err := os.ReadDir(m.opt.WorkRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(m.opt.WorkRoot, e.Name())
		head, _ := m.HeadSHA(ctx, path)
		cand := Candidate{Path: path, Name: e.Name(), Head: head}
		if out, err := m.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			if b := strings.TrimSpace(out); b != "HEAD" {
				cand.Branch = b
			}
		}

		// Uncommitted changes are work, whoever made them.
		if out, err := m.git(ctx, path, "status", "--porcelain", "-uall"); err != nil {
			res.Kept = append(res.Kept, KeptWorktree{Path: path, Reason: "git status failed"})
			continue
		} else if strings.TrimSpace(out) != "" {
			res.Kept = append(res.Kept, KeptWorktree{Path: path, Reason: "uncommitted changes"})
			continue
		}
		// A detached worktree has no branch to compare, so ask git the only question
		// that matters: is this commit anywhere on the remote? A commit made in a
		// review worktree is not, and removing it is how a fix becomes unreachable.
		if cand.Branch == "" {
			if !m.onAnyRemote(ctx, path, cand.Head) {
				res.Kept = append(res.Kept, KeptWorktree{Path: path, Reason: "commits not on any remote"})
				continue
			}
		}
		// A branch that is ahead of its remote holds commits nobody else has.
		if cand.Branch != "" {
			remote, err := m.git(ctx, path, "rev-parse", "--verify", "--quiet", "origin/"+cand.Branch)
			if err != nil || strings.TrimSpace(remote) != cand.Head {
				res.Kept = append(res.Kept, KeptWorktree{Path: path, Reason: "commits not on origin/" + cand.Branch})
				continue
			}
		}
		if !done(cand) {
			res.Kept = append(res.Kept, KeptWorktree{Path: path, Reason: "not finished"})
			continue
		}
		if _, err := m.git(ctx, m.opt.CloneDir, "worktree", "remove", "--force", path); err != nil {
			res.Kept = append(res.Kept, KeptWorktree{Path: path, Reason: "git refused to remove it"})
			continue
		}
		if cand.Branch != "" {
			_, _ = m.git(ctx, m.opt.CloneDir, "branch", "-D", cand.Branch)
		}
		res.Removed = append(res.Removed, path)
	}
	return res, nil
}

// DiffPaths renders the change to specific paths as a patch, relative to ref
// (normally the base branch).
//
// Not HEAD: the change may already be committed on the branch — an earlier run can
// have put it there — in which case `git diff HEAD` shows nothing and the proposal
// would be empty.
func (m *Manager) DiffPaths(ctx context.Context, dir, ref string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	for _, p := range paths {
		// New files are invisible to `git diff` until git knows about them.
		_, _ = m.git(ctx, dir, "add", "-N", "--", p)
	}
	args := append([]string{"diff", "--no-color", ref, "--"}, paths...)
	return m.git(ctx, dir, args...)
}

// RevertPaths restores specific paths to a ref — normally the base branch — and
// deletes the ones that ref does not have.
//
// Reverting to HEAD is not enough: the paths may already be committed on the branch
// (an earlier run put them there), and the push is judged on the net tree change, so
// the branch has to end up identical to the base in those paths.
func (m *Manager) RevertPaths(ctx context.Context, dir, ref string, paths []string) error {
	for _, p := range paths {
		if _, err := m.git(ctx, dir, "checkout", ref, "--", p); err != nil {
			// Not in HEAD: a file the run created. Drop it from the index and disk.
			_, _ = m.git(ctx, dir, "reset", "-q", "--", p)
			if rmErr := os.Remove(filepath.Join(dir, p)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return rmErr
			}
		}
	}
	return nil
}

// CommitPaths commits just the given paths, so a revert does not sweep up unrelated
// work that happens to be uncommitted.
func (m *Manager) CommitPaths(ctx context.Context, dir, message string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, p := range paths {
		// A path this branch created and we just deleted has no pathspec to match;
		// staging the deletion of a tracked one does. Either way, keep going.
		_, _ = m.git(ctx, dir, "add", "-A", "--", p)
	}
	if err := exec.CommandContext(ctx, m.opt.GitBin, "-C", dir, "diff", "--cached", "--quiet").Run(); err == nil {
		return nil // nothing staged: the revert was already the base content
	}
	_, err := m.git(ctx, dir, "commit", "-m", message)
	return err
}
