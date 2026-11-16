package stub

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/brain"
)

func TestRunWritesDeterministicFile(t *testing.T) {
	dir := t.TempDir()
	var log bytes.Buffer
	res, err := New().Run(context.Background(), brain.Spec{Prompt: "hello", Workdir: dir}, &log)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.Driver != "stub" {
		t.Errorf("result = %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "issuewright-stub.txt"))
	if err != nil {
		t.Fatalf("stub did not write a file: %v", err)
	}
	if !strings.Contains(string(raw), "prompt bytes: 5") {
		t.Errorf("file = %q", raw)
	}
	if !strings.Contains(log.String(), "message_end") {
		t.Errorf("stub should emit an event so the log parses like a real run: %q", log.String())
	}
}

func TestRunFailureAndNoop(t *testing.T) {
	d := &Driver{File: "x.txt", Fail: true}
	if _, err := d.Run(context.Background(), brain.Spec{Workdir: t.TempDir()}, &bytes.Buffer{}); err == nil {
		t.Fatal("Fail did not fail")
	}

	t.Setenv("STUB_NOOP", "1")
	dir := t.TempDir()
	if _, err := New().Run(context.Background(), brain.Spec{Workdir: dir}, &bytes.Buffer{}); err != nil {
		t.Fatalf("noop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "issuewright-stub.txt")); !os.IsNotExist(err) {
		t.Error("noop wrote a file")
	}
}

func TestCapsAndName(t *testing.T) {
	d := New()
	if d.Name() != "stub" {
		t.Errorf("name = %q", d.Name())
	}
	if !d.Caps().Resume {
		t.Error("stub should claim Resume so the resume path is testable")
	}
}

func TestRegisteredUnlessNostub(t *testing.T) {
	if _, err := brain.Get("stub"); err != nil {
		t.Fatalf("stub not registered: %v", err)
	}
}
