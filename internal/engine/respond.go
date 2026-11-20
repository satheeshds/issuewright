package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satheeshds/issuewright/internal/config"
	"github.com/satheeshds/issuewright/internal/findings"
	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/state"
	"github.com/satheeshds/issuewright/internal/workflow"
	"github.com/satheeshds/issuewright/prompts"
)

// ownedThreads are the threads this run answered and may therefore resolve: only
// threads the bot itself opened. A human's thread stays open for the human.
type ownedThreads map[int]string // comment id → thread id

// findCommentWork looks for pull requests with comments worth answering.
//
// A comment is actionable when it is from a human, is not already answered, sits
// in an unresolved thread, and either mentions the bot or lives in a thread the
// bot opened. One job per PR: four comments get one coherent answer.
func (e *Engine) findCommentWork(ctx context.Context) ([]commentJob, error) {
	if !e.prof.Workflow("respond").EnabledOr(true) {
		return nil, nil
	}
	if _, err := workflow.Get("respond"); err != nil {
		return nil, nil // the workflow is not compiled in
	}
	prs, err := e.opt.GitHub.ListOpenPRs(ctx, e.opt.Repo.Slug)
	if err != nil {
		return nil, fmt.Errorf("list open pull requests: %w", err)
	}

	handles := e.prof.MentionHandles(e.opt.Repo.Bot)
	var out []commentJob
	for _, pr := range prs {
		if !e.touchedPR(pr) {
			continue
		}
		threads, conversation, err := e.opt.GitHub.ListCommentThreads(ctx, e.opt.Repo.Slug, pr.Number)
		if err != nil {
			e.logf("warning: comments of #%d: %v", pr.Number, err)
			continue
		}
		cursor, err := e.store.LoadCursor(e.opt.Repo.Name, pr.Number)
		if err != nil {
			e.logf("warning: cursor of #%d: %v", pr.Number, err)
			continue
		}

		job := commentJob{PR: pr, Threads: ownedThreads{}}
		for _, th := range threads {
			if th.IsResolved || len(th.Comments) == 0 {
				continue
			}
			botStarted := th.Comments[0].IsFrom(e.opt.Repo.Bot)
			for _, c := range th.Comments {
				if !e.actionable(c, cursor, handles, botStarted) {
					continue
				}
				job.Comments = append(job.Comments, c)
				if botStarted {
					job.Threads[c.ID] = th.ID
				}
			}
		}
		for _, c := range conversation {
			if !e.actionable(c, cursor, handles, false) {
				continue
			}
			job.Comments = append(job.Comments, c)
		}
		if len(job.Comments) > 0 {
			out = append(out, job)
		}
	}
	return out, nil
}

// actionable applies the trigger rules and the no-loop rules.
func (e *Engine) actionable(c github.Comment, cursor state.Cursor, handles []string, botStarted bool) bool {
	if c.ID == 0 || c.IsBot(e.opt.Repo.Bot) {
		return false // never answer ourselves, and never answer another bot
	}
	if strings.TrimSpace(c.Body) == "" {
		return false
	}
	if cursor.Has(c.ID) {
		return false
	}
	return botStarted || config.Mentions(c.Body, handles)
}

// touchedPR reports whether this runner has business with the PR: it authored the
// branch, or it reviewed the PR.
func (e *Engine) touchedPR(pr github.PR) bool {
	if strings.HasPrefix(pr.HeadRef, e.prof.BranchPrefix) {
		return true
	}
	for _, l := range pr.Labels {
		if strings.EqualFold(l, e.prof.Issue.ReviewDoneLabel) {
			return true
		}
	}
	return false
}

// commentJob is one pull request's worth of comments to answer.
type commentJob struct {
	PR       github.PR
	Comments []github.Comment
	Threads  ownedThreads
}

// respond answers a pull request's comments in one run.
func (e *Engine) respond(ctx context.Context, job commentJob) Result {
	wf, err := workflow.Get("respond")
	if err != nil {
		return Result{Repo: e.opt.Repo.Name, Workflow: "respond", Number: job.PR.Number,
			State: StateBlocked, Detail: err.Error()}
	}
	res := Result{Repo: e.opt.Repo.Name, Kind: string(workflow.ReviewComment),
		Workflow: "respond", Number: job.PR.Number}

	ownBranch := strings.HasPrefix(job.PR.HeadRef, e.prof.BranchPrefix)
	perms := wf.Perms()
	if pf, ok := wf.(interface {
		PermsFor(workflow.Job) workflow.Perms
	}); ok {
		perms = pf.PermsFor(workflow.Job{OwnBranch: ownBranch})
	}

	// Acknowledge before thinking, so a human knows the comment landed.
	if !e.opt.DryRun && perms.Comment {
		for _, c := range job.Comments {
			if err := e.opt.GitHub.ReactToComment(ctx, e.opt.Repo.Slug, job.PR.Number, c, "eyes"); err != nil {
				e.logf("warning: could not react to comment %d: %v", c.ID, err)
			}
		}
	}

	workdir := ""
	if !e.opt.DryRun {
		if err := e.wts.EnsureClone(ctx, e.opt.Repo.Slug); err != nil {
			return e.blocked(res, perms, err)
		}
		path, err := e.wts.EnsureReview(ctx, job.PR.Number)
		if err != nil {
			return e.blocked(res, perms, fmt.Errorf("prepare worktree: %w", err))
		}
		workdir = path
	} else {
		workdir = e.wts.ReviewPath(job.PR.Number)
	}

	rules, err := e.rulesFor("respond")
	if err != nil {
		return e.blocked(res, perms, err)
	}

	jobInputs := workflow.Job{
		Kind: workflow.ReviewComment, Repo: e.opt.Repo.Slug, Number: job.PR.Number,
		Title: job.PR.Title, Body: job.PR.Body, Branch: job.PR.HeadRef,
		BaseBranch: e.prof.BaseBranch, Workdir: workdir, Rules: rules,
		Protect: workflow.Protect{SecretPaths: e.prof.Protect.SecretPaths},
		Notes:   e.prof.Prompt.Notes, OwnBranch: ownBranch, DryRun: e.opt.DryRun,
	}
	for _, c := range job.Comments {
		jobInputs.Comments = append(jobInputs.Comments, workflow.JobComment{
			ID: c.ID, Author: c.Author, Body: c.Body, Path: c.Path, Line: c.Line, IsReview: c.IsReview,
		})
	}
	spec, err := wf.Prompt(jobInputs)
	if err != nil {
		return e.blocked(res, perms, err)
	}

	choice := e.choiceFor(job.PR.Labels, wf.Name())
	driver, err := e.driverFor(choice)
	if err != nil {
		return e.blocked(res, perms, err)
	}
	caps := driver.Caps()
	spec.Model, spec.Thinking = choice.Model, choice.Thinking
	if !caps.Model {
		spec.Model = ""
	}
	if !caps.Thinking {
		spec.Thinking = ""
	}
	spec.Workdir = workdir
	spec.Timeout = time.Duration(e.opt.Agent.MaxRunMinutes) * time.Minute

	if e.opt.DryRun {
		res.State = StatePlanned
		res.Detail = fmt.Sprintf("%d comments, own branch=%t, prompt=%dB", len(job.Comments), ownBranch, len(spec.Prompt))
		e.logf("dry run respond#%d: %s", res.Number, res.Detail)
		return res
	}

	logPath, err := e.store.LogPath(e.opt.Agent.Logdir, e.opt.Repo.Name, wf.Name(), res.Number, e.now())
	if err != nil {
		return e.blocked(res, perms, err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return e.blocked(res, perms, err)
	}
	defer logFile.Close()
	runLog := io.MultiWriter(bestEffort{logFile}, bestEffort{e.opt.Log})
	e.logf("running respond#%d with %s%s for %d comment(s) (branch %s, log %s)",
		res.Number, choice.Name, optSuffix(" model=", spec.Model), len(job.Comments), job.PR.HeadRef, logPath)

	runRes, runErr := driver.Run(ctx, spec, runLog)
	_ = logFile.Sync()
	if runErr != nil {
		return e.blocked(res, perms, fmt.Errorf("%s failed: %w", choice.Name, runErr))
	}
	e.logf("finished respond#%d in %s (exit %d, cost %.4f)",
		res.Number, runRes.Duration.Round(time.Second), runRes.ExitCode, runRes.CostUSD)

	summary, err := e.readSummary(workdir)
	if err != nil {
		return e.blocked(res, perms, err)
	}
	resp, err := findings.ParseResponse(summary)
	if err != nil {
		return e.blocked(res, perms, fmt.Errorf("the run's replies are unusable: %w", err))
	}

	// Replies are only posted for comments this run was given. An id that does not
	// match is dropped, never guessed: posting an answer into the wrong thread is
	// worse than not answering.
	known := map[int]github.Comment{}
	for _, c := range job.Comments {
		known[c.ID] = c
	}
	var posted []int
	var unplaceable []int
	for _, r := range resp.Replies {
		c, ok := known[r.InReplyTo]
		if !ok {
			unplaceable = append(unplaceable, r.InReplyTo)
			continue
		}
		body := r.Body
		if err := e.opt.GitHub.ReplyToComment(ctx, e.opt.Repo.Slug, res.Number, c, body); err != nil {
			return e.blocked(res, perms, fmt.Errorf("reply to comment %d: %w", c.ID, err))
		}
		posted = append(posted, c.ID)
	}
	if len(unplaceable) > 0 {
		e.logf("warning: dropped replies for unknown comment ids %v", unplaceable)
	}

	// Changes: pushed only to a branch this runner owns.
	commitNote := ""
	if len(resp.Changes) > 0 && !perms.Write {
		// Never push to someone else's branch. Hand the patch over instead, so the
		// work is not lost and the human is not left guessing.
		patch, derr := e.wts.WorkingDiff(ctx, workdir)
		if derr != nil {
			e.logf("warning: could not read the patch: %v", derr)
		}
		note := "The comment asked for a change, but this pull request's branch is not one I created, so I have not pushed anything."
		if strings.TrimSpace(patch) != "" {
			note += "\n\n```diff\n" + strings.TrimRight(patch, "\n") + "\n```"
		}
		if err := e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, res.Number, note); err != nil {
			e.logf("warning: could not post the patch: %v", err)
		}
		res.Detail = "changes not pushed (not my branch); patch posted"
		e.logf("respond#%d: %s", res.Number, res.Detail)
	} else if len(resp.Changes) > 0 {
		if err := e.wts.EnsureClone(ctx, e.opt.Repo.Slug); err != nil {
			return e.blocked(res, perms, err)
		}
		_ = os.Remove(filepath.Join(workdir, prompts.SummaryFile))
		if ok, err := e.wts.CommitAll(ctx, workdir, fmt.Sprintf(
			"fix: address review comments on #%d", res.Number)); err != nil {
			return e.blocked(res, perms, fmt.Errorf("commit: %w", err))
		} else if ok {
			if err := e.opt.GitHub.PushBranch(ctx, workdir, job.PR.HeadRef); err != nil {
				return e.blocked(res, perms, fmt.Errorf("push: %w", err))
			}
			commitNote = " (pushed)"
			body := "Addressed review comments."
			if resp.Body != "" {
				body = resp.Body
			}
			if err := e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, res.Number, body); err != nil {
				e.logf("warning: could not comment the outcome: %v", err)
			}
		}
	}

	// Resolve the threads this run answered, but only threads the bot opened.
	for _, id := range posted {
		threadID, ok := job.Threads[id]
		if !ok {
			continue
		}
		if err := e.opt.GitHub.ResolveThread(ctx, threadID); err != nil {
			e.logf("warning: could not resolve thread %s: %v", threadID, err)
		}
	}

	// Advance the cursor last: a crash before this point means the next tick
	// retries, and a reply posted twice is caught by the ids already answered.
	cursor, err := e.store.LoadCursor(e.opt.Repo.Name, res.Number)
	if err != nil {
		e.logf("warning: cursor: %v", err)
	}
	for _, id := range posted {
		cursor.MarkAnswered(id)
	}
	if err := e.store.SaveCursor(e.opt.Repo.Name, res.Number, cursor); err != nil {
		e.logf("warning: could not save the comment cursor: %v", err)
	}

	res.State = StateDone
	res.Detail = fmt.Sprintf("%d repl%s%s", len(posted), plural(len(posted)), commitNote)
	return res
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// respondLoop answers comments for every PR with work, one run each.
func (e *Engine) respondLoop(ctx context.Context) []Result {
	jobs, err := e.findCommentWork(ctx)
	if err != nil {
		e.logf("warning: %v", err)
		return nil
	}
	var out []Result
	for _, job := range jobs {
		out = append(out, e.respond(ctx, job))
	}
	return out
}
