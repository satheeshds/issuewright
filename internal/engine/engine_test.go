package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/config"
	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/state"
	"github.com/satheeshds/issuewright/internal/workflow"
	"github.com/satheeshds/issuewright/internal/worktree"
	"github.com/satheeshds/issuewright/prompts"

	_ "github.com/satheeshds/issuewright/internal/brain/stub"
	_ "github.com/satheeshds/issuewright/internal/workflow/implement"
	_ "github.com/satheeshds/issuewright/internal/workflow/review"
)

// harness is a repo wired to a fake remote and a fake GitHub.
type harness struct {
	t        *testing.T
	repo     config.Repo
	agent    config.AgentConfig
	gh       *github.Fake
	profile  *config.Profile
	bare     string
	cloneDir string
	logBuf   *bytes.Buffer
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "agent-clone")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The profile preamble is a committed file in a real repo; committing it here
	// keeps the clone clean, which EnsureClone requires before it advances.
	if err := os.WriteFile(filepath.Join(seed, "AGENT_RULES.md"), []byte("repo rules: be careful\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-qm", "initial")
	gitRun(t, root, "init", "-q", "--bare", bare)
	gitRun(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	gitRun(t, seed, "remote", "add", "origin", bare)
	gitRun(t, seed, "push", "-q", "origin", "main")
	gitRun(t, root, "clone", "-q", bare, clone)

	prof := config.Defaults()
	prof.Checks = []config.Check{{Name: "all", Paths: []string{"**"}, Run: []string{"true"}}}
	prof.Protect.WriteDenylist = []string{"db/*.go"}
	prof.Protect.NeverRun = []string{"make db-reset"}
	prof.Protect.SecretPaths = []string{".env", ".env.*"}
	prof.Prompt.PreambleFile = "AGENT_RULES.md"

	h := &harness{
		t: t, bare: bare, cloneDir: clone, gh: github.NewFake(), profile: prof,
		logBuf: &bytes.Buffer{},
		repo: config.Repo{
			Name: "demo", Path: clone, Slug: "acme/demo", Bot: "demo-bot[bot]",
		},
		agent: config.AgentConfig{
			Workroot: filepath.Join(root, "work"), Logdir: filepath.Join(root, "logs"),
			Brain: "stub", Thinking: "high", MaxRunMinutes: 5,
		},
	}
	return h
}

func (h *harness) engine(t *testing.T, drv brain.Driver, opts ...func(*Options)) (*Engine, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	o := Options{
		Agent: h.agent, Repo: h.repo, GitHub: h.gh, Log: logBuf,
		Driver: drv, Profile: h.profile,
		Now: func() time.Time { return time.Date(2026, 11, 16, 21, 20, 0, 0, time.UTC) },
	}
	for _, f := range opts {
		f(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e, logBuf
}

func (h *harness) addIssue(number int, title, body string, labels ...string) {
	h.gh.Issues = append(h.gh.Issues, github.Issue{
		Number: number, Title: title, Body: body,
		Labels: labels, Assignees: []string{h.repo.Bot},
	})
}

func TestTickImplementsAnIssueEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- added metrics\n"})

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].State != StateDone {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if !strings.Contains(results[0].Detail, "example.test/pr/") {
		t.Errorf("detail should carry the PR URL: %q", results[0].Detail)
	}

	// A branch was pushed and a draft PR opened.
	if len(h.gh.Pushed) != 1 || h.gh.Pushed[0] != "agent/issue-12" {
		t.Fatalf("pushed = %v", h.gh.Pushed)
	}
	if len(h.gh.PRs) != 1 || !h.gh.PRs[0].Draft {
		t.Fatalf("PRs = %+v", h.gh.PRs)
	}
	if !strings.Contains(h.gh.PRs[0].Body, "Closes #12") {
		t.Errorf("PR body must close the issue: %q", h.gh.PRs[0].Body)
	}
	if !strings.Contains(h.gh.PRs[0].Body, "added metrics") {
		t.Errorf("PR body must carry the agent summary: %q", h.gh.PRs[0].Body)
	}
	if !strings.Contains(h.gh.PRs[0].Body, "✅ `all`") {
		t.Errorf("PR body must carry check results: %q", h.gh.PRs[0].Body)
	}

	// Labels and comments tell the human what happened.
	labels := strings.Join(h.gh.Labels[12], ",")
	if !strings.Contains(labels, "agent:done") {
		t.Errorf("labels = %q", labels)
	}
	if len(h.gh.Comments[12]) < 2 {
		t.Errorf("expected a claim comment and a summary: %v", h.gh.Comments[12])
	}

	// The agent's report is never committed.
	files := gitRun(t, h.cloneDir, "ls-tree", "--name-only", "agent/issue-12")
	if strings.Contains(files, prompts.SummaryFile) {
		t.Errorf("SUMMARY.md was committed: %s", files)
	}
	if !strings.Contains(files, "issuewright-stub.txt") {
		t.Errorf("the agent's change is missing from the branch: %s", files)
	}

	// The run log exists and starts with a header.
	logsFiles, err := filepath.Glob(filepath.Join(h.agent.Logdir, "demo", "*.jsonl"))
	if err != nil || len(logsFiles) != 1 {
		t.Fatalf("run logs = %v (%v)", logsFiles, err)
	}
	raw, _ := os.ReadFile(logsFiles[0])
	if !strings.Contains(string(raw), "brain=stub") {
		t.Errorf("run log header = %q", firstLineOf(string(raw)))
	}
}

func TestTickIsIdempotentWhenAlreadyDone(t *testing.T) {
	h := newHarness(t)
	h.addIssue(5, "done already", "body", "agent:done")
	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile})

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].State != StateSkipped {
		t.Fatalf("results = %+v", results)
	}
	if len(h.gh.Pushed) != 0 {
		t.Error("nothing should be pushed for a done issue")
	}
}

func TestBlockedRunLabelsCommentsUnassignsAndDoesNotRetry(t *testing.T) {
	h := newHarness(t)
	h.addIssue(7, "will fail", "body")
	e, logs := h.engine(t, &stubDriver{fail: true})

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].State != StateBlocked {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if !strings.Contains(strings.Join(h.gh.Labels[7], ","), "agent:blocked") {
		t.Errorf("labels = %v", h.gh.Labels[7])
	}
	if len(h.gh.Comments[7]) == 0 {
		t.Error("a blocked run must say why")
	}
	if len(h.gh.Assignees[7]) != 0 {
		t.Errorf("the bot should be unassigned so it is not retried: %v", h.gh.Assignees[7])
	}
	if len(h.gh.Pushed) != 0 {
		t.Errorf("a failed run pushed something: %v", h.gh.Pushed)
	}

	// Second tick: the claim and labels keep it quiet.
	h.gh.Labels[7] = append(h.gh.Labels[7], "agent:blocked")
	h.gh.Issues[0].Assignees = []string{h.repo.Bot}
	results, err = e.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	for _, r := range results {
		if r.State == StateBlocked && r.Number == 7 {
			t.Error("a blocked job was retried without human action")
		}
	}
}

func TestDenylistedWriteBlocks(t *testing.T) {
	h := newHarness(t)
	h.addIssue(11, "touch generated", "body")
	e, logs := h.engine(t, &stubDriver{file: "db/gen.go", content: "hand written\n"})

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if results[0].State != StateBlocked || !strings.Contains(results[0].Detail, "forbids") {
		t.Fatalf("results = %+v", results)
	}
	if len(h.gh.Pushed) != 1 {
		// The work is preserved on the branch, but no PR is opened and the issue
		// is labelled blocked.
		t.Logf("pushed = %v (work preservation)", h.gh.Pushed)
	}
	if len(h.gh.PRs) != 0 {
		t.Errorf("a forbidden change must not open a PR: %+v", h.gh.PRs)
	}
}

func TestForbiddenCommandInRunLogBlocks(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "runs migrate", "body")
	e, logs := h.engine(t, &toolCallDriver{command: "make db-reset && echo done"})

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if results[0].State != StateBlocked || !strings.Contains(results[0].Detail, "make db-reset") {
		t.Fatalf("results = %+v", results)
	}
	if len(h.gh.PRs) != 0 {
		t.Error("no PR should be opened for a never_run violation")
	}
}

func TestReviewWorkflowCannotWrite(t *testing.T) {
	h := newHarness(t)
	h.gh.PRs = []github.PR{{Number: 7, Title: "Add metrics", HeadRef: "agent/issue-7", BaseRef: "main"}}

	// Give the review worktree something to check out.
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-7")
	gitRun(t, h.cloneDir, "worktree", "add", "-q", "-b", "agent/issue-7", dir, "origin/main")
	if err := os.WriteFile(filepath.Join(dir, "pr.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: pr work")
	gitRun(t, dir, "push", "-q", "origin", "agent/issue-7")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/7/head", sha)

	e, logs := h.engine(t, &stubDriver{file: prompts.ReviewFile, content: "## Summary\nlooks fine\n"})
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].Workflow != "review" || results[0].State != StateDone {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if len(h.gh.Pushed) != 0 {
		t.Fatalf("a reviewer pushed: %v", h.gh.Pushed)
	}
	if len(h.gh.PRs) != 1 {
		t.Errorf("a reviewer created a PR: %+v", h.gh.PRs)
	}
	body := strings.Join(h.gh.Reviews[7], "\n")
	if !strings.Contains(body, "looks fine") {
		t.Errorf("review body = %q", body)
	}
	if !strings.Contains(body, "✅ `all`") {
		t.Errorf("review must report the read-only checks it ran: %q", body)
	}
	// No labels were touched: review has no Label permission.
	if len(h.gh.Labels[7]) != 0 {
		t.Errorf("review relabelled the PR: %v", h.gh.Labels[7])
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.addIssue(21, "dry", "body")
	e, logs := h.engine(t, &stubDriver{}, func(o *Options) {
		o.DryRun = true
		o.Verbose = true
	})

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].State != StatePlanned {
		t.Fatalf("results = %+v", results)
	}
	// Only discovery: no labels, comments, pushes or PRs.
	for _, call := range h.gh.Calls {
		for _, write := range []string{"add-label", "remove-label", "comment", "push", "create-pr", "ensure-label", "review "} {
			if strings.HasPrefix(call, write) {
				t.Errorf("dry run wrote to GitHub: %v", h.gh.Calls)
			}
		}
	}
	if len(h.gh.Pushed) != 0 || len(h.gh.PRs) != 0 {
		t.Error("dry run published something")
	}
	if !strings.Contains(logs.String(), "repo rules: be careful") {
		t.Error("--verbose should print the resolved prompt")
	}
	if !strings.Contains(results[0].Detail, "brain=stub") {
		t.Errorf("dry run detail = %q", results[0].Detail)
	}
}

func TestResumeUsesContinueOnSecondRun(t *testing.T) {
	h := newHarness(t)
	h.addIssue(30, "half done", "body")

	// First run: fails, leaving work committed on the branch.
	e, _ := h.engine(t, &stubDriver{fail: true})
	if results, err := e.Tick(context.Background()); err != nil || results[0].State != StateBlocked {
		t.Fatalf("first tick = %+v %v", results, err)
	}
	// A human re-assigns; the claim is stale, so clear it and drop the label.
	if err := state.New(filepath.Join(filepath.Dir(h.agent.Workroot), "state")).DeleteClaim("demo", "implement", 30); err != nil {
		t.Fatal(err)
	}
	h.gh.Labels[30] = nil
	h.gh.Issues[0].Assignees = []string{h.repo.Bot}
	h.gh.Assignees[30] = nil

	drv := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- finished\n"}
	e2, logs := h.engine(t, drv)
	results, err := e2.Tick(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v\n%s", err, logs)
	}
	if results[0].State != StateDone {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if !drv.sawContinue {
		t.Error("the second run should have continued the previous session (-c)")
	}
	if !strings.Contains(drv.sawPrompt, "Continue and finish") {
		t.Errorf("resume prompt = %q", firstLineOf(drv.sawPrompt))
	}
}

func TestStatusReportsPendingAndClaims(t *testing.T) {
	h := newHarness(t)
	h.addIssue(41, "pending work", "body")
	e, _ := h.engine(t, &stubDriver{})

	rows, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rows) != 1 || rows[0].State != "pending" || rows[0].Number != 41 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Detail != "pending work" {
		t.Errorf("detail should show the title: %q", rows[0].Detail)
	}
}

func TestLockPreventsOverlappingTicks(t *testing.T) {
	h := newHarness(t)
	h.addIssue(50, "first", "body")
	e, _ := h.engine(t, &stubDriver{})

	store := state.New(filepath.Join(filepath.Dir(h.agent.Workroot), "state"))
	held, err := store.TryLock("demo")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("a locked repo must not dispatch: %+v", results)
	}
}

// --- test drivers ---------------------------------------------------------

// stubDriver writes one artifact file (like the stub brain) and records the spec.
type stubDriver struct {
	file        string
	content     string
	fail        bool
	sawContinue bool
	sawPrompt   string
	sawWorkdir  string
	sawModel    string
	sawModels   []string
	sawThinking string
	brainText   string
}

func (d *stubDriver) Name() string { return "stub" }
func (d *stubDriver) Caps() brain.Caps {
	return brain.Caps{Resume: true, Model: true, Thinking: true}
}

func (d *stubDriver) Run(ctx context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	d.sawContinue, d.sawPrompt, d.sawWorkdir = spec.Continue, spec.Prompt, spec.Workdir
	d.sawModel, d.sawThinking = spec.Model, spec.Thinking
	d.sawModels = append(d.sawModels, spec.Model)
	if d.content != "" || d.file != "" {
		target := filepath.Join(spec.Workdir, d.file)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return brain.Result{Driver: "stub"}, err
		}
		if err := os.WriteFile(target, []byte(d.content), 0o644); err != nil {
			return brain.Result{Driver: "stub"}, err
		}
	}
	// Always a source change, so the branch differs from the base and checks have
	// something to select; the artifact above is the agent's report.
	if d.file != "issuewright-stub.txt" {
		if err := os.WriteFile(filepath.Join(spec.Workdir, "issuewright-stub.txt"), []byte("work\n"), 0o644); err != nil {
			return brain.Result{Driver: "stub"}, err
		}
	}
	text := d.brainText
	if text == "" {
		text = "stub done"
	}
	io.WriteString(log, `{"type":"message_end","message":{"content":[{"type":"text","text":"`+text+`"}]}}`+"\n")
	if d.fail {
		return brain.Result{Driver: "stub", ExitCode: 1}, os.ErrDeadlineExceeded
	}
	return brain.Result{Driver: "stub", Summary: text}, nil
}

// toolCallDriver emits a JSONL event claiming the agent ran a command.
type toolCallDriver struct {
	command string
}

func (d *toolCallDriver) Name() string     { return "stub" }
func (d *toolCallDriver) Caps() brain.Caps { return brain.Caps{Resume: true} }
func (d *toolCallDriver) Run(ctx context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	raw, _ := json.Marshal(map[string]any{
		"type":     "tool_execution_start",
		"toolName": "bash",
		"args":     map[string]string{"command": d.command},
	})
	log.Write(append(raw, '\n'))
	if err := os.WriteFile(filepath.Join(spec.Workdir, "issuewright-stub.txt"), []byte("work\n"), 0o644); err != nil {
		return brain.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(spec.Workdir, prompts.SummaryFile), []byte("## What changed\n- x\n"), 0o644); err != nil {
		return brain.Result{}, err
	}
	return brain.Result{Driver: "stub"}, nil
}

var _ = worktree.New
var _ = workflow.Names

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// The check runner writes to the same log as the brain. Closing that file before
// the checks run SIGPIPEs them (real failure seen on the box: `go test` exit 141),
// so the log must survive the whole job.
func TestChecksDoNotSIGPIPEOnTheRunLog(t *testing.T) {
	h := newHarness(t)
	h.addIssue(60, "checks must run after the brain", "body")
	h.profile.Checks = []config.Check{{Name: "noisy", Paths: []string{"**"},
		Run: []string{`for i in $(seq 1 200); do echo "line $i of output"; done`}}}

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"})
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if results[0].State != StateDone {
		t.Fatalf("a noisy check must still pass: %+v\n%s", results, logs)
	}
	if !strings.Contains(h.gh.PRs[0].Body, "✅ `noisy`") {
		t.Errorf("check result missing from the PR body: %q", h.gh.PRs[0].Body)
	}
}

// A resumed run often writes no SUMMARY.md. The PR body must then use the
// brain's own final message — never a canned template, and never a stray issue
// reference (a real pilot produced a body starting with "Closes #0").
func TestPRBodyWithoutSummaryUsesBrainText(t *testing.T) {
	h := newHarness(t)
	h.addIssue(70, "no summary", "body")
	drv := &stubDriver{brainText: "Implemented the tests and committed them."}

	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	body := h.gh.PRs[0].Body
	if strings.Contains(body, "Closes #0") {
		t.Errorf("PR body contains a placeholder issue reference:\n%s", body)
	}
	if !strings.Contains(body, "Implemented the tests and committed them.") {
		t.Errorf("PR body should carry the brain's final message:\n%s", body)
	}
	if !strings.Contains(body, "Closes #70") {
		t.Errorf("PR body must close the real issue:\n%s", body)
	}
	if n := strings.Count(body, "Closes #70"); n != 1 {
		t.Errorf("Closes appears %d times:\n%s", n, body)
	}
}

// A real pilot ran with the previous profile: the clone had been advanced to the
// merged profile, but the engine had loaded the profile before the sync. The
// profile must be re-read after the clone moves.
func TestProfileIsReloadedAfterTheCloneAdvances(t *testing.T) {
	h := newHarness(t)
	h.addIssue(80, "profile moved under us", "body")

	// No injected profile: the engine must read .agent/agent.yaml from the clone.
	profilePath := filepath.Join(h.cloneDir, config.ProfileDir, config.ProfileFile)
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfile := func(name string, run string, advisory bool) {
		body := "version: 1\nchecks:\n  - name: " + name + "\n    paths: [\"**\"]\n    run: [\"" + run + "\"]\n"
		if advisory {
			body += "    advisory: true\n"
		}
		if err := os.WriteFile(profilePath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile("old-check", "true", false)
	// The clone must not look dirty, or EnsureClone refuses to advance it.
	gitRun(t, h.cloneDir, "add", "-A")
	gitRun(t, h.cloneDir, "commit", "-qm", "profile: old check")
	gitRun(t, h.cloneDir, "push", "-q", "origin", "HEAD:main")

	o := Options{Agent: h.agent, Repo: h.repo, GitHub: h.gh, Log: io.Discard,
		Driver: &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}}
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := e.Profile().Checks[0].Name; got != "old-check" {
		t.Fatalf("initial profile = %q", got)
	}

	// The base branch moves on: the new check is advisory and named differently.
	writeProfile("new-check", "echo newer", true)
	gitRun(t, h.cloneDir, "add", "-A")
	gitRun(t, h.cloneDir, "commit", "-qm", "profile: new check")
	gitRun(t, h.cloneDir, "push", "-q", "origin", "HEAD:main")

	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := e.Profile().Checks[0].Name; got != "new-check" {
		t.Fatalf("profile was not reloaded: %q", got)
	}
	if len(h.gh.PRs) != 1 {
		t.Fatalf("no PR: %+v", h.gh.Calls)
	}
	if !strings.Contains(h.gh.PRs[0].Body, "`new-check`") {
		t.Errorf("the new check did not run:\n%s", h.gh.PRs[0].Body)
	}
}

// Label mode must not require assignment: on a real repo the App bot could not be
// assigned ("Bot does not have access to the repository"), and the label is the
// only workable trigger.
func TestLabelPickupWorksWithoutAnAssignee(t *testing.T) {
	h := newHarness(t)
	h.profile.Issue.Pickup = "label"
	h.profile.Issue.ForceLabel = "agent:go"
	// The issue is labelled but assigned to nobody.
	h.gh.Issues = append(h.gh.Issues, github.Issue{
		Number: 90, Title: "labelled work", Body: "body", Labels: []string{"agent:go"},
	})

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"})
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].Number != 90 || results[0].State != StateDone {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if len(h.gh.CallsMatching("list-labeled acme/demo agent:go")) != 1 {
		t.Errorf("label pickup should query by label: %v", h.gh.Calls)
	}
	if len(h.gh.CallsMatching("list-assigned")) != 0 {
		t.Errorf("label pickup must not depend on assignees: %v", h.gh.Calls)
	}
}

// In assignee mode an unlabelled, assigned issue still works.
func TestAssigneePickupStillWorks(t *testing.T) {
	h := newHarness(t)
	h.addIssue(91, "assigned work", "body")
	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"})
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].State != StateDone {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if len(h.gh.CallsMatching("list-assigned")) != 1 {
		t.Errorf("assignee mode should query by assignee: %v", h.gh.Calls)
	}
}

// A label on one issue can pick the model for that run, without changing the
// repo's default for everything else.
func TestLabelsChooseTheModelForOneRun(t *testing.T) {
	h := newHarness(t)
	h.agent.Model = "provider/default"
	h.addIssue(100, "needs a stronger model", "body", "model:provider/strong")
	drv := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}

	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if drv.sawModel != "provider/strong" {
		t.Errorf("model = %q, want the label's", drv.sawModel)
	}
	if !strings.Contains(h.gh.PRs[0].Body, "provider/strong") {
		t.Errorf("the PR body should name the model used? (brain attribution)\n%s", h.gh.PRs[0].Body)
	}
}

func TestLabelsChooseThinkingAndFallBackWhenInvalid(t *testing.T) {
	h := newHarness(t)
	h.agent.Thinking = "high"
	h.addIssue(101, "low effort", "body", "thinking:low")
	drv := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}
	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if drv.sawThinking != "low" {
		t.Errorf("thinking = %q, want low", drv.sawThinking)
	}

	// An unknown level must not stop the run: it is ignored and reported.
	h2 := newHarness(t)
	h2.agent.Thinking = "high"
	h2.addIssue(102, "typo in a label", "body", "thinking:ultra")
	drv2 := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}
	e2, logs2 := h2.engine(t, drv2)
	if _, err := e2.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs2)
	}
	if drv2.sawThinking != "high" {
		t.Errorf("thinking = %q, want the configured fallback", drv2.sawThinking)
	}
	if !strings.Contains(logs2.String(), "ignoring thinking label") {
		t.Errorf("the ignored label should be reported:\n%s", logs2.String())
	}
}

func TestCommandLineFlagBeatsTheLabel(t *testing.T) {
	h := newHarness(t)
	h.agent.Model = "provider/default"
	h.addIssue(103, "flag wins", "body", "model:provider/from-label")
	drv := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}
	e, logs := h.engine(t, drv, func(o *Options) {
		o.Override = config.Overrides{Model: "provider/from-flag"}
	})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if drv.sawModel != "provider/from-flag" {
		t.Errorf("model = %q, want the flag", drv.sawModel)
	}
}

func TestUnknownBrainLabelIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.addIssue(104, "bad brain label", "body", "brain:nope")
	drv := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}
	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "ignoring brain label") {
		t.Errorf("an unknown brain should be reported:\n%s", logs.String())
	}
	if len(h.gh.PRs) != 1 {
		t.Error("the run should still have published")
	}
}

// On a repo where GitHub refuses to request review from the App bot ("Could not
// resolve user with login"), the label is the only automatable trigger.
func TestReviewStartsFromALabelOnThePR(t *testing.T) {
	h := newHarness(t)
	h.gh.PRs = []github.PR{{
		Number: 8, Title: "some work", HeadRef: "agent/issue-8", BaseRef: "main",
		HeadRefOID: "sha1111", Labels: []string{"review:go"},
	}}
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-8")
	gitRun(t, h.cloneDir, "worktree", "add", "-q", "-b", "agent/issue-8", dir, "origin/main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: work")
	gitRun(t, dir, "push", "-q", "origin", "agent/issue-8")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/8/head", sha)

	e, logs := h.engine(t, &stubDriver{file: prompts.ReviewFile, content: "## Summary\nfine\n"})
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(results) != 1 || results[0].Workflow != "review" || results[0].State != StateDone {
		t.Fatalf("results = %+v\n%s", results, logs)
	}
	if len(h.gh.Reviews[8]) != 1 {
		t.Errorf("no review posted: %v", h.gh.Calls)
	}
	if len(h.gh.Pushed) != 0 {
		t.Errorf("a reviewer pushed: %v", h.gh.Pushed)
	}
}

// The label stays on the PR, so the next tick must not review the same commit
// again — and a new commit must be reviewed.
func TestReviewSkipsTheSameRevisionAndRepeatsOnNewOnes(t *testing.T) {
	h := newHarness(t)
	h.gh.PRs = []github.PR{{
		Number: 9, Title: "work", HeadRef: "agent/issue-9", BaseRef: "main",
		HeadRefOID: "sha2222", Labels: []string{"review:go"},
	}}
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-9")
	gitRun(t, h.cloneDir, "worktree", "add", "-q", "-b", "agent/issue-9", dir, "origin/main")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: work")
	gitRun(t, dir, "push", "-q", "origin", "agent/issue-9")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/9/head", sha)
	h.gh.PRs[0].HeadRefOID = sha

	drv := &stubDriver{file: prompts.ReviewFile, content: "## Summary\nfine\n"}
	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Reviews[9]) != 1 {
		t.Fatalf("first review missing: %v", h.gh.Calls)
	}

	// Same head commit, another tick.
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if len(h.gh.Reviews[9]) != 1 {
		t.Errorf("the same revision was reviewed twice: %v", h.gh.Calls)
	}
	if len(results) != 1 || results[0].State != StateSkipped {
		t.Errorf("results = %+v", results)
	}

	// A new commit on the PR must be reviewed.
	h.gh.PRs[0].HeadRefOID = "sha3333"
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("third Tick: %v", err)
	}
	if len(h.gh.Reviews[9]) != 2 {
		t.Errorf("a new revision was not reviewed: %v", h.gh.Calls)
	}
}

// The reviewer and the author are allowed to be different models.
func TestReviewCanUseADifferentModelThanImplement(t *testing.T) {
	h := newHarness(t)
	h.agent.Model = "provider/author"
	h.profile.Workflows["review"] = config.WorkflowSettings{
		Brain: config.BrainSettings{Model: "provider/reviewer"},
	}
	h.addIssue(110, "author work", "body")
	h.gh.PRs = []github.PR{{
		Number: 11, Title: "work", HeadRef: "agent/issue-11", BaseRef: "main",
		HeadRefOID: "sha11", Labels: []string{"review:go"},
	}}
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-11")
	gitRun(t, h.cloneDir, "worktree", "add", "-q", "-b", "agent/issue-11", dir, "origin/main")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: work")
	gitRun(t, dir, "push", "-q", "origin", "agent/issue-11")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/11/head", sha)
	h.gh.PRs[0].HeadRefOID = sha

	drv := &stubDriver{file: prompts.SummaryFile, content: "## What changed\n- x\n"}
	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	// Two runs: the implement job, then the review job.
	if len(h.gh.Reviews[11]) != 1 {
		t.Fatalf("no review: %v", h.gh.Calls)
	}
	// Review is scheduled before implement, so the reviewer's model comes first.
	if len(drv.sawModels) != 2 {
		t.Fatalf("expected two runs (review, implement), got %v", drv.sawModels)
	}
	if drv.sawModels[0] != "provider/reviewer" {
		t.Errorf("the review used %q, want the review model", drv.sawModels[0])
	}
	if drv.sawModels[1] != "provider/author" {
		t.Errorf("the implement run used %q, want the repo model", drv.sawModels[1])
	}
	if !strings.Contains(logs.String(), "provider/reviewer") {
		t.Errorf("the review model should be visible in the log:\n%s", logs.String())
	}
}

// Regression: a PR listed by label whose JSON omits labels (the client used to
// drop them) must still trigger the review, because discovery already proved the
// label is there.
func TestReviewTriggersEvenIfTheClientOmitsLabels(t *testing.T) {
	h := newHarness(t)
	h.gh.PRs = []github.PR{{
		Number: 12, Title: "work", HeadRef: "agent/issue-12", BaseRef: "main",
		HeadRefOID: "sha12", Labels: []string{"review:go"},
	}}
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-12")
	gitRun(t, h.cloneDir, "worktree", "add", "-q", "-b", "agent/issue-12", dir, "origin/main")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: work")
	gitRun(t, dir, "push", "-q", "origin", "agent/issue-12")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/12/head", sha)
	h.gh.PRs[0].HeadRefOID = sha

	e, logs := h.engine(t, &stubDriver{file: prompts.ReviewFile, content: "## Summary\nfine\n"})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Reviews[12]) != 1 {
		t.Errorf("no review posted: %v", h.gh.Calls)
	}
}
