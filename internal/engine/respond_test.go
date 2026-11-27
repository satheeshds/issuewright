package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/prompts"
)

// prWithHead prepares a pull request whose head is fetchable as pull/N/head, and
// returns the worktree directory.
func prWithHead(t *testing.T, h *harness, number int, branch string) {
	t.Helper()
	h.gh.PRs = append(h.gh.PRs, github.PR{
		Number: number, Title: "work", HeadRef: branch, BaseRef: "main", HeadRefOID: "sha",
	})
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-"+itoa(number))
	gitRun(t, h.cloneDir, "worktree", "add", "-q", "-b", branch, dir, "origin/main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: work")
	gitRun(t, dir, "push", "-q", "origin", branch)
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/"+itoa(number)+"/head", sha)
	h.gh.PRs[len(h.gh.PRs)-1].HeadRefOID = sha
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// answerScript makes the stub write a replies payload for the given comment ids.
func answerScript(ids ...int) string {
	var b strings.Builder
	b.WriteString("---\nreplies:\n")
	for _, id := range ids {
		b.WriteString("  - in_reply_to: " + itoa(id) + "\n    body: agreed, fixed\n")
	}
	b.WriteString("---\n## Summary\nanswered\n")
	return b.String()
}

func TestRespondAnswersAMentionOnItsOwnPR(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 31, "agent/issue-31")
	h.gh.Conversation[31] = []github.Comment{
		{ID: 501, Author: "satheeshds", Body: "@demo-bot[bot] or @demo-bot why is this here?"},
	}

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: answerScript(501)})
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Replies) != 1 || h.gh.Replies[0].To != 501 {
		t.Fatalf("replies = %+v\n%s", h.gh.Replies, logs)
	}
	if !strings.Contains(h.gh.Replies[0].Body, "agreed, fixed") {
		t.Errorf("reply body = %q", h.gh.Replies[0].Body)
	}
	if len(h.gh.Reactions) != 1 || !strings.HasSuffix(h.gh.Reactions[0], ":eyes") {
		t.Errorf("expected an eyes reaction, got %v", h.gh.Reactions)
	}
	if results[len(results)-1].Workflow != "respond" || results[len(results)-1].State != StateDone {
		t.Errorf("results = %+v", results)
	}

	// Second tick: the comment is answered, so nothing happens.
	h.gh.Replies = nil
	beforeCalls := len(h.gh.CallsMatching("reply"))
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.gh.CallsMatching("reply")); n != beforeCalls {
		t.Errorf("the same comment was answered twice (%d replies)", n-1)
	}
}

func TestRespondAnswersRepliesInItsOwnThreadAndResolvesIt(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 32, "agent/issue-32")
	h.gh.Threads[32] = []github.CommentThread{{
		ID: "THREAD_32",
		Comments: []github.Comment{
			{ID: 601, Author: "demo-bot[bot]", Body: "blocking: this leaks", IsReview: true, Path: "f.txt", Line: 1},
			{ID: 602, Author: "satheeshds", Body: "no it does not, check line 3", IsReview: true, Path: "f.txt", Line: 1},
		},
	}}

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: answerScript(602)})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Replies) != 1 || h.gh.Replies[0].To != 602 || !h.gh.Replies[0].IsReview {
		t.Fatalf("replies = %+v", h.gh.Replies)
	}
	if len(h.gh.ResolvedThreads) != 1 || h.gh.ResolvedThreads[0] != "THREAD_32" {
		t.Errorf("the bot's own thread should be resolved once answered: %v", h.gh.ResolvedThreads)
	}
}

func TestRespondLeavesHumanThreadsAlone(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 33, "agent/issue-33")
	// A human-only thread, no mention: not ours to answer.
	h.gh.Threads[33] = []github.CommentThread{{
		ID: "THREAD_33",
		Comments: []github.Comment{
			{ID: 701, Author: "satheeshds", Body: "I wonder about this", IsReview: true, Path: "f.txt", Line: 1},
			{ID: 702, Author: "someone-else", Body: "me too", IsReview: true, Path: "f.txt", Line: 1},
		},
	}}

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: answerScript(701)})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Replies) != 0 {
		t.Errorf("answered a thread it was not asked about: %+v", h.gh.Replies)
	}
	if len(h.gh.ResolvedThreads) != 0 {
		t.Errorf("resolved a human's thread: %v", h.gh.ResolvedThreads)
	}
}

func TestRespondNeverAnswersItself(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 34, "agent/issue-34")
	h.gh.Conversation[34] = []github.Comment{
		{ID: 801, Author: "demo-bot[bot]", Body: "@demo-bot here is my own note"},
		{ID: 802, Author: "renovate[bot]", Body: "@demo-bot bump?"},
	}

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: answerScript(801, 802)})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Replies) != 0 {
		t.Errorf("replied to itself or another bot: %+v", h.gh.Replies)
	}
}

// A reply naming a comment this run was not given is dropped, not guessed.
func TestRespondDropsRepliesForUnknownComments(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 35, "agent/issue-35")
	h.gh.Conversation[35] = []github.Comment{
		{ID: 901, Author: "satheeshds", Body: "@demo-bot what about this?"},
	}
	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: answerScript(901, 999)})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Replies) != 1 || h.gh.Replies[0].To != 901 {
		t.Errorf("replies = %+v (unknown id must be dropped)", h.gh.Replies)
	}
	if !strings.Contains(logs.String(), "unknown comment ids") {
		t.Errorf("the dropped id should be reported:\n%s", logs.String())
	}
}

// Code changes are pushed only to a branch this runner created.
func TestRespondFixesOnlyItsOwnBranch(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 36, "agent/issue-36")
	h.gh.Conversation[36] = []github.Comment{
		{ID: 1001, Author: "satheeshds", Body: "@demo-bot[bot] please fix the typo"},
	}
	reply := "---\nreplies:\n  - in_reply_to: 1001\n    body: fixed\nchanges:\n  - path: f.txt\n    summary: typo\n---\n## Summary\nfixed\n"

	e, logs := h.engine(t, &stubDriver{file: prompts.SummaryFile, content: reply, editFile: "f.txt", editContent: "fixed\n"})
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Pushed) != 1 || h.gh.Pushed[0] != "agent/issue-36" {
		t.Errorf("push = %v", h.gh.Pushed)
	}

	// A human's branch: the same request must not push.
	h2 := newHarness(t)
	prWithHead(t, h2, 37, "feature/human-work")
	// It counts as touched because this runner reviewed it.
	h2.gh.PRs[len(h2.gh.PRs)-1].Labels = []string{"review:done"}
	h2.gh.Conversation[37] = []github.Comment{
		{ID: 1101, Author: "satheeshds", Body: "@demo-bot[bot] please fix the typo"},
	}
	reply2 := strings.ReplaceAll(reply, "1001", "1101")
	e2, logs2 := h2.engine(t, &stubDriver{file: prompts.SummaryFile, content: reply2, editFile: "f.txt", editContent: "fixed\n"})
	if _, err := e2.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs2)
	}
	if len(h2.gh.Pushed) != 0 {
		t.Errorf("pushed to a branch it does not own: %v", h2.gh.Pushed)
	}
	// The work is not lost: the patch goes to the human.
	body := strings.Join(h2.gh.Comments[37], "\n")
	if !strings.Contains(body, "not one I created") || !strings.Contains(body, "```diff") {
		t.Errorf("the patch should be posted for a branch we do not own:\n%s", body)
	}
	if !strings.Contains(logs2.String(), "patch posted") {
		t.Errorf("the run should say what it did instead:\n%s", logs2.String())
	}
}

// The incident: the agent made the commit itself, so there was nothing left for the
// runner to commit, and the push was skipped — while the reply had already gone out
// naming that commit. The reply must wait for the push, and the push must happen.
func TestRespondPushesACommitTheAgentMadeItself(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 61, "agent/issue-61")
	h.gh.Conversation[61] = []github.Comment{
		{ID: 901, Author: "satheeshds", Body: "@demo-bot fix the fanout level"},
	}
	drv := &stubDriver{file: prompts.SummaryFile,
		content: "---\nreplies:\n  - in_reply_to: 901\n    body: Wrapped it. Fixed in HEAD.\n---\n## Summary\nfixed\n",
		// The agent commits in the worktree, like the daily-fish run did.
		script: `echo fix > fixed.txt && git add -A && git commit -qm "fix: gate the fanout"`}

	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	headPush := h.gh.CallsMatching("push-head agent/issue-61")
	if len(headPush) == 0 {
		t.Fatalf("HEAD was never pushed (this is the incident): %v", h.gh.Calls)
	}
	if len(h.gh.Replies) != 1 || h.gh.Replies[0].To != 901 {
		t.Fatalf("replies = %+v", h.gh.Replies)
	}
	pushAt, replyAt := -1, -1
	for i, c := range h.gh.Calls {
		switch {
		case strings.HasPrefix(c, "push-head"):
			pushAt = i
		case strings.HasPrefix(c, "reply 61 -> 901"):
			replyAt = i
		}
	}
	if pushAt < 0 || replyAt < 0 || pushAt > replyAt {
		t.Errorf("the reply must come after the push: push@%d reply@%d", pushAt, replyAt)
	}
}

// The other half of the incident: a reply that names a commit nobody can find.
func TestRespondRefusesAnInventedCommit(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 62, "agent/issue-62")
	h.gh.Conversation[62] = []github.Comment{
		{ID: 902, Author: "satheeshds", Body: "@demo-bot and this?"},
	}
	drv := &stubDriver{file: prompts.SummaryFile,
		content: "---\nreplies:\n  - in_reply_to: 902\n    body: Agreed, fixed in b9ba799.\n---\n## Summary\nfixed\n"}

	e, logs := h.engine(t, drv)
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(h.gh.Replies) != 0 {
		t.Fatalf("a claim that cannot be verified was posted anyway: %+v", h.gh.Replies)
	}
	var blocked string
	for _, r := range results {
		if r.State == StateBlocked {
			blocked = r.Detail
		}
	}
	if !strings.Contains(blocked, "b9ba799") {
		t.Errorf("the block should name the bad commit, got %q\n%s", blocked, logs)
	}
}

// A review worktree with unpublished work must survive the tick that would have
// deleted it, and must not be reopened for a review on top of it.
func TestReviewWorktreeWithLocalWorkIsNotDeleted(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 63, "feature/thing")
	h.gh.ReviewRequests = []int{63}
	drv := &stubDriver{file: prompts.ReviewFile, content: "## Summary\nfine\n"}

	e, logs := h.engine(t, drv)
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v\n%s", err, logs)
	}
	dir := filepath.Join(h.agent.Workroot, "demo", "review-63")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the review worktree should still exist: %v", err)
	}
	// Something unpublished appears in it, as an agent-committed fix did on the box.
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "fix: unpublished")

	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v\n%s", err, logs)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the worktree holding unpublished work was deleted: %v\n%s", err, logs)
	}
	if _, err := os.Stat(filepath.Join(dir, "fix.txt")); err != nil {
		t.Fatalf("the unpublished commit is no longer checked out: %v", err)
	}
	// And the review refuses to run on top of it, loudly.
	var blocked string
	for _, r := range results {
		if r.State == StateBlocked {
			blocked += r.Detail + "\n"
		}
	}
	if !strings.Contains(blocked, "unpublished work") {
		t.Errorf("the review should refuse to reset a worktree with unpublished work, got %q\n%s", blocked, logs)
	}
}
