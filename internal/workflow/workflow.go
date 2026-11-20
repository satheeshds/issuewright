// Package workflow is the second seam: what a run is allowed to do, and which
// events may start it.
//
// This is where "a review runner is a plugin" comes from. The difference between
// implementing an issue and reviewing a PR is not which agent thinks — it is that
// a reviewer must never write. That is expressed as Perms and enforced by the
// engine, not asked for in a prompt.
package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/satheeshds/issuewright/internal/brain"
)

// Kind is an event type the runner can observe.
type Kind string

// The events issuewright polls for.
const (
	IssueAssigned Kind = "issue_assigned"
	IssueLabel    Kind = "issue_label"
	PROpened      Kind = "pr_opened"
	// ReviewComment starts work from a comment on a pull request: a mention of the
	// bot, or a reply in a thread the bot opened.
	ReviewComment Kind = "review_comment"
	// PRLabel starts work from a label on the pull request. On repos where
	// GitHub refuses to assign or request review from the App bot, this is the
	// only trigger that can be automated.
	PRLabel        Kind = "pr_label"
	CommentMention Kind = "comment_mention"
)

// Trigger declares that a workflow accepts an event. An empty Label on
// IssueLabel means "the profile's force_label".
type Trigger struct {
	Kind  Kind
	Label string
}

// Perms is the safety arm. The engine refuses actions a workflow did not claim;
// a review workflow that hallucinates "LGTM, pushed a fix" produces a comment
// and a non-zero exit, never a branch.
type Perms struct {
	Write   bool // may push a branch and open/update a PR
	Comment bool // may post issue/PR comments and reviews
	Label   bool // may change labels and assignees
	Checks  bool // may run the repo's verification commands
}

// Protect mirrors the profile's hard rules, rendered into the prompt and
// enforced after the run.
type Protect struct {
	WriteDenylist []string
	NeverRun      []string
	SecretPaths   []string
}

// JobComment is one comment a run is asked to answer.
type JobComment struct {
	ID     int
	Author string
	Body   string
	Path   string
	Line   int
	// IsReview marks an inline review comment (replied to on its thread).
	IsReview bool
}

// Job is everything a workflow needs to build a prompt. It carries no GitHub
// client and no git handle: a workflow shapes a run, it does not perform one.
type Job struct {
	Kind       Kind
	Repo       string
	Number     int
	Title      string
	Body       string
	Labels     []string
	Branch     string
	BaseBranch string
	Workdir    string
	// Rules is the repo's rendered preamble (its .agent/rules.md).
	Rules string
	// Protect is the profile's hard rules, already merged.
	Protect Protect
	// ExistingWork summarises commits already on the branch, for drivers that
	// cannot resume a session.
	ExistingWork string
	// Notes are the profile's PR-body notes.
	Notes []string
	// DryRun tells the workflow not to build a prompt that assumes publishing.
	DryRun bool
	// Resume means this branch already has work: ask for a continuation rather
	// than a fresh start.
	Resume bool
	// Comments are the review comments this run answers.
	Comments []JobComment
	// OwnBranch is true when the PR head branch is one this runner created, so a
	// fix may be pushed to it. Never true for someone else's branch.
	OwnBranch bool
}

// Workflow is one kind of run.
type Workflow interface {
	Name() string
	Triggers() []Trigger
	Perms() Perms
	// Prompt renders the run: the embedded template plus the repo's rules.
	Prompt(job Job) (brain.Spec, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Workflow{}
)

// Register adds a workflow. Duplicate names panic, as with database/sql.
func Register(w Workflow) {
	if w == nil || w.Name() == "" {
		panic("workflow: Register with nil or unnamed workflow")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[w.Name()]; dup {
		panic("workflow: duplicate " + w.Name())
	}
	registry[w.Name()] = w
}

// Get returns a workflow by name.
func Get(name string) (Workflow, error) {
	mu.RLock()
	defer mu.RUnlock()
	if w, ok := registry[name]; ok {
		return w, nil
	}
	return nil, &UnknownWorkflowError{Name: name, Known: namesLocked()}
}

// Names lists registered workflows, sorted.
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

// UnknownWorkflowError names the valid choices.
type UnknownWorkflowError struct {
	Name  string
	Known []string
}

func (e *UnknownWorkflowError) Error() string {
	return fmt.Sprintf("unknown workflow %q (known: %v)", e.Name, e.Known)
}

// Unregister removes a workflow. Test-only helper.
func Unregister(name string) {
	mu.Lock()
	defer mu.Unlock()
	delete(registry, name)
}

// LabelDefaults supply the label a trigger means when it declares none: the
// profile chooses the names, the workflow only says "a label starts me".
type LabelDefaults struct {
	Issue string
	PR    string
}

// Event is a discovered thing that might start work.
type Event struct {
	Kind   Kind
	Number int
	Title  string
	Labels []string
}

// Accepts reports whether a workflow wants this event. A trigger that names no
// label takes the profile's default for that kind.
func Accepts(w Workflow, ev Event, defaults LabelDefaults) bool {
	for _, t := range w.Triggers() {
		if t.Kind != ev.Kind {
			continue
		}
		switch ev.Kind {
		case IssueLabel, PRLabel:
			want := t.Label
			if want == "" {
				if ev.Kind == PRLabel {
					want = defaults.PR
				} else {
					want = defaults.Issue
				}
			}
			for _, l := range ev.Labels {
				if strings.EqualFold(l, want) {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// Selection is one workflow paired with the event that starts it.
type Selection struct {
	Workflow Workflow
	Event    Event
}

// Plan pairs events with enabled workflows. Read-only work is scheduled before
// writing work so a short review is not stuck behind a long implement run.
func Plan(events []Event, enabled func(Workflow) bool, defaults LabelDefaults) []Selection {
	sorted := make([]Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		pi, pj := priority(sorted[i].Kind), priority(sorted[j].Kind)
		if pi != pj {
			return pi < pj
		}
		return sorted[i].Number < sorted[j].Number
	})

	seen := map[string]bool{}
	var out []Selection
	for _, ev := range sorted {
		for _, name := range Names() {
			w, err := Get(name)
			if err != nil {
				continue
			}
			if enabled != nil && !enabled(w) {
				continue
			}
			if !Accepts(w, ev, defaults) {
				continue
			}
			key := fmt.Sprintf("%s#%d/%s", w.Name(), ev.Number, ev.Kind)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Selection{Workflow: w, Event: ev})
		}
	}
	return out
}

func priority(k Kind) int {
	switch k {
	case PROpened, PRLabel, CommentMention, ReviewComment:
		return 0
	case IssueAssigned, IssueLabel:
		return 1
	}
	return 2
}

// ErrNoJob means a workflow was asked to run without a job.
var ErrNoJob = errors.New("workflow: empty job")
