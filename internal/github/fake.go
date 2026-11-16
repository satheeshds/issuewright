package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Fake is an in-memory Client for tests: it records every call so a test can
// assert what the engine was allowed to do (a review must never push).
type Fake struct {
	Issues []Issue
	PRs    []PR

	// Calls records "op target detail" lines in order.
	Calls []string

	Labels    map[int][]string
	Comments  map[int][]string
	Reviews   map[int][]string
	Assignees map[int][]string
	Pushed    []string

	// FailOn makes the named operation return an error.
	FailOn string
}

func NewFake() *Fake {
	return &Fake{
		Labels:    map[int][]string{},
		Comments:  map[int][]string{},
		Reviews:   map[int][]string{},
		Assignees: map[int][]string{},
	}
}

func (f *Fake) record(format string, args ...any) error {
	line := fmt.Sprintf(format, args...)
	f.Calls = append(f.Calls, line)
	if f.FailOn != "" && strings.HasPrefix(line, f.FailOn) {
		return fmt.Errorf("fake failure: %s", line)
	}
	return nil
}

// CallsMatching returns recorded calls with the given prefix.
func (f *Fake) CallsMatching(prefix string) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) ListAssignedIssues(_ context.Context, slug, login string) ([]Issue, error) {
	if err := f.record("list-assigned %s %s", slug, login); err != nil {
		return nil, err
	}
	var out []Issue
	for _, i := range f.Issues {
		for _, a := range i.Assignees {
			if a == login {
				out = append(out, i)
			}
		}
	}
	return out, nil
}

func (f *Fake) GetIssue(_ context.Context, _ string, number int) (Issue, error) {
	if err := f.record("get-issue %d", number); err != nil {
		return Issue{}, err
	}
	for _, i := range f.Issues {
		if i.Number == number {
			return i, nil
		}
	}
	return Issue{}, ErrNotFound
}

func (f *Fake) ListReviewRequestedPRs(_ context.Context, slug, login string) ([]PR, error) {
	if err := f.record("list-reviews %s %s", slug, login); err != nil {
		return nil, err
	}
	return f.PRs, nil
}

func (f *Fake) GetPR(_ context.Context, _ string, number int) (PR, error) {
	if err := f.record("get-pr %d", number); err != nil {
		return PR{}, err
	}
	for _, p := range f.PRs {
		if p.Number == number {
			return p, nil
		}
	}
	return PR{}, ErrNotFound
}

func (f *Fake) FindPRByHead(_ context.Context, _ string, head string) (PR, bool, error) {
	if err := f.record("find-pr %s", head); err != nil {
		return PR{}, false, err
	}
	for _, p := range f.PRs {
		if p.HeadRef == head {
			return p, true, nil
		}
	}
	return PR{}, false, nil
}

func (f *Fake) CreateDraftPR(_ context.Context, _ string, head, base, title, body string) (PR, error) {
	if err := f.record("create-pr %s %s %q", head, base, title); err != nil {
		return PR{}, err
	}
	n := 100 + len(f.PRs)
	pr := PR{Number: n, Title: title, Body: body, HeadRef: head, BaseRef: base, Draft: true,
		URL: "https://example.test/pr/" + strconv.Itoa(n)}
	f.PRs = append(f.PRs, pr)
	return pr, nil
}

func (f *Fake) UpdatePRBody(_ context.Context, _ string, number int, body string) error {
	if err := f.record("update-pr %d", number); err != nil {
		return err
	}
	for i := range f.PRs {
		if f.PRs[i].Number == number {
			f.PRs[i].Body = body
		}
	}
	return nil
}

func (f *Fake) ReviewPR(_ context.Context, _ string, number int, body string) error {
	if err := f.record("review %d", number); err != nil {
		return err
	}
	f.Reviews[number] = append(f.Reviews[number], body)
	return nil
}

func (f *Fake) Comment(_ context.Context, _ string, number int, body string) error {
	if err := f.record("comment %d", number); err != nil {
		return err
	}
	f.Comments[number] = append(f.Comments[number], body)
	return nil
}

func (f *Fake) AddLabel(_ context.Context, _ string, number int, label string) error {
	if err := f.record("add-label %d %s", number, label); err != nil {
		return err
	}
	f.Labels[number] = append(f.Labels[number], label)
	return nil
}

func (f *Fake) RemoveLabel(_ context.Context, _ string, number int, label string) error {
	if err := f.record("remove-label %d %s", number, label); err != nil {
		return err
	}
	kept := f.Labels[number][:0]
	for _, l := range f.Labels[number] {
		if l != label {
			kept = append(kept, l)
		}
	}
	f.Labels[number] = kept
	return nil
}

func (f *Fake) EnsureLabel(_ context.Context, _, label string) error {
	return f.record("ensure-label %s", label)
}

func (f *Fake) Unassign(_ context.Context, _ string, number int, login string) error {
	if err := f.record("unassign %d %s", number, login); err != nil {
		return err
	}
	kept := f.Assignees[number][:0]
	for _, a := range f.Assignees[number] {
		if a != login {
			kept = append(kept, a)
		}
	}
	f.Assignees[number] = kept
	return nil
}

func (f *Fake) PushBranch(_ context.Context, _, branch string) error {
	if err := f.record("push %s", branch); err != nil {
		return err
	}
	f.Pushed = append(f.Pushed, branch)
	return nil
}
