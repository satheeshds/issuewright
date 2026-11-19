package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH installs a `gh` script on PATH that records its arguments and prints
// canned responses from dir. This exercises the real argument plumbing, which is
// where a wrapper bug actually lives.
func fakeGH(t *testing.T, dir string) *CLI {
	t.Helper()
	bin := filepath.Join(t.TempDir())
	script := `#!/bin/sh
printf '%s|' "$@" >> "$GH_LOG"
printf '\n' >> "$GH_LOG"
printf '%s' "${GH_TOKEN:-}" > "$GH_TOKEN_LOG"
case "$1 $2" in
  "issue list")  cat "$GH_DIR/issues.json" ;;
  "issue view")  cat "$GH_DIR/issue.json" ;;
  "pr list")     cat "$GH_DIR/prs.json" ;;
  "pr view")     cat "$GH_DIR/pr.json" ;;
  "pr create")   printf 'https://github.com/acme/demo/pull/42\n' ;;
  *)             printf 'ok\n' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gitScript := `#!/bin/sh
printf '%s|' "$@" >> "$GIT_LOG"
printf '\n' >> "$GIT_LOG"
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_LOG", filepath.Join(dir, "gh.log"))
	t.Setenv("GH_TOKEN_LOG", filepath.Join(dir, "gh.token"))
	t.Setenv("GIT_LOG", filepath.Join(dir, "git.log"))
	t.Setenv("GH_DIR", dir)
	return &CLI{GhBin: "gh", GitBin: "git"}
}

func writeCanned(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

func TestCLIListsAssignedIssues(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	writeCanned(t, dir, "issues.json", `[
	  {"number": 39, "title": "Improve observability", "body": "do it", "url": "u",
	   "state": "OPEN",
	   "labels": [{"name": "enhancement"}],
	   "assignees": [{"login": "demo-bot[bot]"}]}
	]`)

	issues, err := c.ListAssignedIssues(context.Background(), "acme/demo", "demo-bot[bot]")
	if err != nil {
		t.Fatalf("ListAssignedIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 39 || issues[0].Title != "Improve observability" {
		t.Fatalf("issues = %+v", issues)
	}
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "enhancement" {
		t.Errorf("labels = %+v", issues[0].Labels)
	}
	if len(issues[0].Assignees) != 1 || issues[0].Assignees[0] != "demo-bot[bot]" {
		t.Errorf("assignees = %+v", issues[0].Assignees)
	}

	args := readLog(t, filepath.Join(dir, "gh.log"))
	for _, want := range []string{"issue|list", "--repo|acme/demo", "--assignee|demo-bot[bot]", "--state|open", "number,title,body"} {
		if !strings.Contains(args, want) {
			t.Errorf("gh args missing %q: %s", want, args)
		}
	}
}

func TestCLICreateDraftPRParsesNumber(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	writeCanned(t, dir, "prs.json", `[]`)

	pr, err := c.CreateDraftPR(context.Background(), "acme/demo", "agent/issue-39", "main", "title", "body")
	if err != nil {
		t.Fatalf("CreateDraftPR: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("number = %d, want 42 from the URL", pr.Number)
	}
	if !pr.Draft {
		t.Error("PR must be created as a draft")
	}
	args := readLog(t, filepath.Join(dir, "gh.log"))
	for _, want := range []string{"pr|create", "--draft", "--head|agent/issue-39", "--base|main"} {
		if !strings.Contains(args, want) {
			t.Errorf("gh args missing %q: %s", want, args)
		}
	}
}

func TestCLIFindsExistingPR(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	writeCanned(t, dir, "prs.json", `[{"number": 7, "headRefName": "agent/issue-1", "url": "u"}]`)

	pr, ok, err := c.FindPRByHead(context.Background(), "acme/demo", "agent/issue-1")
	if err != nil || !ok || pr.Number != 7 {
		t.Fatalf("FindPRByHead = %+v %v %v", pr, ok, err)
	}

	writeCanned(t, dir, "prs.json", `[]`)
	if _, ok, err := c.FindPRByHead(context.Background(), "acme/demo", "agent/issue-2"); err != nil || ok {
		t.Fatalf("empty lookup = %v %v", ok, err)
	}
}

func TestCLIWritesUseTheRightSubcommands(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	ctx := context.Background()

	if err := c.ReviewPR(ctx, "acme/demo", 7, "looks fine"); err != nil {
		t.Fatal(err)
	}
	if err := c.Comment(ctx, "acme/demo", 7, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLabel(ctx, "acme/demo", 7, "agent:done"); err != nil {
		t.Fatal(err)
	}
	if err := c.Unassign(ctx, "acme/demo", 7, "demo-bot[bot]"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureLabel(ctx, "acme/demo", "agent:done"); err != nil {
		t.Fatal(err)
	}

	log := readLog(t, filepath.Join(dir, "gh.log"))
	for _, want := range []string{
		"pr|review|7|--repo|acme/demo|--comment|--body|looks fine",
		"issue|comment|7",
		"issue|edit|7|--repo|acme/demo|--add-label|agent:done",
		"--remove-assignee|demo-bot[bot]",
		"label|create|agent:done",
		"--force",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %q in:\n%s", want, log)
		}
	}
}

func TestCLIPushBranchUsesGitInWorkdir(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	if err := c.PushBranch(context.Background(), "/tmp/wt", "agent/issue-9"); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	got := readLog(t, filepath.Join(dir, "git.log"))
	want := "-C|/tmp/wt|push|--set-upstream|origin|agent/issue-9|"
	if got != want {
		t.Errorf("git args = %q, want %q", got, want)
	}
}

func TestCLIExportsToken(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	c.Token = "ghs_secret"
	writeCanned(t, dir, "issue.json", `{"number": 1, "title": "t"}`)
	if _, err := c.GetIssue(context.Background(), "acme/demo", 1); err != nil {
		t.Fatalf("GetIssue: %v (%s)", err, readLog(t, filepath.Join(dir, "gh.log")))
	}
	if got := readLog(t, filepath.Join(dir, "gh.token")); got != "ghs_secret" {
		t.Errorf("GH_TOKEN visible to gh = %q", got)
	}
}

func TestCLIReportsGhFailure(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"),
		[]byte("#!/bin/sh\necho 'gh: not authenticated' >&2\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &CLI{GhBin: "gh"}
	_, err := c.ListAssignedIssues(context.Background(), "acme/demo", "bot")
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("error did not surface gh stderr: %v", err)
	}
}

func TestFakeRecordsOnlyWhatHappened(t *testing.T) {
	f := NewFake()
	f.Issues = []Issue{{Number: 1, Assignees: []string{"bot"}}}
	got, err := f.ListAssignedIssues(context.Background(), "acme/demo", "bot")
	if err != nil || len(got) != 1 {
		t.Fatalf("fake list = %+v %v", got, err)
	}
	if _, err := f.ListAssignedIssues(context.Background(), "acme/demo", "someone-else"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsMatching("list-assigned")); n != 2 {
		t.Errorf("calls recorded = %d", n)
	}
}

// Pushing has the same trap as fetching: git cannot use GH_TOKEN without a
// credential helper, so a push with a token must configure one.
func TestPushBranchUsesTheTokenAsCredentials(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	c.Token = "ghs_push_token"
	c.Token = "ghs_push_token"
	c.GitBin = "git"
	if err := c.PushBranch(context.Background(), "/tmp/wt", "agent/issue-1"); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	got := readLog(t, filepath.Join(dir, "git.log"))
	if !strings.Contains(got, "auth git-credential") {
		t.Errorf("push did not configure the credential helper:\n%s", got)
	}
	if !strings.Contains(got, "push|--set-upstream|origin|agent/issue-1") {
		t.Errorf("push arguments wrong:\n%s", got)
	}
}

func TestPushBranchWithoutTokenKeepsThePlainCommand(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	if err := c.PushBranch(context.Background(), "/tmp/wt", "agent/issue-2"); err != nil {
		t.Fatal(err)
	}
	got := readLog(t, filepath.Join(dir, "git.log"))
	if strings.Contains(got, "auth git-credential") {
		t.Errorf("a tokenless push should not configure a helper:\n%s", got)
	}
}

// A pull request discovered *by* a label must carry that label: the workflow
// trigger matches on it, and a real run silently planned no review because the
// PR JSON had no labels field at all.
func TestCLIListsPRLabels(t *testing.T) {
	dir := t.TempDir()
	c := fakeGH(t, dir)
	writeCanned(t, dir, "prs.json", `[{"number": 47, "headRefName": "b", "headRefOid": "abc123",
	  "labels": [{"name": "review:go"}, {"name": "needs-review"}]}]`)

	prs, err := c.ListPRsWithLabel(context.Background(), "acme/demo", "review:go")
	if err != nil {
		t.Fatalf("ListPRsWithLabel: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %+v", prs)
	}
	if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "review:go" {
		t.Errorf("labels = %v", prs[0].Labels)
	}
	if prs[0].HeadRefOID != "abc123" {
		t.Errorf("head SHA missing: %q", prs[0].HeadRefOID)
	}
	if args := readLog(t, filepath.Join(dir, "gh.log")); !strings.Contains(args, "labels") {
		t.Errorf("the gh query does not ask for labels: %s", args)
	}
}
