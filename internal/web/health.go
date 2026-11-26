package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner runs a command and returns its standard output. Injected so the tests do
// not need systemd or a git checkout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the real thing.
func ExecRunner(_ context.Context, name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Timer is one systemd timer as the page shows it.
type Timer struct {
	Unit     string    `json:"unit"`
	Next     time.Time `json:"next"`
	Last     time.Time `json:"last"`
	NextIn   string    `json:"next_in"`
	Overdue  bool      `json:"overdue"`
	Invalid  bool      `json:"invalid"`
	ErrorMsg string    `json:"error,omitempty"`
}

// TokenState is what the minter left behind.
type TokenState struct {
	Configured bool      `json:"configured"`
	ExpiresAt  time.Time `json:"expires_at"`
	ExpiresIn  string    `json:"expires_in"`
	Stale      bool      `json:"stale"`
	Note       string    `json:"note,omitempty"`
}

// CloneState is the agent's checkout of a repo.
type CloneState struct {
	Path    string `json:"path"`
	Head    string `json:"head,omitempty"`
	Subject string `json:"subject,omitempty"`
	Age     string `json:"age,omitempty"`
	Behind  string `json:"note,omitempty"`
	Missing bool   `json:"missing"`
}

// Timers lists the issuewright timers through systemctl, falling back to a per-unit
// query when `list-timers --output=json` is unavailable (older systemd).
func Timers(ctx context.Context, run Runner, units []string) []Timer {
	now := time.Now()
	if out, err := run(ctx, "systemctl", "list-timers", "--output=json", "--no-pager"); err == nil {
		var raw []struct {
			Next    int64  `json:"next"`
			Last    int64  `json:"last"`
			Unit    string `json:"unit"`
			NextRaw string `json:"-"`
		}
		if json.Unmarshal(out, &raw) == nil && len(raw) > 0 {
			byUnit := map[string]Timer{}
			for _, r := range raw {
				t := Timer{Unit: r.Unit}
				if r.Next > 0 {
					// systemd's JSON reports microseconds since the epoch.
					t.Next = time.UnixMicro(r.Next)
				}
				if r.Last > 0 {
					t.Last = time.UnixMicro(r.Last)
				}
				byUnit[r.Unit] = t
			}
			out := make([]Timer, 0, len(units))
			for _, u := range units {
				t, ok := byUnit[u]
				if !ok {
					t = Timer{Unit: u, Invalid: true, Overdue: true}
				}
				out = append(out, finish(t, now))
			}
			return out
		}
	}

	out := make([]Timer, 0, len(units))
	for _, u := range units {
		t := Timer{Unit: u}
		raw, err := run(ctx, "systemctl", "show", u,
			"-p", "NextElapseUSecRealtime", "-p", "LastTriggerUSec", "--no-pager")
		if err != nil {
			t.Invalid = true
			t.ErrorMsg = "not installed"
			out = append(out, finish(t, now))
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch key {
			case "NextElapseUSecRealtime":
				t.Next = parseSystemdTime(value)
			case "LastTriggerUSec":
				t.Last = parseSystemdTime(value)
			}
		}
		out = append(out, finish(t, now))
	}
	return out
}

// parseSystemdTime handles both spellings systemd uses: a human date and a
// microsecond epoch.
func parseSystemdTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" {
		return time.Time{}
	}
	if usec, err := strconv.ParseInt(v, 10, 64); err == nil && usec > 1e15 {
		return time.UnixMicro(usec)
	}
	for _, layout := range []string{"Mon 2006-01-02 15:04:05 MST", time.RFC3339, "2006-01-02 15:04:05 MST"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

func finish(t Timer, now time.Time) Timer {
	if t.Next.IsZero() {
		if !t.Invalid {
			t.Invalid = true
		}
		return t
	}
	t.NextIn = HumanDuration(t.Next.Sub(now))
	t.Overdue = t.Next.Before(now)
	return t
}

// ReadTokenState reads the expiry file the minter writes. Absent means "not a
// GitHub App setup", which is not a problem — just nothing to report.
func ReadTokenState(ctx context.Context, path string, now time.Time) TokenState {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TokenState{Configured: false, Note: "no App token expiry recorded (PAT or gh login)"}
	}
	value := strings.TrimSpace(string(raw))
	expires, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return TokenState{Configured: true, Note: "unreadable expiry: " + value}
	}
	st := TokenState{Configured: true, ExpiresAt: expires, ExpiresIn: HumanDuration(expires.Sub(now))}
	if expires.Before(now) {
		st.Stale = true
		st.Note = "expired " + HumanDuration(now.Sub(expires)) + " ago"
	} else if expires.Sub(now) < 10*time.Minute {
		st.Note = "expires soon"
	}
	return st
}

// ReadClone reports the head of the agent's checkout, so the page can show which
// commit the next run will start from.
func ReadClone(ctx context.Context, run Runner, path string) CloneState {
	st := CloneState{Path: path}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		st.Missing = true
		st.Behind = "no clone yet"
		return st
	}
	out, err := run(ctx, "git", "-C", path, "log", "-1", "--format=%h%x00%s%x00%cr")
	if err != nil {
		st.Behind = "could not read git log"
		return st
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\x00")
	if len(parts) > 0 {
		st.Head = parts[0]
	}
	if len(parts) > 1 {
		st.Subject = parts[1]
	}
	if len(parts) > 2 {
		st.Age = parts[2]
	}
	return st
}

// VersionOrUnknown keeps the header honest when the build has no version stamped.
func VersionOrUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}
