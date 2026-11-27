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

	Labels         map[int][]string
	Comments       map[int][]string
	Reviews        map[int][]string
	InlineComments map[int][]ReviewComment
	Assignees      map[int][]string
	Pushed         []string

	// Threads and Conversation are what the fake reports for a PR's comments.
	Threads      map[int][]CommentThread
	Conversation map[int][]Comment
	// IssueComments is what the fake reports for ListComments, on issues and pull
	// requests alike (GitHub serves both from the issues endpoint).
	IssueComments map[int][]Comment
	// ReviewStates reports a PR's most recent human review state by number, e.g.
	// "CHANGES_REQUESTED". Empty means no review yet.
	ReviewStates map[int]string
	Replies      []FakeReply
	Reactions    []string
	// ResolvedThreads records thread ids the runner resolved.
	ResolvedThreads []string

	// FailOn makes the named operation return an error.
	FailOn string
	// ReviewRequests lists the PR numbers GitHub would report as
	// "review-requested:<login>". Empty by default because GitHub refuses to
	// request review from most App bots — a Fake that returned everything hid a
	// bug where the label path never ran.
	ReviewRequests []int
}

func NewFake() *Fake {
	return &Fake{
		Labels:         map[int][]string{},
		Comments:       map[int][]string{},
		Reviews:        map[int][]string{},
		InlineComments: map[int][]ReviewComment{},
		Assignees:      map[int][]string{},
		Threads:        map[int][]CommentThread{},
		Conversation:   map[int][]Comment{},
		IssueComments:  map[int][]Comment{},
		ReviewStates:   map[int]string{},
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
		if !isOpen(i.State) {
			continue
		}
		for _, a := range i.Assignees {
			if a == login {
				out = append(out, i)
			}
		}
	}
	return out, nil
}

func (f *Fake) ListIssuesWithLabel(_ context.Context, slug, label string) ([]Issue, error) {
	if err := f.record("list-labeled %s %s", slug, label); err != nil {
		return nil, err
	}
	var out []Issue
	for _, i := range f.Issues {
		if !isOpen(i.State) {
			continue
		}
		for _, l := range i.Labels {
			if l == label {
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
	var out []PR
	for _, pr := range f.PRs {
		if !isOpen(pr.State) {
			continue
		}
		for _, n := range f.ReviewRequests {
			if pr.Number == n {
				out = append(out, pr)
			}
		}
	}
	return out, nil
}

func (f *Fake) ListPRsWithLabel(_ context.Context, slug, label string) ([]PR, error) {
	if err := f.record("list-labeled-prs %s %s", slug, label); err != nil {
		return nil, err
	}
	var out []PR
	for _, pr := range f.PRs {
		if !isOpen(pr.State) {
			continue
		}
		for _, l := range pr.Labels {
			if l == label {
				out = append(out, pr)
			}
		}
	}
	return out, nil
}

func (f *Fake) ListOpenPRs(_ context.Context, slug string) ([]PR, error) {
	if err := f.record("list-open-prs %s", slug); err != nil {
		return nil, err
	}
	var out []PR
	for _, pr := range f.PRs {
		if isOpen(pr.State) {
			out = append(out, pr)
		}
	}
	return out, nil
}

// isOpen treats an empty state as open, because most fixtures do not set one.
func isOpen(state string) bool {
	return state == "" || strings.EqualFold(state, "OPEN")
}

func (f *Fake) ListCommentThreads(_ context.Context, slug string, pr int) ([]CommentThread, []Comment, error) {
	if err := f.record("list-comments %s#%d", slug, pr); err != nil {
		return nil, nil, err
	}
	return f.Threads[pr], f.Conversation[pr], nil
}

func (f *Fake) ListComments(_ context.Context, slug string, number, afterID int) ([]Comment, error) {
	if err := f.record("list-issue-comments %s#%d after=%d", slug, number, afterID); err != nil {
		return nil, err
	}
	var out []Comment
	for _, c := range f.IssueComments[number] {
		if c.ID > afterID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *Fake) ReplyToComment(_ context.Context, _ string, pr int, to Comment, body string) error {
	if err := f.record("reply %d -> %d", pr, to.ID); err != nil {
		return err
	}
	f.Replies = append(f.Replies, FakeReply{PR: pr, To: to.ID, IsReview: to.IsReview, Body: body})
	return nil
}

func (f *Fake) ReactToComment(_ context.Context, _ string, pr int, to Comment, content string) error {
	if err := f.record("react %d -> %d %s", pr, to.ID, content); err != nil {
		return err
	}
	f.Reactions = append(f.Reactions, fmt.Sprintf("%d:%s", to.ID, content))
	return nil
}

func (f *Fake) ResolveThread(_ context.Context, threadID string) error {
	if err := f.record("resolve-thread %s", threadID); err != nil {
		return err
	}
	f.ResolvedThreads = append(f.ResolvedThreads, threadID)
	return nil
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

func (f *Fake) CreateReview(_ context.Context, _ string, number int, body string, comments []ReviewComment) error {
	if err := f.record("review-inline %d %d-comments", number, len(comments)); err != nil {
		return err
	}
	f.Reviews[number] = append(f.Reviews[number], body)
	f.InlineComments[number] = append(f.InlineComments[number], comments...)
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

// FakeReply is one reply the engine posted.
type FakeReply struct {
	PR       int
	To       int
	IsReview bool
	Body     string
}
