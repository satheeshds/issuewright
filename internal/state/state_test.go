package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLockIsExclusive(t *testing.T) {
	s := New(t.TempDir())
	first, err := s.TryLock("demo")
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Release()

	if _, err := s.TryLock("demo"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock error = %v, want ErrLocked", err)
	}

	// A different repo must not be blocked.
	other, err := s.TryLock("other")
	if err != nil {
		t.Fatalf("other repo lock: %v", err)
	}
	other.Release()

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := s.TryLock("demo")
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	again.Release()
}

func TestLockFileRecordsThePid(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	l, err := s.TryLock("demo")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	raw, err := os.ReadFile(filepath.Join(dir, "demo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Error("lock file should record who holds it")
	}
}

func TestClaimRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	c := Claim{Repo: "demo", Workflow: "implement", Number: 39, Kind: "issue_assigned",
		Branch: "agent/issue-39", Attempt: 2, Started: time.Now().UTC().Truncate(time.Second), State: "running"}
	if err := s.SaveClaim(c); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}
	got, ok, err := s.LoadClaim("demo", "implement", 39)
	if err != nil || !ok {
		t.Fatalf("LoadClaim = %v %v", ok, err)
	}
	if got.Number != 39 || got.Branch != "agent/issue-39" || got.Attempt != 2 {
		t.Errorf("claim = %+v", got)
	}

	claims, err := s.Claims("demo")
	if err != nil || len(claims) != 1 {
		t.Fatalf("Claims = %v %v", claims, err)
	}

	if err := s.DeleteClaim("demo", "implement", 39); err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	if _, ok, _ := s.LoadClaim("demo", "implement", 39); ok {
		t.Error("claim survived deletion")
	}
	if err := s.DeleteClaim("demo", "implement", 39); err != nil {
		t.Errorf("deleting a missing claim must be a no-op: %v", err)
	}
}

func TestCorruptClaimIsTreatedAsAbsent(t *testing.T) {
	s := New(t.TempDir())
	if err := os.MkdirAll(s.RepoDir("demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.RepoDir("demo"), "implement-1.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LoadClaim("demo", "implement", 1); err != nil || ok {
		t.Fatalf("corrupt claim = %v %v", ok, err)
	}
	if claims, err := s.Claims("demo"); err != nil || len(claims) != 0 {
		t.Fatalf("Claims on corrupt store = %v %v", claims, err)
	}
}

func TestNextAttempt(t *testing.T) {
	s := New(t.TempDir())
	if got := s.NextAttempt("demo", "implement", 5); got != 1 {
		t.Errorf("first attempt = %d", got)
	}
	_ = s.SaveClaim(Claim{Repo: "demo", Workflow: "implement", Number: 5, Attempt: 2})
	if got := s.NextAttempt("demo", "implement", 5); got != 3 {
		t.Errorf("next attempt = %d", got)
	}
}

func TestLogPath(t *testing.T) {
	s := New(t.TempDir())
	logDir := t.TempDir()
	path, err := s.LogPath(logDir, "demo", "implement", 39, time.Date(2026, 11, 16, 21, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if filepath.Base(path) != "implement-39-20261116T213000-run.jsonl" {
		t.Errorf("log name = %s", filepath.Base(path))
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("log dir not created: %v", err)
	}
}
