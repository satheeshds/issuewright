package prompts

import (
	"strings"
	"testing"
)

func TestRenderSubstitutesEverything(t *testing.T) {
	got, err := Render("implement", Inputs{
		Repo: "acme/demo", Number: 39, Title: "Improve observability",
		Body: "add metrics", Branch: "agent/issue-39", BaseBranch: "main",
		Workdir:       "/srv/agent/work/demo/issue-39",
		Rules:         "schema first, then make sqlc",
		WriteDenylist: []string{"db/*.go"},
		NeverRun:      []string{"make migrate"},
		SecretPaths:   []string{".env"},
		Notes:         []string{"note one"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"issue #39", "acme/demo", "Improve observability", "add metrics",
		"agent/issue-39", "schema first, then make sqlc",
		"- db/*.go", "- make migrate", "- .env", "note one",
		"/srv/agent/work/demo/issue-39",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt missing %q", want)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unsubstituted placeholder left: %s", firstPlaceholder(got))
	}
}

func TestRenderEmptyBodyAndLists(t *testing.T) {
	got, err := Render("implement", Inputs{Repo: "a/b", Number: 1, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "issue body is empty") {
		t.Error("empty body should tell the agent to infer or block")
	}
	if !strings.Contains(got, "(none declared)") {
		t.Error("empty lists should say so")
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unsubstituted placeholder: %s", firstPlaceholder(got))
	}
}

func TestRenderReviewIsReadOnly(t *testing.T) {
	got, err := Render("review", Inputs{Repo: "a/b", Number: 7, Title: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"read-only", "REVIEW.md", "cannot write"} {
		if !strings.Contains(got, want) {
			t.Errorf("review prompt missing %q", want)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unsubstituted placeholder: %s", firstPlaceholder(got))
	}
}

func TestRenderUnknownWorkflow(t *testing.T) {
	if _, err := Render("triage", Inputs{}); err == nil {
		t.Fatal("expected an error for a workflow with no template")
	}
}

func TestSummaryTemplateExists(t *testing.T) {
	if s := Summary(); !strings.Contains(s, "## Checks") {
		t.Errorf("summary template = %q", s)
	}
}

func firstPlaceholder(s string) string {
	i := strings.Index(s, "{{")
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], "}}")
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j+2]
}
