package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func moduleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNewWorkflowWritesCompilableLookingSource(t *testing.T) {
	dir := moduleDir(t)
	files, err := New("workflow", "triage", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	var src string
	for path, body := range files {
		if !strings.Contains(path, filepath.Join("internal", "workflow", "triage")) {
			t.Errorf("unexpected path %s", path)
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src = body
	}
	for _, want := range []string{"package triage", `"triage"`, "workflow.Register", "func (e *Engine) Perms()", "func (e *Engine) Prompt("} {
		if !strings.Contains(src, want) {
			t.Errorf("generated source missing %q\n%s", want, src)
		}
	}
	if strings.Contains(src, "example.com/demo") || strings.Contains(src, "%!") {
		t.Errorf("module path leaked into the source:\n%s", src)
	}
}

func TestNewBrainWritesDriver(t *testing.T) {
	dir := moduleDir(t)
	files, err := New("brain", "claude-code", dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for path, body := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if !strings.Contains(path, filepath.Join("internal", "brain", "claudecode")) {
			t.Errorf("dashed name not normalised in path: %s", path)
		}
		for _, want := range []string{"package claudecode", "brain.Register", `"claude-code"`, "func (d *Driver) Caps()"} {
			if !strings.Contains(body, want) {
				t.Errorf("driver source missing %q", want)
			}
		}
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	dir := moduleDir(t)
	if _, err := New("workflow", "Bad Name", dir); err == nil {
		t.Error("bad name accepted")
	}
	if _, err := New("gadget", "ok", dir); err == nil {
		t.Error("bad kind accepted")
	}
	if _, err := New("workflow", "ok", t.TempDir()); err == nil {
		t.Error("missing go.mod accepted")
	}
}

func TestGeneratedWorkflowHasNoPlaceholderLeft(t *testing.T) {
	dir := moduleDir(t)
	files, err := New("workflow", "fix-ci", dir)
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range files {
		if strings.Contains(body, "@@") {
			t.Errorf("%s still has a placeholder", path)
		}
		if strings.Contains(body, "%[") {
			t.Errorf("%s has a stray Sprintf verb", path)
		}
	}
}

func TestDocsRow(t *testing.T) {
	if row := DocsRow("workflow", "triage"); !strings.Contains(row, "`triage`") {
		t.Errorf("row = %q", row)
	}
	if row := DocsRow("brain", "claude-code"); !strings.Contains(row, "`claude-code`") {
		t.Errorf("row = %q", row)
	}
}
