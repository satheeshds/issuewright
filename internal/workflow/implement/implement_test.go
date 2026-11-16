package implement

import (
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/workflow"
)

func job() workflow.Job {
	return workflow.Job{
		Kind:       workflow.IssueAssigned,
		Repo:       "acme/demo",
		Number:     39,
		Title:      "Add metrics",
		Body:       "instrument the API",
		Branch:     "agent/issue-39",
		BaseBranch: "main",
		Workdir:    "/w/issue-39",
		Rules:      "schema first",
		Protect: workflow.Protect{
			WriteDenylist: []string{"db/*.go"},
			NeverRun:      []string{"make migrate"},
			SecretPaths:   []string{".env"},
		},
		Notes: []string{"needs make migrate"},
	}
}

func TestPermsWrite(t *testing.T) {
	p := New().Perms()
	if !p.Write || !p.Comment || !p.Label || !p.Checks {
		t.Errorf("implement must claim all four permissions: %+v", p)
	}
}

func TestTriggers(t *testing.T) {
	ts := New().Triggers()
	if len(ts) != 2 || ts[0].Kind != workflow.IssueAssigned || ts[1].Kind != workflow.IssueLabel {
		t.Errorf("triggers = %+v", ts)
	}
}

func TestFreshPromptCarriesJobAndRules(t *testing.T) {
	spec, err := New().Prompt(job())
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if spec.Workdir != "/w/issue-39" {
		t.Errorf("workdir = %q", spec.Workdir)
	}
	for _, want := range []string{"issue #39", "acme/demo", "Add metrics", "schema first", "db/*.go", "make migrate", "needs make migrate"} {
		if !strings.Contains(spec.Prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if spec.Continue {
		t.Error("a fresh run must not ask for continuation")
	}
}

func TestResumePromptIsShortAndPointsAtExistingWork(t *testing.T) {
	j := job()
	j.Resume = true
	j.ExistingWork = "abc123 feat: add metric collector"
	spec, err := New().Prompt(j)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(spec.Prompt, "Continue and finish") {
		t.Errorf("resume prompt = %q", spec.Prompt)
	}
	if !strings.Contains(spec.Prompt, "abc123") {
		t.Error("resume prompt must name existing work")
	}
	if strings.Contains(spec.Prompt, "## What to do") {
		t.Error("resume prompt should not repeat the whole task description")
	}
	if len(spec.Prompt) > 1200 {
		t.Errorf("resume prompt too long (%d bytes)", len(spec.Prompt))
	}
}

func TestEmptyJobRejected(t *testing.T) {
	if _, err := New().Prompt(workflow.Job{}); err == nil {
		t.Fatal("expected ErrNoJob")
	}
}
