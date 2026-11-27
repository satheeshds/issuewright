// Turns: a human comment on an issue or pull request starts the next turn of the
// same conversation. This file is the rule for *which* comments do that, and the
// loop that runs them.
package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/satheeshds/issuewright/internal/config"
	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/state"
	"github.com/satheeshds/issuewright/internal/workflow"
)

// handles are the strings that ask the bot for an answer, most specific first.
func (e *Engine) handles() []string {
	return config.MentionHandles(e.opt.Repo.Bot,
		e.opt.Agent.Mentions, e.opt.Repo.Mentions, e.prof.Issue.Mentions)
}

// turnStart reports whether a comment takes a turn on an existing conversation.
//
// The rules, in order of how much they mean:
//
//   - a comment by the bot or by any other bot is never a turn (reply loops);
//   - a comment already answered is never a turn again (the cursor is the
//     exactly-once guarantee, keyed by comment id);
//   - while the conversation is *asking*, any human comment is the answer — the
//     agent asked a question there, so a human speaking is the reply;
//   - a blocked conversation waits for a human to speak again, which is how
//     "re-assign to retry" becomes "reply to retry";
//   - a comment on the pull request this agent opened is feedback on its own
//     work, so it always counts — that PR exists to be reviewed;
//   - otherwise the human has to mean it: mention the bot, or reply inside a
//     review thread the bot opened.
func turnStart(conv state.Conversation, c github.Comment, bot string, handles []string, ownPR, inOwnedThread bool) bool {
	if c.ID == 0 || c.IsBot(bot) {
		return false
	}
	if strings.TrimSpace(c.Body) == "" {
		return false
	}
	if conv.Seen(c.ID) {
		return false
	}
	switch conv.State {
	case state.ConversationAsking, state.ConversationBlocked:
		return true
	}
	return ownPR || inOwnedThread || config.Mentions(c.Body, handles)
}

// turnJob is one conversation with something to answer.
type turnJob struct {
	conv     state.Conversation
	comments []github.Comment
	// restart is set when a human asked for a fresh session. It is handled before
	// any work: the point of restarting is that the history is wrong.
	restart bool
}

// turnLoop runs one turn per conversation that has something new to answer.
// Turns come after the trigger-driven work of the tick, so a fresh assignment is
// never stuck behind a chat follow-up.
func (e *Engine) turnLoop(ctx context.Context) []Result {
	jobs, err := e.findTurns(ctx)
	if err != nil {
		e.logf("warning: could not collect turns: %v", err)
		return nil
	}
	var out []Result
	for _, job := range jobs {
		out = append(out, e.turn(ctx, job))
		if out[len(out)-1].State == StateBlocked && !e.opt.DryRun {
			// One block per tick, as with trigger-driven work: a storm of failure
			// comments is noise, and the next tick will find the next one.
			break
		}
	}
	return out
}

// findTurns looks at the conversations this repo has, not at the repo's whole
// comment history: archived conversations cost nothing, and a conversation's
// cursor means only its own recent comments are read.
func (e *Engine) findTurns(ctx context.Context) ([]turnJob, error) {
	convs, err := e.store.Conversations(e.opt.Repo.Name)
	if err != nil {
		return nil, err
	}
	handles := e.handles()
	var out []turnJob
	for _, conv := range convs {
		if _, err := workflow.Get(conv.Workflow); err != nil {
			continue // a workflow that is no longer compiled in
		}
		if !e.prof.Workflow(conv.Workflow).EnabledOr(true) {
			continue
		}
		comments, restart, err := e.pendingComments(ctx, conv, handles)
		if err != nil {
			e.logf("warning: comments of %s#%d: %v", conv.Workflow, conv.Number, err)
			continue
		}
		if len(comments) == 0 && !restart {
			continue
		}
		out = append(out, turnJob{conv: conv, comments: comments, restart: restart})
	}
	return out, nil
}

// pendingComments gathers the comments a conversation has not answered yet: its
// own issue or pull request, the pull request its branch produced, and the review
// threads the bot opened there.
func (e *Engine) pendingComments(ctx context.Context, conv state.Conversation, handles []string) ([]github.Comment, bool, error) {
	numbers := []int{conv.Number}
	var pr github.PR
	hasPR := false
	if conv.Branch != "" {
		got, found, err := e.opt.GitHub.FindPRByHead(ctx, e.opt.Repo.Slug, conv.Branch)
		if err == nil && found {
			pr, hasPR = got, true
			if got.Number != conv.Number {
				numbers = append(numbers, got.Number)
			}
		}
	}

	var out []github.Comment
	restart := false
	seen := map[int]bool{}
	for _, n := range numbers {
		comments, err := e.opt.GitHub.ListComments(ctx, e.opt.Repo.Slug, n, conv.LastCommentID)
		if err != nil {
			return nil, false, err
		}
		for _, c := range comments {
			if seen[c.ID] {
				continue
			}
			if !c.IsBot(e.opt.Repo.Bot) && !conv.Seen(c.ID) && e.isRestart(c.Body) {
				restart = true
			}
			// ownPR: a comment on the pull request this agent opened is feedback on
			// its own work, and needs no mention to count.
			if !turnStart(conv, c, e.opt.Repo.Bot, handles, hasPR && n == pr.Number, false) {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}
	}

	// A reply inside a thread the bot opened needs no mention: the thread is the
	// conversation. A human's thread stays the human's.
	if hasPR {
		threads, _, err := e.opt.GitHub.ListCommentThreads(ctx, e.opt.Repo.Slug, pr.Number)
		if err != nil {
			e.logf("warning: review threads of #%d: %v", pr.Number, err)
		}
		for _, th := range threads {
			if th.IsResolved || len(th.Comments) < 2 {
				continue
			}
			if !th.Comments[0].IsFrom(e.opt.Repo.Bot) {
				continue
			}
			for _, c := range th.Comments[1:] {
				if seen[c.ID] || !turnStart(conv, c, e.opt.Repo.Bot, handles, false, true) {
					continue
				}
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	return out, restart, nil
}

// isRestart reports whether a comment asks for a fresh session.
func (e *Engine) isRestart(body string) bool {
	return config.Mentions(body, e.handles()) && strings.Contains(strings.ToLower(body), "restart")
}

// turn runs one turn: restart it, refuse it, or hand it to dispatch with the
// conversation it belongs to.
func (e *Engine) turn(ctx context.Context, job turnJob) Result {
	conv := job.conv
	res := Result{Repo: e.opt.Repo.Name, Kind: string(turnKind(conv)), Workflow: conv.Workflow, Number: conv.Number}

	wf, err := workflow.Get(conv.Workflow)
	if err != nil {
		res.State, res.Detail = StateBlocked, err.Error()
		return res
	}

	if job.restart {
		return e.restartConversation(ctx, wf, conv, res, job.comments)
	}
	if e.opt.DryRun {
		res.State = StatePlanned
		res.Detail = fmt.Sprintf("turn %d: %d comment(s), session=%s", conv.Turns+1, len(job.comments), conv.SessionID)
		e.logf("dry run %s#%d: %s", conv.Workflow, conv.Number, res.Detail)
		return res
	}
	if max := e.prof.Issue.MaxTurns; max > 0 && conv.Turns >= max {
		out := e.blockedWithDetail(res, wf.Perms(), fmt.Sprintf(
			"this conversation has used its %d turns. Comment `@%s restart` to start a fresh session with the same issue.",
			max, e.botName()))
		// The comments are consumed: without this, the budget block would repeat on
		// every tick, and a storm of identical comments is exactly what a brake is
		// for.
		e.saveConversation(&conv, state.ConversationBlocked, job.comments, 0)
		return out
	}

	sel, issue, pr, err := e.loadTarget(ctx, wf, conv)
	if err != nil {
		return e.blocked(res, wf.Perms(), err)
	}
	return e.dispatch(ctx, sel, issue, pr, job.comments)
}

// loadTarget fetches the issue or pull request a conversation is about, and the
// pull request its branch produced (for an implement conversation).
func (e *Engine) loadTarget(ctx context.Context, wf workflow.Workflow, conv state.Conversation) (workflow.Selection, github.Issue, github.PR, error) {
	kind := turnKind(conv)
	if wf.Name() == "review" {
		pr, err := e.opt.GitHub.GetPR(ctx, e.opt.Repo.Slug, conv.Number)
		if err != nil {
			return workflow.Selection{}, github.Issue{}, github.PR{}, err
		}
		sel := workflow.Selection{Workflow: wf, Event: workflow.Event{
			Kind: kind, Number: conv.Number, Title: pr.Title, Labels: pr.Labels}}
		return sel, github.Issue{}, pr, nil
	}
	issue, err := e.opt.GitHub.GetIssue(ctx, e.opt.Repo.Slug, conv.Number)
	if err != nil {
		return workflow.Selection{}, github.Issue{}, github.PR{}, err
	}
	sel := workflow.Selection{Workflow: wf, Event: workflow.Event{
		Kind: kind, Number: conv.Number, Title: issue.Title, Labels: issue.Labels}}
	var pr github.PR
	if conv.Branch != "" {
		if got, found, ferr := e.opt.GitHub.FindPRByHead(ctx, e.opt.Repo.Slug, conv.Branch); ferr == nil && found {
			pr = got
		}
	}
	return sel, issue, pr, nil
}

// turnKind is the event a turn reports itself as. It is not a trigger — the
// trigger was a comment — but it decides what a result looks like.
func turnKind(conv state.Conversation) workflow.Kind {
	if conv.Workflow == "review" {
		return workflow.ReviewComment
	}
	return workflow.IssueAssigned
}

// restartConversation starts a fresh session for the same target: the record
// stays (it is the audit trail of turns and cost), the history does not.
func (e *Engine) restartConversation(ctx context.Context, wf workflow.Workflow, conv state.Conversation, res Result, comments []github.Comment) Result {
	conv.Gen++
	conv.SessionID = state.SessionIDFor(conv.Repo, conv.Workflow, conv.Number, conv.Gen)
	conv.State = state.ConversationWorking
	conv.Turns = 0
	for _, c := range comments {
		conv.MarkSeen(c.ID)
	}
	if err := e.store.SaveConversation(conv); err != nil {
		e.logf("warning: could not save the restarted conversation: %v", err)
	}
	if err := e.store.DeleteClaim(e.opt.Repo.Name, conv.Workflow, conv.Number); err != nil {
		e.logf("warning: could not clear the claim: %v", err)
	}
	_ = e.opt.GitHub.RemoveLabel(ctx, e.opt.Repo.Slug, conv.Number, e.prof.Issue.QuestionLabel)
	_ = e.opt.GitHub.RemoveLabel(ctx, e.opt.Repo.Slug, conv.Number, e.prof.Issue.BlockedLabel)
	_ = e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, conv.Number, fmt.Sprintf(
		"🔄 Fresh session. This conversation now has no memory of the earlier turns; your next comment on this "+
			"thread starts again from the beginning, with the issue as it reads today.\n\n<sub>session `%s`</sub>",
		conv.SessionID))
	e.logf("%s#%d restarted as %s", conv.Workflow, conv.Number, conv.SessionID)
	res.State, res.Detail = StateDone, "restarted: "+conv.SessionID
	return res
}

// turnComments converts the pending comments into the workflow's view of them.
func turnComments(pending []github.Comment) []workflow.JobComment {
	if len(pending) == 0 {
		return nil
	}
	out := make([]workflow.JobComment, 0, len(pending))
	for _, c := range pending {
		out = append(out, workflow.JobComment{
			ID: c.ID, Author: c.Author, Body: c.Body, Path: c.Path, Line: c.Line, IsReview: c.IsReview,
		})
	}
	return out
}

// turnReplyBody is what the agent says back on the comments that started the turn:
// its own final words, or the report's opening if it left none. Short on purpose —
// the detail is in the pull request and the run log.
func turnReplyBody(brainSummary, summary string) string {
	if body := strings.TrimSpace(brainSummary); body != "" {
		return clip(body, 2000)
	}
	if body := firstLines(summary, 8); body != "" {
		return body
	}
	return "Done — the change is on the pull request."
}

// turnReplyTargets is one comment per thread: the newest in each. Four comments on
// one file are one conversation, and answering each of them separately is how a bot
// becomes noise.
func turnReplyTargets(pending []github.Comment) []github.Comment {
	var order []string
	byKey := map[string]github.Comment{}
	for _, c := range pending {
		key := "conversation"
		if c.IsReview {
			key = "review:" + c.Path
		}
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = c // the slice is oldest-first, so the last wins
	}
	out := make([]github.Comment, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// lastReviewLimit caps the review remembered in the conversation file. The point
// is the reviewer's own findings, not a transcript: a few kilobytes is plenty, and
// the state file stays readable.
const lastReviewLimit = 4000

// ask posts the agent's question, marks the target as waiting, and keeps
// everything else: the branch, the draft pull request and the session. A question
// is a turn, not a failure.
func (e *Engine) ask(ctx context.Context, res Result, perms workflow.Perms, question, prURL string) Result {
	if perms.Comment {
		if err := e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, res.Number, questionComment(question, prURL)); err != nil {
			out := e.blocked(res, perms, fmt.Errorf("post the question: %w", err))
			return out
		}
	}
	if perms.Label {
		// A review carries its own in-progress label; the question label is shared,
		// because "waiting for a human" is the same state wherever it happens.
		inProgress := e.prof.Issue.InProgressLabel
		if res.Workflow == "review" {
			inProgress = e.prof.Issue.ReviewInProgressLabel
		}
		_ = e.opt.GitHub.RemoveLabel(ctx, e.opt.Repo.Slug, res.Number, inProgress)
		_ = e.opt.GitHub.EnsureLabel(ctx, e.opt.Repo.Slug, e.prof.Issue.QuestionLabel)
		_ = e.opt.GitHub.AddLabel(ctx, e.opt.Repo.Slug, res.Number, e.prof.Issue.QuestionLabel)
	}
	// The claim is what the status page reads, so it has to say "waiting", not
	// "running" or "done".
	claim := state.Claim{Repo: e.opt.Repo.Name, Workflow: res.Workflow, Number: res.Number,
		Kind: res.Kind, State: StateAsking, Detail: "waiting for an answer: " + firstLine(question), Started: e.now()}
	if prev, ok, err := e.store.LoadClaim(e.opt.Repo.Name, res.Workflow, res.Number); err == nil && ok {
		claim.Branch, claim.Attempt, claim.Started = prev.Branch, prev.Attempt, prev.Started
	}
	if claim.Attempt == 0 {
		claim.Attempt = 1
	}
	_ = e.store.SaveClaim(claim)

	res.State = StateAsking
	res.Detail = "waiting for an answer: " + firstLine(question)
	e.logf("%s#%d is waiting for an answer", res.Workflow, res.Number)
	return res
}

// questionComment renders the agent's question as the comment a human answers.
func questionComment(question, prURL string) string {
	var b strings.Builder
	b.WriteString("❓ **I need an answer before I can finish this.**\n\n")
	b.WriteString(strings.TrimSpace(question))
	b.WriteString("\n\nReply in a comment here and I will carry on from where I stopped — same session, same branch.")
	if prURL != "" {
		b.WriteString("\n\nThe work so far is in " + prURL + ".")
	}
	b.WriteString("\n\n<sub>If this is the wrong track entirely, say `restart` and I will start again from the issue.</sub>")
	return b.String()
}

// saveConversation records what the turn did: the state a human sees, the new
// cursor, the turns and the cost. Called on every way out of a run, so a killed
// tick is not the only thing the next tick knows about.
func (e *Engine) saveConversation(c *state.Conversation, st string, pending []github.Comment, costUSD float64) {
	if e.opt.DryRun || c == nil || c.Workflow == "" || c.Number == 0 {
		return
	}
	c.State = st
	c.Turn(e.now(), costUSD)
	for _, cm := range pending {
		c.MarkSeen(cm.ID)
	}
	if err := e.store.SaveConversation(*c); err != nil {
		e.logf("warning: could not save the conversation: %v", err)
	}
}

// clip shortens text to a byte budget on a line boundary, so a stored review reads
// as prose rather than as a chopped word.
func clip(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i > limit/2 {
		cut = cut[:i]
	}
	return cut + "\n\n(truncated)"
}

// botName is the login a human types when they mean the bot, without the App's
// `[bot]` suffix.
func (e *Engine) botName() string {
	return strings.TrimSuffix(e.opt.Repo.Bot, "[bot]")
}
