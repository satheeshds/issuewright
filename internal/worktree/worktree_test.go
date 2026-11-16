package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newOrigin builds a bare "remote" plus an initial commit on main, so the
// worktree lifecycle is tested against real git instead of mocks.
func newOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	run(seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-qm", "initial")
	run(root, "init", "-q", "--bare", bare)
	run(bare, "symbolic-ref", "HEAD", "refs/heads/main")
	run(seed, "remote", "add", "origin", bare)
	run(seed, "push", "-q", "origin", "main")
	return bare
}

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	bare := newOrigin(t)
	m := New(Options{
		CloneDir:   filepath.Join(root, "clone"),
		WorkRoot:   filepath.Join(root, "work"),
		BaseBranch: "main",
		CloneURL:   bare,
	})
	return m, bare
}

func TestEnsureCloneIsIdempotent(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatalf("first clone: %v", err)
	}
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatalf("second clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.CloneDir(), "README.md")); err != nil {
		t.Fatalf("clone has no content: %v", err)
	}
}

func TestEnsureIssueCreatesThenResumes(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatal(err)
	}

	path, branch, resumed, err := m.EnsureIssue(ctx, 39)
	if err != nil {
		t.Fatalf("EnsureIssue: %v", err)
	}
	if resumed {
		t.Error("a fresh issue must not report resumed")
	}
	if branch != "agent/issue-39" {
		t.Errorf("branch = %q", branch)
	}
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Errorf("worktree missing checkout: %v", err)
	}

	// A commit becomes the resume checkpoint.
	if err := os.WriteFile(filepath.Join(path, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.CommitAll(ctx, path, "agent: work in progress"); err != nil || !ok {
		t.Fatalf("CommitAll = %v %v", ok, err)
	}

	path2, _, resumed2, err := m.EnsureIssue(ctx, 39)
	if err != nil || !resumed2 || path2 != path {
		t.Fatalf("resume = %q %v %v", path2, resumed2, err)
	}

	// Nothing staged means nothing to commit.
	if ok, err := m.CommitAll(ctx, path, "again"); err != nil || ok {
		t.Fatalf("empty commit = %v %v", ok, err)
	}
}

func TestEnsureIssueResumesBranchWithNoWorktree(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatal(err)
	}
	path, _, _, err := m.EnsureIssue(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a lost worktree (reboot, prune) with the branch intact.
	if _, err := m.git(ctx, m.CloneDir(), "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	_, _, resumed, err := m.EnsureIssue(ctx, 7)
	if err != nil {
		t.Fatalf("EnsureIssue after prune: %v", err)
	}
	if !resumed {
		t.Error("an existing branch must report resumed")
	}
}

func TestChangedFilesAndSummary(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatal(err)
	}
	path, _, _, err := m.EnsureIssue(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "internal", "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.CommitAll(ctx, path, "feat: add a"); err != nil || !ok {
		t.Fatalf("commit = %v %v", ok, err)
	}
	files, err := m.ChangedFiles(ctx, path)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(files) != 1 || files[0] != "internal/a.go" {
		t.Errorf("changed = %v", files)
	}
	summary, err := m.CommitSummary(ctx, path, 5)
	if err != nil || !strings.Contains(summary, "feat: add a") {
		t.Errorf("summary = %q, %v", summary, err)
	}
	// A brand-new directory must be listed file by file, not as "dir/".
	if err := os.MkdirAll(filepath.Join(path, "packages", "openapi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "packages", "openapi", "swagger.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err = m.ChangedFiles(ctx, path)
	if err != nil {
		t.Fatalf("ChangedFiles with untracked dir: %v", err)
	}
	found := false
	for _, f := range files {
		if f == "packages/openapi/swagger.yaml" {
			found = true
		}
		if strings.HasSuffix(f, "/") {
			t.Errorf("directory reported as a path: %q", f)
		}
	}
	if !found {
		t.Errorf("untracked file not listed individually: %v", files)
	}

	behind, err := m.BehindBase(ctx, path)
	if err != nil || behind {
		t.Errorf("behind = %v, %v", behind, err)
	}
}

func TestEnsureReviewIsDetached(t *testing.T) {
	m, bare := newManager(t)
	ctx := context.Background()
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatal(err)
	}
	// A PR head exists as branch agent/issue-3 on origin.
	path, branch, _, err := m.EnsureIssue(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "pr.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.CommitAll(ctx, path, "feat: pr work"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := m.git(ctx, path, "push", "-q", "origin", branch); err != nil {
		t.Fatal(err)
	}
	// gh's pull/<n>/head ref does not exist offline; emulate it on the remote.
	sha, err := m.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", bare, "update-ref", "refs/pull/3/head", strings.TrimSpace(sha))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	reviewPath, err := m.EnsureReview(ctx, 3)
	if err != nil {
		t.Fatalf("EnsureReview: %v", err)
	}
	out, err := m.git(ctx, reviewPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "HEAD" {
		t.Errorf("review worktree is not detached: %q", strings.TrimSpace(out))
	}
}

func TestCleanupRemovesOnlyMergedWorktrees(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.EnsureClone(ctx, "acme/demo"); err != nil {
		t.Fatal(err)
	}
	keep, _, _, err := m.EnsureIssue(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	drop, _, _, err := m.EnsureIssue(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := m.Cleanup(ctx, func(branch string) bool {
		return branch == "agent/issue-2"
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(removed) != 1 || removed[0] != drop {
		t.Fatalf("removed = %v (want %s)", removed, drop)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unmerged worktree was removed: %v", err)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Errorf("merged worktree still present: %v", err)
	}
}
