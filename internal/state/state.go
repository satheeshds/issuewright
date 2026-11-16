// Package state keeps the runner's small amount of memory: one lock per repo so
// two ticks never overlap, and one claim record per in-flight job so a killed run
// resumes instead of restarting.
//
// Everything here is plain files under /srv/agent/state — greppable, deletable,
// and surviving the machine's nightly suspend.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrLocked means another tick already holds this repo's lock.
var ErrLocked = errors.New("another tick is already running for this repo")

// Store is the state directory.
type Store struct {
	Dir string
}

// New returns a Store rooted at dir.
func New(dir string) *Store { return &Store{Dir: dir} }

// RepoDir is the per-repo state directory.
func (s *Store) RepoDir(repo string) string {
	return filepath.Join(s.Dir, repo)
}

// Lock is a held repository lock.
type Lock struct {
	file *os.File
	path string
}

// TryLock takes the per-repo lock without blocking. A second call for the same
// repo returns ErrLocked, which is how overlapping timer ticks stay harmless.
func (s *Store) TryLock(repo string) (*Lock, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, repo+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	// Useful when a human wonders who is running.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0)
	return &Lock{file: f, path: path}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	cerr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return cerr
}

// Path is the lock file path, for logs.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Claim is one job the runner has picked up.
type Claim struct {
	Repo     string    `json:"repo"`
	Workflow string    `json:"workflow"`
	Number   int       `json:"number"`
	Kind     string    `json:"kind"`
	Branch   string    `json:"branch"`
	Attempt  int       `json:"attempt"`
	Started  time.Time `json:"started_at"`
	State    string    `json:"state"`
	Detail   string    `json:"detail,omitempty"`
}

func (s *Store) claimPath(repo, workflow string, number int) string {
	return filepath.Join(s.RepoDir(repo), fmt.Sprintf("%s-%d.json", workflow, number))
}

// SaveClaim writes a claim atomically.
func (s *Store) SaveClaim(c Claim) error {
	if err := os.MkdirAll(s.RepoDir(c.Repo), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := s.claimPath(c.Repo, c.Workflow, c.Number)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadClaim returns the claim for a job, if any.
func (s *Store) LoadClaim(repo, workflow string, number int) (Claim, bool, error) {
	raw, err := os.ReadFile(s.claimPath(repo, workflow, number))
	if errors.Is(err, os.ErrNotExist) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, err
	}
	var c Claim
	if err := json.Unmarshal(raw, &c); err != nil {
		// A corrupt claim must not wedge the repo: treat it as absent.
		return Claim{}, false, nil
	}
	return c, true, nil
}

// DeleteClaim forgets a job (after it landed or was blocked).
func (s *Store) DeleteClaim(repo, workflow string, number int) error {
	err := os.Remove(s.claimPath(repo, workflow, number))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Claims lists every claim in a repo, sorted by workflow then number.
func (s *Store) Claims(repo string) ([]Claim, error) {
	entries, err := os.ReadDir(s.RepoDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Claim
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.RepoDir(repo), e.Name()))
		if err != nil {
			continue
		}
		var c Claim
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// NextAttempt bumps the attempt counter for a job, so a run log can say "third
// try" and a storm of retries is visible.
func (s *Store) NextAttempt(repo, workflow string, number int) int {
	c, ok, err := s.LoadClaim(repo, workflow, number)
	if err != nil || !ok {
		return 1
	}
	return c.Attempt + 1
}

// LogPath returns the run log path for a job, creating the directory.
func (s *Store) LogPath(logDir, repo, workflow string, number int, now time.Time) (string, error) {
	dir := filepath.Join(logDir, repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s-run.jsonl", workflow, strconv.Itoa(number),
		now.UTC().Format("20060102T150405"))
	return filepath.Join(dir, name), nil
}
