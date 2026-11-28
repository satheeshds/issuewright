package web

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satheeshds/issuewright/internal/config"
)

// fixture builds a state root that looks like a provisioned box: one repo with a
// running job, a blocked job, a finished job, and a run log for each.
func fixture(t *testing.T) (Options, string) {
	t.Helper()
	root := t.TempDir()
	workroot := filepath.Join(root, "work")
	logdir := filepath.Join(root, "logs", "demo")
	statedir := filepath.Join(root, "state", "demo")
	for _, d := range []string{workroot, logdir, statedir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A profile whose label names differ from the defaults, to prove the page reads it.
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repoPath, config.ProfileDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// ReadClone looks for .git before asking git anything.
	if err := os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(repoPath, config.ProfileDir, config.ProfileFile), `
version: 1
issue:
  force_label: build:go
  in_progress_label: build:running
  blocked_label: build:stuck
  done_label: build:finished
  review_label: review:again
`)
	write(filepath.Join(statedir, "implement-51.json"),
		`{"repo":"demo","workflow":"implement","number":51,"kind":"issue_label","branch":"agent/issue-51","attempt":2,"started_at":"2026-11-25T13:50:00Z","state":"blocked","detail":"push rejected: workflows permission"}`)
	write(filepath.Join(statedir, "implement-52.json"),
		`{"repo":"demo","workflow":"implement","number":52,"kind":"issue_label","branch":"agent/issue-52","attempt":1,"started_at":"2026-11-25T14:00:00Z","state":"running","detail":""}`)
	write(filepath.Join(statedir, "review-47.json"),
		`{"repo":"demo","workflow":"review","number":47,"kind":"pr_label","attempt":1,"started_at":"2026-11-25T12:00:00Z","state":"done","detail":"reviewed:abc1234"}`)
	write(filepath.Join(logdir, "implement-51-20261125T135000-run.jsonl"),
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"git push origin agent/issue-51"}}`+"\n")

	cfg := &config.Config{
		Agent: config.AgentConfig{Workroot: workroot, Logdir: filepath.Join(root, "logs")},
		Repos: []config.Repo{{Name: "demo", Slug: "acme/demo", Path: repoPath, Bot: "demo-bot[bot]"}},
	}
	return Options{
		Config:  cfg,
		Refresh: 15 * time.Second,
		Now:     func() time.Time { return time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC) },
		Run: fakeRunner(t, map[string]string{
			"systemctl list-timers":       `[{"next":` + itoa64(time.Now().Add(5*time.Minute).UnixMicro()) + `,"last":0,"unit":"issuewright@demo.timer"}]`,
			"git -C " + repoPath + " log": "abc1234\x00the last commit\x003 hours ago",
		}),
		Version: "v0.2.0",
	}, root
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	opt, _ := fixture(t)
	srv, err := New(opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := get(t, srv.Handler(), "GET", "/healthz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v0.2.0") {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzFailsWhenStateIsGone(t *testing.T) {
	opt, root := fixture(t)
	if err := os.RemoveAll(filepath.Join(root, "state")); err != nil {
		t.Fatal(err)
	}
	srv, _ := New(opt)
	if rec := get(t, srv.Handler(), "GET", "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz without state = %d", rec.Code)
	}
}

func TestAPIStatusSplitsJobsByState(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	rec := get(t, srv.Handler(), "GET", "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var d Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Repos) != 1 {
		t.Fatalf("repos = %+v", d.Repos)
	}
	repo := d.Repos[0]
	if len(repo.Running) != 1 || repo.Running[0].Number != 52 {
		t.Errorf("running = %+v", repo.Running)
	}
	if len(repo.Blocked) != 1 || repo.Blocked[0].Number != 51 {
		t.Errorf("blocked = %+v", repo.Blocked)
	}
	if !strings.Contains(repo.Blocked[0].Detail, "workflows permission") {
		t.Errorf("the reason must be shown verbatim: %q", repo.Blocked[0].Detail)
	}
	if len(repo.Finished) != 1 || repo.Finished[0].Workflow != "review" {
		t.Errorf("finished = %+v", repo.Finished)
	}
	if repo.Clone.Head != "abc1234" {
		t.Errorf("clone = %+v", repo.Clone)
	}
}

// The page must print the label the repo actually uses, from its own profile.
func TestDashboardUsesTheRepoProfilesLabelNames(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	rec := get(t, srv.Handler(), "GET", "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard = %d:\n%s", rec.Code, body)
	}

	if !strings.Contains(body, "build:stuck") {
		t.Error("the blocked label from the repo profile is not shown")
	}
	if strings.Contains(body, "agent:blocked") && !strings.Contains(body, "build:stuck") {
		t.Error("the page used the default label instead of the repo's")
	}
	if !strings.Contains(body, "remove this label to retry") {
		t.Error("the blocked job does not say what unblocks it")
	}
	if !strings.Contains(body, "https://github.com/acme/demo/issues/51") {
		t.Error("a blocked issue should link to GitHub")
	}
	if !strings.Contains(body, "https://github.com/acme/demo/pull/47") {
		t.Error("a review claim should link to the pull request, not the issue")
	}
	if !strings.Contains(body, "git push origin agent/issue-51") {
		t.Error("the tail of the failing run should be visible")
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the page should refresh itself")
	}
}

func TestAPIRunsFiltersByNumber(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	rec := get(t, srv.Handler(), "GET", "/api/runs?repo=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("runs = %d %s", rec.Code, rec.Body)
	}
	var runs []RunSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Number != 51 || runs[0].Workflow != "implement" {
		t.Fatalf("runs = %+v", runs)
	}
	if got := get(t, srv.Handler(), "GET", "/api/runs?repo=demo&n=999"); len(got.Body.String()) > 5 {
		t.Errorf("filtering by an unknown number should return an empty list: %s", got.Body)
	}
	if rec := get(t, srv.Handler(), "GET", "/api/runs?repo=nope"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown repo = %d", rec.Code)
	}
}

func TestAPILogJSONAndRaw(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	name := "implement-51-20261125T135000-run.jsonl"

	rec := get(t, srv.Handler(), "GET", "/api/log/"+name+"?repo=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("log = %d %s", rec.Code, rec.Body)
	}
	var sum RunSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.ToolCount != 1 || sum.Number != 51 {
		t.Errorf("summary = %+v", sum)
	}

	raw := get(t, srv.Handler(), "GET", "/api/log/"+name+"?repo=demo&raw=1")
	if !strings.Contains(raw.Body.String(), "tool_execution_start") {
		t.Errorf("raw log = %q", raw.Body.String())
	}
	if ct := raw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("raw content type = %q", ct)
	}
}

// Path traversal must not reach outside the log directory.
func TestAPILogRejectsForeignNames(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	for _, name := range []string{"..%2F..%2Fetc%2Fpasswd", "notes.txt", "implement-51-run.jsonl"} {
		rec := get(t, srv.Handler(), "GET", "/api/log/"+name+"?repo=demo")
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 400/404", name, rec.Code)
		}
	}
}

// Read-only: a mutating verb is refused with an explanation, not a 404.
func TestMutatingVerbsAreRefused(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/status"},
		{"PUT", "/api/retry"},
		{"DELETE", "/api/log/implement-51-20261125T135000-run.jsonl"},
		{"POST", "/"},
	} {
		rec := get(t, srv.Handler(), tc.method, tc.path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "read-only") {
			t.Errorf("%s %s body should explain: %q", tc.method, tc.path, rec.Body.String())
		}
	}
}

func TestServerNeedsConfig(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected an error without a config")
	}
}

var _ = context.Background

// A template that fails mid-render must not send a half page and then a second
// header ("superfluous response.WriteHeader"), which is what a live box logged.
func TestBrokenTemplateGivesAClean500(t *testing.T) {
	opt, _ := fixture(t)
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	// A template that writes some bytes and then fails: exactly the bad case.
	srv.tpl = template.Must(template.New("broken").Parse(`<html>partial{{.NoSuchField}}`))

	rec := get(t, srv.Handler(), "GET", "/")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("a partial page was sent before the error: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "in the service log") {
		t.Errorf("the body should point at the log: %q", rec.Body.String())
	}
	// http.ResponseRecorder does not police double WriteHeader; a real server does,
	// and the buffered path is what avoids it. Assert we wrote the body once.
	if rec.Header().Get("Content-Type") == "text/html; charset=utf-8" {
		t.Error("the HTML content type was set even though the render failed")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	opt, _ := fixture(t)
	srv, _ := New(opt)
	for _, path := range []string{"/nope", "/favicon.ico", "/api"} {
		if rec := get(t, srv.Handler(), "GET", path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestDashboardShowsAWaitingConversation(t *testing.T) {
	opt, root := fixture(t)
	statedir := filepath.Join(root, "state", "demo")
	// A turn that ended with a question: the claim says asking, the conversation
	// knows which turn it is on.
	if err := os.WriteFile(filepath.Join(statedir, "implement-53.json"),
		[]byte(`{"repo":"demo","workflow":"implement","number":53,"kind":"issue_assigned","branch":"agent/issue-53","attempt":1,"started_at":"2026-11-25T14:30:00Z","state":"asking","detail":"waiting for an answer: postgres or sqlite?"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(statedir, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statedir, "conversations", "implement-53.json"),
		[]byte(`{"repo":"demo","workflow":"implement","number":53,"session_id":"iw-demo-implement-53","state":"asking","turns":3,"cost_usd":1.25}`), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, _ := New(opt)
	rec := get(t, srv.Handler(), "GET", "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard = %d:\n%s", rec.Code, body)
	}
	if !strings.Contains(body, "Waiting for an answer") {
		t.Error("a conversation waiting for a human should have its own section")
	}
	if !strings.Contains(body, "implement #53") || !strings.Contains(body, "postgres or sqlite") {
		t.Errorf("the question should be visible:\n%s", body)
	}
	if !strings.Contains(body, "turn 3") {
		t.Error("the turn count should be shown")
	}
	if !strings.Contains(body, "answer the question in a comment") {
		t.Error("the page should say what unblocks it")
	}
}
