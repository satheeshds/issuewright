package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func timeUnix(sec int64) time.Time { return time.Unix(sec, 0) }

// The fixtures are events copied from a real run on the box (implement-50), trimmed
// to the fields this page reads. Shapes, not guesses.
func TestSummarizeRealRun(t *testing.T) {
	sum, err := SummarizeFile(filepath.Join("testdata", "implement-51-20261125T121500-run.jsonl"))
	if err != nil {
		t.Fatalf("SummarizeFile: %v", err)
	}
	if sum.Workflow != "implement" || sum.Number != 51 {
		t.Errorf("workflow/number from the filename = %q/%d", sum.Workflow, sum.Number)
	}
	if sum.ToolCount != 4 || len(sum.ToolCalls) != 4 {
		t.Fatalf("tool calls = %d (%v)", sum.ToolCount, sum.ToolCalls)
	}
	if !strings.Contains(strings.Join(sum.ToolCalls, " "), "git log") {
		t.Errorf("tool commands not captured: %v", sum.ToolCalls)
	}
	if sum.FinalText == "" {
		t.Error("the final assistant text was not captured")
	}
	if sum.Truncated {
		t.Error("a complete file must not be reported as truncated")
	}
}

// A killed run is the interesting case: the last line is half-written, and the page
// still has to show what happened before the kill.
func TestSummarizeTruncatedRun(t *testing.T) {
	sum, err := SummarizeFile(filepath.Join("testdata", "truncated.jsonl"))
	if err != nil {
		t.Fatalf("SummarizeFile: %v", err)
	}
	if !sum.Truncated {
		t.Fatal("a file without a trailing newline must report truncated")
	}
	if sum.ToolCount < 2 {
		t.Errorf("events before the truncation were lost: %v", sum.ToolCalls)
	}
	// The un-finished response still cost something, and we saw its usage.
	if sum.CostUSD == 0 {
		t.Error("cost of the in-flight response was dropped")
	}
}

// A run that never produced a final message (killed mid-turn) must not invent one.
func TestSummarizeWithoutFinalMessage(t *testing.T) {
	sum, err := SummarizeFile(filepath.Join("testdata", "no-final-message.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if sum.FinalText != "" {
		t.Errorf("final text should be empty, got %q", sum.FinalText)
	}
	if sum.ToolCount != 1 {
		t.Errorf("tool count = %d", sum.ToolCount)
	}
}

// forbidden-command detection lives in the checks package; here we only need the
// command text to survive so the page can show it.
func TestSummarizeKeepsForbiddenLookingCommands(t *testing.T) {
	sum, err := SummarizeFile(filepath.Join("testdata", "truncated.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sum.ToolCalls, "\n")
	if !strings.Contains(joined, "make db-reset") {
		t.Errorf("the command a never_run rule would match is missing: %v", sum.ToolCalls)
	}
}

func TestListRunsNewestFirstAndLimited(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"tool_execution_start","args":{"command":"ls"}}` + "\n"
	// Written oldest-first so the mtimes are unambiguous.
	for i, name := range []string{
		"implement-1-20260101T000000-run.jsonl",
		"implement-2-20260101T000100-run.jsonl",
		"review-3-20260101T000200-run.jsonl",
		"notes.txt", // ignored: not a run log
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(1700000000+i*60), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := ListRuns(dir, 2)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	if runs[0].Number != 3 || runs[1].Number != 2 {
		t.Errorf("newest first, got %+v", runs)
	}
	if _, err := ListRuns(filepath.Join(dir, "missing"), 5); err != nil {
		t.Errorf("a missing log dir is not an error: %v", err)
	}
}

func TestNewestRunFindsTheJob(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"tool_execution_start","args":{"command":"ls"}}` + "\n"
	for _, name := range []string{
		"implement-7-20260101T000000-run.jsonl",
		"review-7-20260101T000100-run.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := NewestRun(dir, "review", 7)
	if !ok || got.Workflow != "review" {
		t.Fatalf("NewestRun = %+v %v", got, ok)
	}
	if _, ok := NewestRun(dir, "respond", 7); ok {
		t.Error("a workflow with no logs must not be found")
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[string]string{
		HumanDuration(0):          "—",
		HumanDuration(45 * 1e9):   "45s",
		HumanDuration(90 * 1e9):   "1m30s",
		HumanDuration(3700 * 1e9): "1h01m",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("duration = %q, want %q", got, want)
		}
	}
}
