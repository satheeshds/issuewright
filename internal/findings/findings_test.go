package findings

import (
	"strings"
	"testing"
)

func TestParseStructuredReview(t *testing.T) {
	raw := `---
title: adds telemetry
findings:
  - severity: blocking
    path: ./lib/telemetry.dart
    line: 30
    message: route arguments leak coordinates
  - path: lib/main.dart
    line: 12
    message: missing disposal
    suggestion: |
      telemetry.dispose();
---
## Summary
Mostly fine, one blocker.
`
	rev, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rev.Title != "adds telemetry" {
		t.Errorf("title = %q", rev.Title)
	}
	if len(rev.Findings) != 2 {
		t.Fatalf("findings = %+v", rev.Findings)
	}
	if rev.Findings[0].Severity != Blocking || rev.Findings[0].Path != "lib/telemetry.dart" || rev.Findings[0].Line != 30 {
		t.Errorf("first finding = %+v", rev.Findings[0])
	}
	if rev.Findings[1].Severity != NonBlocking {
		t.Errorf("a missing severity should default to non-blocking: %+v", rev.Findings[1])
	}
	if !strings.Contains(rev.Findings[1].CommentBody(), "```suggestion") {
		t.Errorf("suggestion not rendered: %q", rev.Findings[1].CommentBody())
	}
	if !strings.Contains(rev.Body, "## Summary") || strings.Contains(rev.Body, "severity:") {
		t.Errorf("body = %q", rev.Body)
	}
	if !HasBlocking(rev.Findings) {
		t.Error("HasBlocking missed a blocker")
	}
	if got := SummaryLine(rev.Findings); got != "1 blocking, 1 non-blocking" {
		t.Errorf("summary line = %q", got)
	}
}

// A reviewer that writes plain markdown must still work: the review is posted as
// one comment, exactly as before this format existed.
func TestParseProseOnlyReview(t *testing.T) {
	rev, err := Parse("## Summary\nlooks fine\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rev.Findings) != 0 {
		t.Errorf("findings = %+v", rev.Findings)
	}
	if rev.Body != "## Summary\nlooks fine" {
		t.Errorf("body = %q", rev.Body)
	}
	if SummaryLine(nil) != "no findings" {
		t.Errorf("empty summary = %q", SummaryLine(nil))
	}
}

func TestParseRejectsUselessFindings(t *testing.T) {
	for name, raw := range map[string]string{
		"no path":    "---\nfindings:\n  - message: something\n---\nbody",
		"no message": "---\nfindings:\n  - path: a.go\n---\nbody",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseToleratesMalformedFrontMatter(t *testing.T) {
	// Broken YAML is an error the engine catches and reports; it must not panic.
	if _, err := Parse("---\nfindings: [oops\n---\nbody"); err == nil {
		t.Fatal("expected a YAML error")
	}
	// A "---" that is not front matter stays prose.
	rev, err := Parse("---\nnot: front matter without a closing fence")
	if err != nil || rev.Body == "" {
		t.Fatalf("rev=%+v err=%v", rev, err)
	}
}
