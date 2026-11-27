package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/state"
	"github.com/satheeshds/issuewright/prompts"
)

// conversationDriver is a brain that can name a conversation, records every spec
// it was handed, and writes what each turn "produced".
type conversationDriver struct {
	caps  brain.Caps
	calls []brain.Spec
	// steps are consumed one per run: the files the agent leaves behind and the
	// text it reports. The last step repeats.
	steps []driverStep
}

type driverStep struct {
	files   map[string]string
	summary string
}

func newConversationDriver(steps ...driverStep) *conversationDriver {
	return &conversationDriver{
		caps:  brain.Caps{Resume: true, Conversation: true, Model: true, Thinking: true},
		steps: steps,
	}
}

func (d *conversationDriver) Name() string     { return "stub" }
func (d *conversationDriver) Caps() brain.Caps { return d.caps }

func (d *conversationDriver) Run(_ context.Context, spec brain.Spec, log io.Writer) (brain.Result, error) {
	d.calls = append(d.calls, spec)
	step := d.steps[len(d.steps)-1]
	if len(d.calls) <= len(d.steps) {
		step = d.steps[len(d.calls)-1]
	}
	// A change to a source file, so the branch differs from the base and the
	// checks have something to select.
	if err := os.WriteFile(filepath.Join(spec.Workdir, "change.txt"),
		[]byte(fmt.Sprintf("turn %d\n", len(d.calls))), 0o644); err != nil {
		return brain.Result{}, err
	}
	for name, content := range step.files {
		if err := os.WriteFile(filepath.Join(spec.Workdir, name), []byte(content), 0o644); err != nil {
			return brain.Result{}, err
		}
	}
	text := step.summary
	if text == "" {
		text = "done"
	}
	io.WriteString(log, `{"type":"message_end","message":{"content":[{"type":"text","text":"`+text+`"}]}}`+"\n")
	return brain.Result{Driver: "stub", Summary: text, CostUSD: 0.5}, nil
}

func summaryStep(body string) driverStep {
	return driverStep{
		files:   map[string]string{prompts.SummaryFile: "## What changed\n- " + body + "\n"},
		summary: body,
	}
}

// syncLabels copies the labels the engine wrote back into the issue fixtures,
// which is what the production gh client reports on the next tick. The fake does
// not do it for us, by design: a test that wants a label change to be visible has
// to say so.
func (h *harness) syncLabels() {
	for i := range h.gh.Issues {
		if labels, ok := h.gh.Labels[h.gh.Issues[i].Number]; ok {
			h.gh.Issues[i].Labels = append([]string{}, labels...)
		}
	}
	for i := range h.gh.PRs {
		if labels, ok := h.gh.Labels[h.gh.PRs[i].Number]; ok {
			h.gh.PRs[i].Labels = append([]string{}, labels...)
		}
	}
}

// ticker returns a function that runs one tick and keeps the fixtures honest
// afterwards.
func (h *harness) ticker(t *testing.T, e *Engine, logs *bytes.Buffer) func() []Result {
	t.Helper()
	return func() []Result {
		res, err := e.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v\n%s", err, logs)
		}
		h.syncLabels()
		return res
	}
}

// conversation reads the state the engine wrote for a target.
func (h *harness) conversation(t *testing.T, e *Engine, workflow string, number int) state.Conversation {
	t.Helper()
	c, ok, err := e.store.LoadConversation(h.repo.Name, workflow, number)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if !ok {
		t.Fatalf("no conversation for %s#%d", workflow, number)
	}
	return c
}

func TestFirstTurnCreatesADeterministicSession(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("added metrics"))
	e, logs := h.engine(t, drv)

	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v\n%s", err, logs)
	}
	if len(drv.calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(drv.calls))
	}
	want := state.SessionIDFor("demo", "implement", 12, 0)
	if drv.calls[0].Conversation != want {
		t.Errorf("session handed to the brain = %q, want %q", drv.calls[0].Conversation, want)
	}
	conv := h.conversation(t, e, "implement", 12)
	if conv.SessionID != want || conv.Turns != 1 {
		t.Errorf("conversation = %+v", conv)
	}
	if conv.Branch != "agent/issue-12" || conv.Cwd == "" {
		t.Errorf("conversation should record where it works: %+v", conv)
	}
	if conv.CostUSD != 0.5 {
		t.Errorf("cost = %v, want the brain's reported 0.5", conv.CostUSD)
	}
	if conv.State != state.ConversationDone {
		t.Errorf("state = %q, want done", conv.State)
	}
}

func TestCommentStartsATurnOnTheSameSession(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(
		summaryStep("added metrics"),
		summaryStep("renamed the counter"),
	)
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)

	tick()
	h.gh.IssueComments[12] = []github.Comment{
		{ID: 100, Author: "human", Body: "@demo-bot please also rename the counter."},
	}

	results := tick()
	if len(drv.calls) != 2 {
		t.Fatalf("runs = %d, want 2 (one turn for the comment)\n%s", len(drv.calls), logs)
	}
	if got := drv.calls[1].Conversation; got != drv.calls[0].Conversation {
		t.Errorf("turn used session %q, want the same as the first run %q", got, drv.calls[0].Conversation)
	}
	if !strings.Contains(drv.calls[1].Prompt, "rename the counter") {
		t.Errorf("the comment is not in the turn's prompt:\n%s", drv.calls[1].Prompt)
	}
	if !strings.Contains(drv.calls[1].Prompt, "turn 2") {
		t.Errorf("the prompt should say which turn this is:\n%s", drv.calls[1].Prompt)
	}
	var turns int
	for _, r := range results {
		if r.State != StateSkipped {
			turns++
		}
	}
	if turns != 1 {
		t.Fatalf("results = %+v, want exactly one turn\n%s", results, logs)
	}
	if got := h.conversation(t, e, "implement", 12).Turns; got != 2 {
		t.Errorf("turns = %d, want 2", got)
	}

	// The same comment must not start a third turn.
	tick()
	if len(drv.calls) != 2 {
		t.Errorf("an answered comment started another turn: %d runs", len(drv.calls))
	}
}

func TestTwoCommentsAreOneTurn(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("first"), summaryStep("second"))
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	h.gh.IssueComments[12] = []github.Comment{
		{ID: 100, Author: "human", Body: "@demo-bot one thing: rename foo"},
		{ID: 101, Author: "human", Body: "@demo-bot and also add a test"},
	}
	tick()
	if len(drv.calls) != 2 {
		t.Fatalf("runs = %d, want one turn for both comments", len(drv.calls))
	}
	for _, want := range []string{"rename foo", "add a test"} {
		if !strings.Contains(drv.calls[1].Prompt, want) {
			t.Errorf("turn prompt missing %q:\n%s", want, drv.calls[1].Prompt)
		}
	}
}

func TestQuestionIsATurnNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(driverStep{
		files: map[string]string{
			prompts.QuestionFile: "Which database should the counter live in?\n",
		},
		summary: "asked",
	})
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)

	results := tick()
	if len(results) != 1 || results[0].State != StateAsking {
		t.Fatalf("results = %+v, want one asking result\n%s", results, logs)
	}
	if len(h.gh.CallsMatching("unassign")) != 0 {
		t.Errorf("a question must not unassign the bot: %v", h.gh.CallsMatching("unassign"))
	}
	if !strings.Contains(strings.Join(h.gh.Comments[12], "\n"), "database") {
		t.Errorf("the question was not posted: %v", h.gh.Comments[12])
	}
	if !hasLabel(h.gh.Labels[12], "agent:question") {
		t.Errorf("labels = %v, want agent:question", h.gh.Labels[12])
	}
	if hasLabel(h.gh.Labels[12], "agent:done") {
		t.Errorf("a question is not done: %v", h.gh.Labels[12])
	}
	conv := h.conversation(t, e, "implement", 12)
	if conv.State != state.ConversationAsking {
		t.Errorf("conversation state = %q, want asking", conv.State)
	}
	claim, _, _ := e.store.LoadClaim(h.repo.Name, "implement", 12)
	if claim.State != StateAsking {
		t.Errorf("claim state = %q, want asking", claim.State)
	}

	// Nothing new: the question must not be repeated.
	tick()
	if len(drv.calls) != 1 {
		t.Errorf("runs = %d, wanting no re-run while waiting for an answer", len(drv.calls))
	}
}

func TestAnswerContinuesTheSameAgent(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(
		driverStep{files: map[string]string{
			prompts.QuestionFile: "Which store: postgres or sqlite?\n",
		}},
		summaryStep("used postgres"),
	)
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	h.gh.IssueComments[12] = []github.Comment{
		{ID: 200, Author: "human", Body: "postgres, please"},
	}
	tick()
	if len(drv.calls) != 2 {
		t.Fatalf("runs = %d, want the answer to start a turn", len(drv.calls))
	}
	answer := drv.calls[1]
	if answer.Conversation != drv.calls[0].Conversation {
		t.Errorf("the answer ran in session %q, want %q", answer.Conversation, drv.calls[0].Conversation)
	}
	for _, want := range []string{"postgres, please", "Which store"} {
		if !strings.Contains(answer.Prompt, want) {
			t.Errorf("the turn prompt should carry %q:\n%s", want, answer.Prompt)
		}
	}
	if got := h.conversation(t, e, "implement", 12).Turns; got != 2 {
		t.Errorf("turns = %d, want 2", got)
	}
}

func TestBotCommentsNeverStartATurn(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("first"))
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()
	h.gh.IssueComments[12] = []github.Comment{
		{ID: 300, Author: "demo-bot[bot]", Body: "I pushed something"},
		{ID: 301, Author: "other[bot]", Body: "reviewing"},
	}
	tick()
	if len(drv.calls) != 1 {
		t.Errorf("a bot comment started a turn: %d runs", len(drv.calls))
	}
}

func TestRestartStartsAFreshSession(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("first"))
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	h.gh.IssueComments[12] = []github.Comment{
		{ID: 400, Author: "human", Body: "@demo-bot restart, the approach is wrong"},
	}
	tick()
	if len(drv.calls) != 1 {
		t.Fatalf("a restart must not run the old conversation: %d runs", len(drv.calls))
	}
	conv := h.conversation(t, e, "implement", 12)
	if conv.SessionID != state.SessionIDFor("demo", "implement", 12, 1) {
		t.Errorf("session after restart = %q, want the gen-1 id", conv.SessionID)
	}
	if conv.Turns != 0 || conv.State != state.ConversationWorking {
		t.Errorf("restarted conversation = %+v, want a clean working record", conv)
	}
	if !strings.Contains(strings.Join(h.gh.Comments[12], "\n"), "Fresh session") {
		t.Errorf("the restart should be announced: %v", h.gh.Comments[12])
	}
	// A fresh session runs with the new id on the next comment.
	h.gh.IssueComments[12] = append(h.gh.IssueComments[12],
		github.Comment{ID: 401, Author: "human", Body: "@demo-bot try the other approach"})
	tick()
	if len(drv.calls) != 2 {
		t.Fatalf("runs = %d, want the new turn to run", len(drv.calls))
	}
	if got := drv.calls[1].Conversation; got != conv.SessionID {
		t.Errorf("new turn used session %q, want %q", got, conv.SessionID)
	}
}

func TestMaxTurnsStopsTheConversation(t *testing.T) {
	h := newHarness(t)
	h.profile.Issue.MaxTurns = 1
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("first"))
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	h.gh.IssueComments[12] = []github.Comment{
		{ID: 500, Author: "human", Body: "@demo-bot one more thing"},
	}
	results := tick()
	if len(drv.calls) != 1 {
		t.Errorf("a conversation over its turn budget ran: %d runs", len(drv.calls))
	}
	var blocked *Result
	for i := range results {
		if results[i].State == StateBlocked {
			blocked = &results[i]
		}
	}
	if blocked == nil {
		t.Fatalf("results = %+v, want a blocked turn", results)
	}
	if !strings.Contains(blocked.Detail, "turns") {
		t.Errorf("the block should explain the budget: %q", blocked.Detail)
	}
	// The comment is consumed, so the next tick does not block again — no second
	// wall of identical comments.
	before := len(h.gh.Comments[12])
	tick()
	if len(drv.calls) != 1 {
		t.Errorf("the blocked turn repeated: %d runs", len(drv.calls))
	}
	if after := len(h.gh.Comments[12]); after != before {
		t.Errorf("the budget block commented again: %d → %d", before, after)
	}
}

func TestDryRunPlansATurnWithoutWritingState(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("first"))
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()
	h.gh.IssueComments[12] = []github.Comment{
		{ID: 600, Author: "human", Body: "@demo-bot also add a test"},
	}
	dry, dryLogs := h.engine(t, drv, func(o *Options) { o.DryRun = true })
	results := h.ticker(t, dry, dryLogs)()
	if len(drv.calls) != 1 {
		t.Errorf("a dry run must not run the brain: %d runs", len(drv.calls))
	}
	var planned int
	for _, r := range results {
		if r.State == StatePlanned && r.Workflow == "implement" {
			planned++
		}
	}
	if planned == 0 {
		t.Errorf("no planned turn reported: %+v", results)
	}
	if conv := h.conversation(t, dry, "implement", 12); conv.Seen(600) {
		t.Error("a dry run must not advance the cursor")
	}
}

func TestAuthorConversationAnswersItsOwnPR(t *testing.T) {
	h := newHarness(t)
	// The issue already had its run: the label is what a real repo would carry, and
	// what stops the fresh trigger from dispatching the issue again.
	h.addIssue(31, "Add metrics", "instrument it", "agent:done")
	prWithHead(t, h, 31, "agent/issue-31")
	// A comment on the agent's own pull request needs no mention: the PR exists to
	// be reviewed. Here the mention is present anyway, to prove the author path
	// takes priority over respond's.
	// Conversation comments on a PR are served by the same issues endpoint the
	// runner reads for turns.
	h.gh.IssueComments[31] = []github.Comment{
		{ID: 700, Author: "human", Body: "@demo-bot this loop is off by one"},
	}

	drv := newConversationDriver(summaryStep("fixed the loop"))
	e, logs := h.engine(t, drv)
	if err := e.store.SaveConversation(state.Conversation{
		Repo: h.repo.Name, Workflow: "implement", Number: 31,
		SessionID: state.SessionIDFor(h.repo.Name, "implement", 31, 0),
		Branch:    "agent/issue-31", State: state.ConversationDone, Turns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	tick := h.ticker(t, e, logs)
	results := tick()

	if len(h.gh.Replies) != 0 {
		t.Errorf("respond answered a pull request that has an author: %+v", h.gh.Replies)
	}
	if len(drv.calls) != 1 {
		t.Fatalf("runs = %d, want one author turn", len(drv.calls))
	}
	if !strings.Contains(drv.calls[0].Prompt, "off by one") {
		t.Errorf("the author turn did not see the comment:\n%s", drv.calls[0].Prompt)
	}
	if drv.calls[0].Conversation != state.SessionIDFor(h.repo.Name, "implement", 31, 0) {
		t.Errorf("the turn ran in %q, want the author's session", drv.calls[0].Conversation)
	}
	var author bool
	for _, r := range results {
		if r.Workflow == "implement" && r.State == StateDone {
			author = true
		}
	}
	if !author {
		t.Errorf("results = %+v, want an implement turn", results)
	}
	conv := h.conversation(t, e, "implement", 31)
	if !conv.Seen(700) {
		t.Error("the comment should be marked answered")
	}
}

func TestRespondStillAnswersAForeignPR(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 32, "feature/human-work")
	h.gh.PRs[len(h.gh.PRs)-1].Labels = []string{"review:done"}
	h.gh.Conversation[32] = []github.Comment{
		{ID: 800, Author: "human", Body: "@demo-bot why did you flag this?"},
	}

	drv := newConversationDriver(summaryStep("answered"))
	// respond's payload is the replies front matter, not a summary.
	drv.steps = []driverStep{{files: map[string]string{
		prompts.SummaryFile: "---\nreplies:\n  - in_reply_to: 800\n    body: because it is racy\n---\n## Summary\nanswered\n",
	}}}
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	results := tick()

	if len(h.gh.Replies) != 1 || h.gh.Replies[0].To != 800 {
		t.Fatalf("replies = %+v\n%s", h.gh.Replies, logs)
	}
	var responded bool
	for _, r := range results {
		if r.Workflow == "respond" {
			responded = true
		}
	}
	if !responded {
		t.Errorf("results = %+v, want a respond run", results)
	}
}

func TestReviewKeepsOneConversationAcrossPushes(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 41, "feature/thing")
	h.gh.ReviewRequests = []int{41}

	first := "---\ntitle: needs work\nfindings:\n  - severity: blocking\n    path: f.txt\n    line: 1\n    message: this is off by one\n---\n## Summary\nneeds a fix\n"
	drv := newConversationDriver(
		driverStep{files: map[string]string{prompts.ReviewFile: first}, summary: "reviewed"},
		driverStep{files: map[string]string{prompts.ReviewFile: "---\ntitle: fixed\nfindings: []\n---\n## Summary\nresolved\n"}},
	)
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	conv := h.conversation(t, e, "review", 41)
	if conv.SessionID != state.SessionIDFor("demo", "review", 41, 0) {
		t.Errorf("review session = %q", conv.SessionID)
	}
	if conv.ReviewedSHA == "" {
		t.Error("the reviewed revision should be recorded")
	}
	if !strings.Contains(conv.LastReview, "off by one") {
		t.Errorf("the review should be remembered for the next push: %q", conv.LastReview)
	}

	// A new push is a new turn in the same conversation, with its own review in
	// front of it.
	pushAnotherCommit(t, h, 41, "f.txt", "y\n")
	h.gh.PRs[0].HeadRefOID = pushAnotherCommit(t, h, 41, "g.txt", "z\n")
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v\n%s", err, logs)
	}
	if len(drv.calls) != 2 {
		t.Fatalf("runs = %d, want the new revision reviewed\n%s", len(drv.calls), logs)
	}
	second := drv.calls[1]
	if second.Conversation != drv.calls[0].Conversation {
		t.Errorf("re-review used session %q, want %q", second.Conversation, drv.calls[0].Conversation)
	}
	for _, want := range []string{"off by one", "Previous findings", "turn 2"} {
		if !strings.Contains(second.Prompt, want) {
			t.Errorf("the re-review prompt should carry %q:\n%s", want, second.Prompt)
		}
	}
}

// pushAnotherCommit advances the PR's head branch and republishes it as the PR
// head, returning the new sha.
func pushAnotherCommit(t *testing.T, h *harness, number int, name, content string) string {
	t.Helper()
	dir := filepath.Join(h.agent.Workroot, "demo", "issue-"+itoa(number))
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "feat: more")
	gitRun(t, dir, "push", "-q", "origin", "HEAD")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, h.bare, "update-ref", "refs/pull/"+itoa(number)+"/head", sha)
	return sha
}

func TestClosedIssueArchivesTheConversation(t *testing.T) {
	h := newHarness(t)
	h.addIssue(14, "Add metrics", "instrument the API")
	drv := newConversationDriver(summaryStep("first"))
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()
	if conv := h.conversation(t, e, "implement", 14); conv.Turns != 1 {
		t.Fatalf("conversation = %+v", conv)
	}

	// The issue closes: the record leaves the live set but stays on disk.
	h.gh.Issues[0].State = "CLOSED"
	h.gh.Issues[0].Assignees = nil
	h.syncLabels()
	tick()

	if _, ok, _ := e.store.LoadConversation(h.repo.Name, "implement", 14); ok {
		t.Error("a closed issue's conversation should be archived")
	}
	live, err := e.store.Conversations(h.repo.Name)
	if err != nil || len(live) != 0 {
		t.Errorf("live conversations = %v, %v", live, err)
	}
	raw, err := os.ReadFile(filepath.Join(e.store.RepoDir(h.repo.Name), "conversations", "archive", "implement-14.json"))
	if err != nil {
		t.Fatalf("the archive should keep the record: %v", err)
	}
	if !strings.Contains(string(raw), "archived") {
		t.Errorf("archived record = %s", raw)
	}
}

func TestStatusShowsConversationTurnsAndCost(t *testing.T) {
	h := newHarness(t)
	h.addIssue(12, "Add metrics", "instrument the API")
	drv := newConversationDriver(driverStep{files: map[string]string{
		prompts.QuestionFile: "postgres or sqlite?\n",
	}})
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	rows, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Workflow == "implement" && r.Number == 12 {
			found = true
			if r.Turns != 1 {
				t.Errorf("turns = %d, want 1", r.Turns)
			}
			if r.CostUSD != 0.5 {
				t.Errorf("cost = %v, want 0.5", r.CostUSD)
			}
			if !strings.HasPrefix(r.Session, "iw-demo-implement-12") {
				t.Errorf("session = %q", r.Session)
			}
			if r.State != StateAsking {
				t.Errorf("state = %q, want asking", r.State)
			}
			if !strings.Contains(r.Detail, "postgres") {
				t.Errorf("detail should quote the question: %q", r.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("status has no row for the conversation: %+v", rows)
	}
}

func TestTurnFromAReviewThreadSkipsResolvedOnes(t *testing.T) {
	h := newHarness(t)
	h.addIssue(33, "Add metrics", "instrument it", "agent:done")
	prWithHead(t, h, 33, "agent/issue-33")
	h.gh.Threads[33] = []github.CommentThread{
		{ID: "T1", IsResolved: true, Comments: []github.Comment{
			{ID: 901, Author: "demo-bot[bot]", Body: "this is racy"},
			{ID: 902, Author: "human", Body: "you are wrong", IsReview: true},
		}},
		{ID: "T2", Comments: []github.Comment{
			{ID: 903, Author: "demo-bot[bot]", Body: "this is slow", IsReview: true},
			{ID: 904, Author: "human", Body: "explain", IsReview: true},
		}},
	}
	drv := newConversationDriver(summaryStep("answered"))
	e, logs := h.engine(t, drv)
	if err := e.store.SaveConversation(state.Conversation{
		Repo: h.repo.Name, Workflow: "implement", Number: 33,
		SessionID: state.SessionIDFor(h.repo.Name, "implement", 33, 0),
		Branch:    "agent/issue-33", State: state.ConversationDone, Turns: 1,
	}); err != nil {
		t.Fatal(err)
	}
	tick := h.ticker(t, e, logs)
	tick()

	if len(drv.calls) != 1 {
		t.Fatalf("runs = %d, want one turn for the open thread only", len(drv.calls))
	}
	if !strings.Contains(drv.calls[0].Prompt, "explain") {
		t.Errorf("the open thread's reply is not in the prompt:\n%s", drv.calls[0].Prompt)
	}
	if strings.Contains(drv.calls[0].Prompt, "you are wrong") {
		t.Error("a resolved thread's reply started a turn")
	}
}

func TestReviewQuestionIsACommentNotAReview(t *testing.T) {
	h := newHarness(t)
	prWithHead(t, h, 44, "feature/thing")
	h.gh.ReviewRequests = []int{44}
	drv := newConversationDriver(driverStep{files: map[string]string{
		prompts.QuestionFile: "Does the new endpoint replace the old one?\n",
	}})
	e, logs := h.engine(t, drv)
	tick := h.ticker(t, e, logs)
	tick()

	if reviews := len(h.gh.Reviews[44]); reviews != 0 {
		t.Errorf("a question was posted as a review (%d): a review cannot be edited", reviews)
	}
	if !strings.Contains(strings.Join(h.gh.Comments[44], "\n"), "replace the old one") {
		t.Errorf("the question was not a comment: %v", h.gh.Comments[44])
	}
	if !hasLabel(h.gh.Labels[44], "agent:question") {
		t.Errorf("labels = %v, want agent:question", h.gh.Labels[44])
	}
	if hasLabel(h.gh.Labels[44], "review:in-progress") {
		t.Errorf("the review in-progress label should be retired: %v", h.gh.Labels[44])
	}
	if conv := h.conversation(t, e, "review", 44); conv.State != state.ConversationAsking {
		t.Errorf("review conversation state = %q", conv.State)
	}
}
