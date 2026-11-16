package review

import (
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/workflow"
)

func TestPermsCannotWrite(t *testing.T) {
	p := New().Perms()
	if p.Write {
		t.Fatal("a reviewer that can write is not a reviewer")
	}
	if !p.Comment || !p.Checks {
		t.Errorf("review must be allowed to comment and run read-only checks: %+v", p)
	}
	if p.Label {
		t.Error("review must not relabel the PR")
	}
}

func TestPromptIsReadOnlyAndSelfContained(t *testing.T) {
	spec, err := New().Prompt(workflow.Job{
		Kind:       workflow.PROpened,
		Repo:       "acme/demo",
		Number:     7,
		Title:      "Add metrics",
		Body:       "the PR body",
		Branch:     "agent/issue-7",
		BaseBranch: "main",
		Workdir:    "/w/review-7",
		Rules:      "no generated edits",
		Protect:    workflow.Protect{SecretPaths: []string{".env"}},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	for _, want := range []string{"pull request #7", "read-only", "REVIEW.md", "no generated edits", ".env"} {
		if !strings.Contains(spec.Prompt, want) {
			t.Errorf("review prompt missing %q", want)
		}
	}
	if strings.Contains(spec.Prompt, "{{") {
		t.Error("unsubstituted placeholder")
	}
}

func TestEmptyJobRejected(t *testing.T) {
	if _, err := New().Prompt(workflow.Job{}); err == nil {
		t.Fatal("expected ErrNoJob")
	}
}
