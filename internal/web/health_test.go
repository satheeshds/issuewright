package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeRunner answers per-command, so the tests describe systemd and git without
// needing either.
func fakeRunner(t *testing.T, responses map[string]string) Runner {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name
		for _, a := range args {
			key += " " + a
		}
		for prefix, out := range responses {
			if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
				return []byte(out), nil
			}
		}
		return nil, os.ErrNotExist
	}
}

func TestTimersFromListTimersJSON(t *testing.T) {
	now := time.Now()
	next := now.Add(4 * time.Minute).UnixMicro()
	last := now.Add(-6 * time.Minute).UnixMicro()
	run := fakeRunner(t, map[string]string{
		"systemctl list-timers": `[{"next":` + itoa64(next) + `,"last":` + itoa64(last) +
			`,"unit":"issuewright@daily-fish.timer","activates":"issuewright@daily-fish.service"},` +
			`{"next":0,"last":` + itoa64(last) + `,"unit":"issuewright-token.timer"}]`,
	})
	got := Timers(context.Background(), run, []string{"issuewright@daily-fish.timer", "issuewright-token.timer", "missing.timer"})
	if len(got) != 3 {
		t.Fatalf("timers = %+v", got)
	}
	if got[0].Unit != "issuewright@daily-fish.timer" || got[0].NextIn == "" || got[0].Overdue {
		t.Errorf("first timer = %+v", got[0])
	}
	if !got[1].Invalid {
		t.Errorf("a timer with no next elapse should be flagged: %+v", got[1])
	}
	if !got[2].Invalid || !got[2].Overdue {
		t.Errorf("a timer systemd does not know must be flagged: %+v", got[2])
	}
}

func TestTimersAreOverdueWhenTheClockMovedPast(t *testing.T) {
	now := time.Now()
	run := fakeRunner(t, map[string]string{
		"systemctl list-timers": `[{"next":` + itoa64(now.Add(-90*time.Second).UnixMicro()) +
			`,"last":` + itoa64(now.Add(-11*time.Minute).UnixMicro()) + `,"unit":"t.timer"}]`,
	})
	got := Timers(context.Background(), run, []string{"t.timer"})
	if !got[0].Overdue {
		t.Errorf("a past elapse is overdue: %+v", got[0])
	}
}

func TestTimersFallBackToShow(t *testing.T) {
	run := fakeRunner(t, map[string]string{
		"systemctl show x.timer": "NextElapseUSecRealtime=Thu 2026-11-26 16:10:15 IST\nLastTriggerUSec=Thu 2026-11-26 16:00:17 IST",
	})
	got := Timers(context.Background(), run, []string{"x.timer"})
	if len(got) != 1 || got[0].Next.IsZero() {
		t.Fatalf("fallback parse = %+v", got[0])
	}
	if got[0].Last.IsZero() {
		t.Errorf("last trigger not parsed: %+v", got[0])
	}
}

func TestTokenState(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)

	// No file: a PAT setup, nothing wrong.
	if st := ReadTokenState(context.Background(), filepath.Join(dir, "nope"), now); st.Configured || st.Stale {
		t.Errorf("absent expiry should be neutral: %+v", st)
	}

	path := filepath.Join(dir, "token.expiry")
	future := now.Add(48 * time.Minute).Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(future+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := ReadTokenState(context.Background(), path, now)
	if !st.Configured || st.Stale || st.Note != "" {
		t.Errorf("valid token = %+v", st)
	}
	if st.ExpiresIn != "48m00s" {
		t.Errorf("expires in = %q", st.ExpiresIn)
	}

	// About to expire: say so rather than showing green.
	soon := now.Add(5 * time.Minute).Format(time.RFC3339)
	os.WriteFile(path, []byte(soon), 0o600)
	if st := ReadTokenState(context.Background(), path, now); st.Note != "expires soon" {
		t.Errorf("soon = %+v", st)
	}

	// Past: stale, with the age spelled out.
	past := now.Add(-2 * time.Hour).Format(time.RFC3339)
	os.WriteFile(path, []byte(past), 0o600)
	st = ReadTokenState(context.Background(), path, now)
	if !st.Stale || st.Note == "" {
		t.Errorf("stale = %+v", st)
	}

	// Corrupt: report, do not crash.
	os.WriteFile(path, []byte("yesterday"), 0o600)
	if st := ReadTokenState(context.Background(), path, now); !st.Configured || st.Note == "" {
		t.Errorf("unreadable = %+v", st)
	}
}

func TestReadClone(t *testing.T) {
	dir := t.TempDir()
	if st := ReadClone(context.Background(), fakeRunner(t, nil), dir); !st.Missing {
		t.Errorf("no .git should be missing: %+v", st)
	}

	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := fakeRunner(t, map[string]string{
		"git -C " + dir + " log": "abc1234\x00Display app version (#54)\x002 hours ago",
	})
	st := ReadClone(context.Background(), run, dir)
	if st.Missing || st.Head != "abc1234" || st.Subject != "Display app version (#54)" || st.Age != "2 hours ago" {
		t.Errorf("clone state = %+v", st)
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
