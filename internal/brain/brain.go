// Package brain is the seam between the runner and whatever CLI thinks.
//
// The tick loop never knows which agent produced a change: it asks the registry
// for a driver by name, hands it a Spec, and reads the Result. A new agent CLI
// is a new package plus one Register call.
package brain

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Caps describe what a driver can do. The runner adapts to them rather than
// assuming: a driver without Resume gets a cold-start prompt instead of a
// resume flag it does not understand.
type Caps struct {
	Resume bool // can continue a previous session in the same directory
	// Conversation means the driver can open a *named* conversation and get its
	// history back on the next run, rather than "the latest session in this
	// directory". It is what makes one agent per issue possible: the runner hands
	// over Spec.Conversation and the driver either resumes it or creates it.
	Conversation bool
	Model        bool // accepts a model override
	Thinking     bool // accepts a thinking/reasoning level
	Streaming    bool // emits structured events (JSONL) rather than plain text
}

// Spec is one run: everything a driver needs and nothing about the repo.
type Spec struct {
	Prompt   string
	Workdir  string
	Model    string
	Thinking string
	Timeout  time.Duration
	Continue bool
	// Conversation is the driver-specific handle for this target's history — a
	// session id for a CLI that supports one. Empty means "no identity yet", and
	// the driver falls back to Continue.
	Conversation string
}

// Result is what a run produced.
type Result struct {
	Driver   string
	Model    string
	Thinking string
	ExitCode int
	Duration time.Duration
	// Summary is the agent's final text, when the driver exposes it.
	Summary string
	// CostUSD is best effort; zero means "not reported".
	CostUSD float64
	// Settled means the driver knows the run finished even if the CLI had to be
	// stopped (see the pi driver).
	Settled bool
}

// Driver is one agent CLI.
type Driver interface {
	Name() string
	Caps() Caps
	Run(ctx context.Context, spec Spec, log io.Writer) (Result, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Driver{}
)

// Register adds a driver. Drivers call this from init(). Registering the same
// name twice is a programming error and panics, as with database/sql.
func Register(d Driver) {
	if d == nil || d.Name() == "" {
		panic("brain: Register with nil or unnamed driver")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[d.Name()]; dup {
		panic("brain: duplicate driver " + d.Name())
	}
	registry[d.Name()] = d
}

// Get returns a driver by name.
func Get(name string) (Driver, error) {
	mu.RLock()
	defer mu.RUnlock()
	if d, ok := registry[name]; ok {
		return d, nil
	}
	return nil, &UnknownDriverError{Name: name, Known: namesLocked()}
}

// Names lists registered drivers, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return namesLocked()
}

func namesLocked() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// UnknownDriverError names the valid choices, because "no such brain" alone
// wastes a debugging session.
type UnknownDriverError struct {
	Name  string
	Known []string
}

func (e *UnknownDriverError) Error() string {
	return fmt.Sprintf("unknown brain %q (known: %v)", e.Name, e.Known)
}

// ThinkingLevels are the levels the pi-family CLIs accept.
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// ValidThinking reports whether a level is known. An empty level is valid (the
// driver default).
func ValidThinking(level string) bool {
	if level == "" {
		return true
	}
	for _, l := range ThinkingLevels {
		if l == level {
			return true
		}
	}
	return false
}

// Unregister removes a driver. Test-only helper.
func Unregister(name string) {
	mu.Lock()
	defer mu.Unlock()
	delete(registry, name)
}
