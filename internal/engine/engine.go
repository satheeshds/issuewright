// Package engine is the tick loop: discover events, plan work, run it, verify it,
// publish it — and refuse everything the workflow's Perms did not claim.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satheeshds/issuewright/internal/brain"
	"github.com/satheeshds/issuewright/internal/checks"
	"github.com/satheeshds/issuewright/internal/config"
	"github.com/satheeshds/issuewright/internal/github"
	"github.com/satheeshds/issuewright/internal/state"
	"github.com/satheeshds/issuewright/internal/workflow"
	"github.com/satheeshds/issuewright/internal/worktree"
	"github.com/satheeshds/issuewright/prompts"
)

// Job states reported by the engine.
const (
	StatePlanned = "planned" // dry run: resolved, not executed
	StateRunning = "running"
	StateDone    = "done"
	StateBlocked = "blocked"
	StateSkipped = "skipped"
)

// Options configure an Engine.
type Options struct {
	Agent    config.AgentConfig
	Repo     config.Repo
	DryRun   bool
	Override config.Overrides
	GitHub   github.Client
	Log      io.Writer
	Verbose  bool
	// Now is injectable for tests.
	Now func() time.Time
	// Driver overrides brain registry lookup (tests, and --brain when the driver
	// needs construction arguments).
	Driver brain.Driver
	// Profile overrides loading .agent/agent.yaml (tests).
	Profile *config.Profile
}

// Result is what one job did.
type Result struct {
	Repo     string
	Kind     string
	Workflow string
	Number   int
	State    string
	Detail   string
}

// StatusRow is one line of `issuewright status`.
type StatusRow struct {
	Repo     string `json:"repo"`
	Kind     string `json:"kind"`
	Number   int    `json:"number"`
	Workflow string `json:"workflow"`
	State    string `json:"state"`
	Detail   string `json:"detail"`
}

// Engine runs workflows for one repo.
type Engine struct {
	opt   Options
	prof  *config.Profile
	store *state.Store
	wts   *worktree.Manager
	now   func() time.Time
}

// New builds an Engine for one repo.
func New(opt Options) (*Engine, error) {
	if opt.GitHub == nil {
		return nil, errors.New("engine: a github.Client is required")
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Repo.Name == "" || opt.Repo.Slug == "" {
		return nil, fmt.Errorf("engine: repo name and slug are required (got %q/%q)", opt.Repo.Name, opt.Repo.Slug)
	}

	prof := opt.Profile
	if prof == nil {
		var err error
		prof, err = config.LoadProfile(opt.Repo.Path)
		if err != nil {
			return nil, err
		}
	}
	if err := prof.Validate(); err != nil {
		return nil, err
	}
	for _, name := range workflow.Names() {
		if msg := prof.PickupConflict(name); msg != "" && opt.Verbose {
			fmt.Fprintf(opt.Log, "[%s] warning: %s\n", opt.Repo.Name, msg)
		}
	}

	choice := config.ResolveBrain(opt.Agent, opt.Repo, prof, opt.Override)
	if !brain.ValidThinking(choice.Thinking) {
		return nil, fmt.Errorf("engine: thinking level %q is not one of %v", choice.Thinking, brain.ThinkingLevels)
	}
	if opt.Driver == nil {
		if _, err := brain.Get(choice.Name); err != nil {
			return nil, err
		}
	}

	if cli, ok := opt.GitHub.(*github.CLI); ok && cli.Token == "" {
		cli.Token = tokenFor(opt.Repo)
	}
	stateDir := filepath.Join(filepath.Dir(opt.Agent.Workroot), "state")
	return &Engine{
		opt:   opt,
		prof:  prof,
		store: state.New(stateDir),
		wts: worktree.New(worktree.Options{
			CloneDir:     opt.Repo.Path,
			WorkRoot:     filepath.Join(opt.Agent.Workroot, opt.Repo.Name),
			BaseBranch:   prof.BaseBranch,
			BranchPrefix: prof.BranchPrefix,
			Token:        tokenFor(opt.Repo),
			GhBin:        opt.Agent.GhBin,
		}),
		now: opt.Now,
	}, nil
}

// Profile exposes the loaded profile (used by --dry-run reporting and tests).
func (e *Engine) Profile() *config.Profile { return e.prof }

// reloadProfile re-reads the profile after the clone has advanced. An injected
// profile (tests, --dry-run) is left alone.
func (e *Engine) reloadProfile() error {
	if e.opt.Profile != nil {
		return nil
	}
	prof, err := config.LoadProfile(e.opt.Repo.Path)
	if err != nil {
		return fmt.Errorf("reload profile: %w", err)
	}
	if err := prof.Validate(); err != nil {
		return err
	}
	e.prof = prof
	return nil
}

// BrainChoice is the resolved brain for this repo.
func (e *Engine) BrainChoice() config.BrainChoice {
	return config.ResolveBrain(e.opt.Agent, e.opt.Repo, e.prof, e.opt.Override)
}

func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(e.opt.Log, "[%s] "+format+"\n", append([]any{e.opt.Repo.Name}, args...)...)
}

// discover turns GitHub state into candidate events.
func (e *Engine) discover(ctx context.Context) ([]workflow.Event, []github.Issue, []github.PR, error) {
	var events []workflow.Event

	forceLabel := e.prof.Issue.ForceLabel
	byLabel := e.prof.PickupMode("implement") == "label"

	// Assignment and labelling are independent triggers. In label mode the label
	// alone is enough, which matters when assigning an App bot is not possible.
	var issues []github.Issue
	if byLabel && forceLabel != "" {
		labeled, err := e.opt.GitHub.ListIssuesWithLabel(ctx, e.opt.Repo.Slug, forceLabel)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("list issues labelled %s: %w", forceLabel, err)
		}
		issues = labeled
		for _, is := range issues {
			events = append(events, workflow.Event{
				Kind: workflow.IssueLabel, Number: is.Number, Title: is.Title, Labels: is.Labels,
			})
		}
	} else {
		assigned, err := e.opt.GitHub.ListAssignedIssues(ctx, e.opt.Repo.Slug, e.opt.Repo.Bot)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("list assigned issues: %w", err)
		}
		issues = assigned
		for _, is := range issues {
			events = append(events, workflow.Event{
				Kind: workflow.IssueAssigned, Number: is.Number, Title: is.Title, Labels: is.Labels,
			})
			if hasLabel(is.Labels, forceLabel) {
				events = append(events, workflow.Event{
					Kind: workflow.IssueLabel, Number: is.Number, Title: is.Title, Labels: is.Labels,
				})
			}
		}
	}

	prs, err := e.opt.GitHub.ListReviewRequestedPRs(ctx, e.opt.Repo.Slug, e.opt.Repo.Bot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list review requests: %w", err)
	}
	seenPR := map[int]bool{}
	for _, pr := range prs {
		seenPR[pr.Number] = true
		events = append(events, workflow.Event{
			Kind: workflow.PROpened, Number: pr.Number, Title: pr.Title, Labels: pr.Labels,
		})
	}
	// A label on the PR, for repos where GitHub refuses to request review from a
	// bot ("Could not resolve user with login …").
	if label := e.prof.Issue.ReviewLabel; label != "" && e.prof.Workflow("review").EnabledOr(true) {
		labelled, err := e.opt.GitHub.ListPRsWithLabel(ctx, e.opt.Repo.Slug, label)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("list prs labelled %s: %w", label, err)
		}
		for _, pr := range labelled {
			if seenPR[pr.Number] {
				continue
			}
			seenPR[pr.Number] = true
			prs = append(prs, pr)
			events = append(events, workflow.Event{
				Kind: workflow.PRLabel, Number: pr.Number, Title: pr.Title, Labels: pr.Labels,
			})
		}
	}
	return events, issues, prs, nil
}

// labelDefaults names the label each trigger means in this repo.
func (e *Engine) labelDefaults() workflow.LabelDefaults {
	return workflow.LabelDefaults{
		Issue: e.prof.Issue.ForceLabel,
		PR:    e.prof.Issue.ReviewLabel,
	}
}

func (e *Engine) enabled() func(workflow.Workflow) bool {
	return func(w workflow.Workflow) bool {
		return e.prof.Workflow(w.Name()).EnabledOr(true)
	}
}

// Tick runs one pass: every pending event that a workflow accepts, one at a time.
func (e *Engine) Tick(ctx context.Context) ([]Result, error) {
	lock, err := e.store.TryLock(e.opt.Repo.Name)
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			e.logf("another tick holds the lock; skipping this pass")
			return nil, nil
		}
		return nil, err
	}
	defer lock.Release()

	if err := e.wts.EnsureClone(ctx, e.opt.Repo.Slug); err != nil {
		return nil, err
	}
	// The clone may have just advanced: re-read the profile so a change on the
	// base branch is honoured by this tick, not the next one.
	if err := e.reloadProfile(); err != nil {
		return nil, err
	}

	events, issues, prs, err := e.discover(ctx)
	if err != nil {
		return nil, err
	}
	byIssue := map[int]github.Issue{}
	for _, is := range issues {
		byIssue[is.Number] = is
	}
	byPR := map[int]github.PR{}
	for _, pr := range prs {
		byPR[pr.Number] = pr
	}

	var results []Result
	for _, sel := range workflow.Plan(events, e.enabled(), e.labelDefaults()) {
		if r, ok := e.suppressed(sel); ok {
			results = append(results, r)
			continue
		}
		res := e.dispatch(ctx, sel, byIssue[sel.Event.Number], byPR[sel.Event.Number])
		results = append(results, res)
		if res.State == StateBlocked && !e.opt.DryRun {
			// One block per tick is enough: a storm of blocks buries the issue.
			e.logf("stopping this pass after a blocked %s#%d", sel.Workflow.Name(), res.Number)
			break
		}
	}

	e.cleanupMerged(ctx)
	return results, nil
}

// suppressed skips work that is already in flight, finished, or blocked by a
// human. This is what makes the tick idempotent.
func (e *Engine) suppressed(sel workflow.Selection) (Result, bool) {
	base := Result{Repo: e.opt.Repo.Name, Kind: string(sel.Event.Kind),
		Workflow: sel.Workflow.Name(), Number: sel.Event.Number}

	labels := sel.Event.Labels
	if hasLabel(labels, e.prof.Issue.InProgressLabel) {
		base.State, base.Detail = StateSkipped, "already in progress"
		return base, true
	}
	if hasLabel(labels, e.prof.Issue.BlockedLabel) {
		base.State, base.Detail = StateSkipped, "blocked; re-assign to retry"
		return base, true
	}
	if hasLabel(labels, e.prof.Issue.DoneLabel) {
		base.State, base.Detail = StateSkipped, "already done"
		return base, true
	}
	claim, ok, err := e.store.LoadClaim(e.opt.Repo.Name, sel.Workflow.Name(), sel.Event.Number)
	if err == nil && ok && claim.State == StateBlocked {
		base.State, base.Detail = StateSkipped, "claim is blocked"
		return base, true
	}
	return Result{}, false
}

// dispatch runs one workflow for one event.
func (e *Engine) dispatch(ctx context.Context, sel workflow.Selection, issue github.Issue, pr github.PR) Result {
	wf := sel.Workflow
	perms := wf.Perms()
	res := Result{Repo: e.opt.Repo.Name, Kind: string(sel.Event.Kind), Workflow: wf.Name(), Number: sel.Event.Number}

	// Build the job description.
	title, body, labels := sel.Event.Title, "", sel.Event.Labels
	branch := e.wts.IssueBranch(sel.Event.Number)
	workdir := e.wts.IssuePath(sel.Event.Number)
	var resume bool

	if wf.Name() == "implement" {
		if issue.Number != 0 {
			title, body, labels = issue.Title, issue.Body, issue.Labels
		}
		// Prepare the worktree before the prompt so resume state is known. A dry
		// run must not touch the filesystem beyond the clone.
		if e.opt.DryRun {
			resume = dirExists(workdir)
		} else {
			if err := e.wts.EnsureClone(ctx, e.opt.Repo.Slug); err != nil {
				return e.blocked(res, perms, fmt.Errorf("clone: %w", err))
			}
			path, br, resumed, err := e.wts.EnsureIssue(ctx, sel.Event.Number)
			if err != nil {
				return e.blocked(res, perms, fmt.Errorf("prepare worktree: %w", err))
			}
			workdir, branch, resume = path, br, resumed
		}
	} else {
		if pr.Number != 0 {
			title, body, labels = pr.Title, pr.Body, pr.Labels
			if pr.HeadRef != "" {
				branch = pr.HeadRef
			}
		}
		if !e.opt.DryRun {
			path, err := e.wts.EnsureReview(ctx, sel.Event.Number)
			if err != nil {
				return e.blocked(res, perms, fmt.Errorf("prepare review worktree: %w", err))
			}
			workdir = path
		} else {
			workdir = e.wts.ReviewPath(sel.Event.Number)
		}
	}

	rules, err := e.rulesFor(wf.Name())
	if err != nil {
		return e.blocked(res, perms, err)
	}

	job := workflow.Job{
		Kind:       sel.Event.Kind,
		Repo:       e.opt.Repo.Slug,
		Number:     sel.Event.Number,
		Title:      title,
		Body:       body,
		Labels:     labels,
		Branch:     branch,
		BaseBranch: e.prof.BaseBranch,
		Workdir:    workdir,
		Rules:      rules,
		Protect: workflow.Protect{
			WriteDenylist: e.prof.Protect.WriteDenylist,
			NeverRun:      e.prof.Protect.NeverRun,
			SecretPaths:   e.prof.Protect.SecretPaths,
		},
		Notes:  e.prof.Prompt.Notes,
		DryRun: e.opt.DryRun,
		Resume: resume,
	}

	spec, err := wf.Prompt(job)
	if err != nil {
		return e.blocked(res, perms, err)
	}

	// A review is per revision: the same head commit must not be reviewed on every
	// tick, and a new push must be.
	if wf.Name() == "review" && pr.HeadRefOID != "" {
		if claim, ok, _ := e.store.LoadClaim(e.opt.Repo.Name, wf.Name(), sel.Event.Number); ok &&
			claim.Detail == "reviewed:"+pr.HeadRefOID && claim.State == StateDone {
			res.State = StateSkipped
			res.Detail = "already reviewed " + pr.HeadRefOID[:7]
			e.logf("skipping review#%d: already reviewed %s", res.Number, pr.HeadRefOID[:7])
			return res
		}
	}

	choice := e.choiceFor(labels, wf.Name())
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
	spec.Continue = resume && caps.Resume
	if resume && !caps.Resume {
		if summary, serr := e.wts.CommitSummary(ctx, workdir, 10); serr == nil {
			spec.Prompt += "\n\nWork already on this branch:\n" + summary + "\n"
		}
	}

	if e.opt.DryRun {
		res.State = StatePlanned
		res.Detail = fmt.Sprintf("brain=%s model=%q thinking=%q branch=%s checks=%d prompt=%dB",
			choice.Name, spec.Model, spec.Thinking, branch, len(checks.Select(e.prof, dryChanged(e.prof))), len(spec.Prompt))
		e.logf("dry run %s#%d (%s): %s", wf.Name(), res.Number, sel.Event.Kind, res.Detail)
		if e.opt.Verbose {
			fmt.Fprintf(e.opt.Log, "----- prompt -----\n%s\n------------------\n", spec.Prompt)
		}
		return res
	}

	// Claim it visibly, then run.
	if perms.Label {
		_ = e.opt.GitHub.EnsureLabel(ctx, e.opt.Repo.Slug, e.prof.Issue.InProgressLabel)
		if err := e.opt.GitHub.AddLabel(ctx, e.opt.Repo.Slug, sel.Event.Number, e.prof.Issue.InProgressLabel); err != nil {
			return e.blocked(res, perms, fmt.Errorf("claim label: %w", err))
		}
	}
	if perms.Comment {
		_ = e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, sel.Event.Number, fmt.Sprintf(
			"🤖 **%s** picked this up on `%s`.\n\nI'll report back here; the details go to the run log.", e.opt.Repo.Bot, branch))
	}

	attempt := e.store.NextAttempt(e.opt.Repo.Name, wf.Name(), sel.Event.Number)
	claim := state.Claim{Repo: e.opt.Repo.Name, Workflow: wf.Name(), Number: sel.Event.Number,
		Kind: string(sel.Event.Kind), Branch: branch, Attempt: attempt, Started: e.now(), State: StateRunning}
	if err := e.store.SaveClaim(claim); err != nil {
		e.logf("warning: could not save claim: %v", err)
	}

	logPath, err := e.store.LogPath(e.opt.Agent.Logdir, e.opt.Repo.Name, wf.Name(), sel.Event.Number, e.now())
	if err != nil {
		return e.blocked(res, perms, fmt.Errorf("run log: %w", err))
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return e.blocked(res, perms, fmt.Errorf("run log: %w", err))
	}
	defer logFile.Close()
	runLog := io.MultiWriter(bestEffort{logFile}, bestEffort{e.opt.Log})
	fmt.Fprintf(runLog, "# issuewright %d#%d workflow=%s attempt=%d brain=%s workdir=%s\n",
		sel.Event.Number, res.Number, wf.Name(), attempt, choice.Name, workdir)

	e.logf("running %s#%d with %s%s%s (branch %s, log %s)", wf.Name(), res.Number, choice.Name,
		optSuffix(" model=", choice.Model), optSuffix(" thinking=", choice.Thinking), branch, logPath)
	runRes, runErr := driver.Run(ctx, spec, runLog)

	// Flush for the enforcement scan, but keep the handle open: the checks write
	// to the same log, and a closed file would SIGPIPE the check commands.
	_ = logFile.Sync()

	fail := func(reason string) Result {
		return e.blocked(res, perms, errors.New(reason))
	}

	if runErr != nil {
		e.preserveWork(ctx, workdir, perms)
		return fail(fmt.Sprintf("%s failed: %v", choice.Name, runErr))
	}
	e.logf("finished %s#%d in %s (exit %d, cost %.4f)", wf.Name(), res.Number, runRes.Duration.Round(time.Second), runRes.ExitCode, runRes.CostUSD)

	// Hard rules, enforced rather than trusted.
	if blocked, err := e.readBlocked(workdir); err != nil {
		return fail(err.Error())
	} else if blocked != "" {
		return fail("the agent stopped to ask:\n\n" + blocked)
	}
	if violations, err := e.forbiddenCalls(logPath); err != nil {
		e.logf("warning: could not scan the run log: %v", err)
	} else if len(violations) > 0 {
		return fail("the agent ran commands the profile forbids:\n\n- " + strings.Join(violations, "\n- "))
	}

	var changed []string
	if perms.Checks {
		changed, err = e.wts.ChangedFiles(ctx, workdir)
		if err != nil {
			return fail(fmt.Sprintf("list changed files: %v", err))
		}
		if e.opt.Verbose {
			e.logf("changed files: %v", changed)
		}
		if denied := checks.DeniedWrites(e.prof.Protect.WriteDenylist, changed); len(denied) > 0 {
			return fail("the agent edited files the profile forbids:\n\n- " + strings.Join(denied, "\n- "))
		}
		if secrets := checks.SecretFiles(e.prof.Protect.SecretPaths, changed); len(secrets) > 0 {
			return fail("the change touches secret files:\n\n- " + strings.Join(secrets, "\n- "))
		}
	}

	summary, err := e.readSummary(workdir)
	if err != nil {
		return fail(err.Error())
	}
	reviewBody, err := e.readReview(workdir)
	if err != nil {
		return fail(err.Error())
	}

	var checkResults []checks.Result
	if perms.Checks && len(changed) > 0 {
		runner := checks.New(checks.Options{Dir: workdir, Log: runLog})
		checkResults = runner.RunAll(ctx, checks.Select(e.prof, changed))
	}

	// Publish.
	detail := ""
	switch {
	case wf.Name() == "review":
		if perms.Write {
			return fail("a review workflow must not claim Write")
		}
		body := reviewBody
		if body == "" {
			body = runRes.Summary
		}
		body = e.withChecks(body, checkResults)
		if err := e.opt.GitHub.ReviewPR(ctx, e.opt.Repo.Slug, res.Number, body); err != nil {
			return fail(fmt.Sprintf("post review: %v", err))
		}
		detail = "review posted"
		if pr.HeadRefOID != "" {
			detail = "reviewed:" + pr.HeadRefOID
		}
	default:
		if !perms.Write {
			return fail(wf.Name() + " cannot publish without Write permission")
		}
		prURL, err := e.publish(ctx, res.Number, branch, title, summary, runRes.Summary, choice, checkResults)
		if err != nil {
			return fail(err.Error())
		}
		detail = prURL
		if perms.Comment {
			_ = e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, res.Number, fmt.Sprintf(
				"Draft pull request: %s\n\n**Checks**\n\n%s", prURL, checks.Summary(checkResults)))
		}
	}

	if checks.Failed(checkResults) && wf.Name() != "review" {
		return e.blockedWithDetail(res, perms, "checks failed (the PR records why):\n\n"+checks.Summary(checkResults))
	}

	if perms.Label {
		_ = e.opt.GitHub.RemoveLabel(ctx, e.opt.Repo.Slug, res.Number, e.prof.Issue.InProgressLabel)
		_ = e.opt.GitHub.EnsureLabel(ctx, e.opt.Repo.Slug, e.prof.Issue.DoneLabel)
		_ = e.opt.GitHub.AddLabel(ctx, e.opt.Repo.Slug, res.Number, e.prof.Issue.DoneLabel)
	}
	claim.State, claim.Detail = StateDone, detail
	_ = e.store.SaveClaim(claim)

	res.State, res.Detail = StateDone, detail
	return res
}

// publish commits leftovers, pushes the branch, and opens or updates the PR.
func (e *Engine) publish(ctx context.Context, number int, branch, title, summary, brainSummary string, choice config.BrainChoice, results []checks.Result) (string, error) {
	dir := e.wts.IssuePath(number)
	if branch == e.prof.BaseBranch {
		return "", fmt.Errorf("refusing to publish to the base branch %q", branch)
	}
	if !strings.HasPrefix(branch, e.prof.BranchPrefix) {
		return "", fmt.Errorf("refusing to publish branch %q: it does not start with %q", branch, e.prof.BranchPrefix)
	}

	// The agent's report is never committed, but always read first.
	body := e.prBody(number, summary, brainSummary, choice, results)
	_ = os.Remove(filepath.Join(dir, prompts.SummaryFile))
	_ = os.Remove(filepath.Join(dir, prompts.BlockedFile))
	_ = os.Remove(filepath.Join(dir, prompts.ReviewFile))

	if ok, err := e.wts.CommitAll(ctx, dir, fmt.Sprintf("chore(agent): leftover work for #%d", number)); err != nil {
		return "", fmt.Errorf("commit leftovers: %w", err)
	} else if ok {
		e.logf("committed leftover changes for #%d", number)
	}

	if err := e.opt.GitHub.PushBranch(ctx, dir, branch); err != nil {
		return "", fmt.Errorf("push: %w", err)
	}

	pr, found, err := e.opt.GitHub.FindPRByHead(ctx, e.opt.Repo.Slug, branch)
	if err != nil {
		return "", fmt.Errorf("look up pull request: %w", err)
	}
	if found {
		if err := e.opt.GitHub.UpdatePRBody(ctx, e.opt.Repo.Slug, pr.Number, body); err != nil {
			return "", fmt.Errorf("update pull request: %w", err)
		}
		if e.prof.Workflow("implement").EnabledOr(true) {
			_ = e.opt.GitHub.Comment(ctx, e.opt.Repo.Slug, number, fmt.Sprintf("Updated #%d (%s).", pr.Number, pr.URL))
		}
		return pr.URL, nil
	}

	created, err := e.opt.GitHub.CreateDraftPR(ctx, e.opt.Repo.Slug, branch, e.prof.BaseBranch, prTitle(number, title), body)
	if err != nil {
		return "", fmt.Errorf("create pull request: %w", err)
	}
	return created.URL, nil
}

func (e *Engine) prBody(number int, summary, brainSummary string, choice config.BrainChoice, results []checks.Result) string {
	body := strings.TrimSpace(summary)
	if body == "" {
		// No SUMMARY.md (a resumed run often just says "done"): use the brain's
		// final message rather than a canned template that would claim nothing.
		if brainSummary = strings.TrimSpace(brainSummary); brainSummary != "" {
			body = "> " + strings.ReplaceAll(brainSummary, "\n", "\n> ")
		}
	}
	if body == "" {
		body = prompts.Summary()
	}
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n---\n\n## Checks\n\n")
	b.WriteString(checks.Summary(results))
	if len(e.prof.Prompt.Notes) > 0 {
		b.WriteString("\n## Before merge\n\n")
		for _, n := range e.prof.Prompt.Notes {
			b.WriteString("- " + n + "\n")
		}
	}
	// Name the brain, model and thinking level: the PR is the audit trail, and a
	// label can change which model produced the work.
	attribution := fmt.Sprintf("Built by issuewright with the `%s` brain", choice.Name)
	var settings []string
	if choice.Model != "" {
		settings = append(settings, "model `"+choice.Model+"`")
	}
	if choice.Thinking != "" {
		settings = append(settings, "thinking `"+choice.Thinking+"`")
	}
	if len(settings) > 0 {
		attribution += " (" + strings.Join(settings, ", ") + ")"
	}
	fmt.Fprintf(&b, "\n<sub>%s.</sub>\n", attribution)
	if !strings.Contains(body, fmt.Sprintf("Closes #%d", number)) {
		fmt.Fprintf(&b, "\nCloses #%d\n", number)
	}
	return b.String()
}

func (e *Engine) withChecks(body string, results []checks.Result) string {
	if len(results) == 0 {
		return body
	}
	return body + "\n\n---\n\n**Read-only checks run by issuewright:**\n\n" + checks.Summary(results)
}

func (e *Engine) rulesFor(name string) (string, error) {
	path := e.prof.Prompt.PreambleFile
	if custom := e.prof.Workflow(name).Prompt; custom != "" {
		path = custom
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.opt.Repo.Path, path)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read rules %s: %w", path, err)
	}
	return string(raw), nil
}

// choiceFor resolves the brain for one job: labels from the issue are more
// specific than the profile, which is more specific than the fleet default.
func (e *Engine) choiceFor(labels []string, workflowName string) config.BrainChoice {
	labelOv, warnings := e.prof.LabelOverrides(labels)
	for _, w := range warnings {
		e.logf("warning: %s", w)
	}
	if labelOv.Brain != "" {
		if _, err := brain.Get(labelOv.Brain); err != nil {
			e.logf("warning: ignoring brain label: %v", err)
			labelOv.Brain = ""
		}
	}
	if labelOv.Thinking != "" && !brain.ValidThinking(labelOv.Thinking) {
		e.logf("warning: ignoring thinking label %q: not one of %v", labelOv.Thinking, brain.ThinkingLevels)
		labelOv.Thinking = ""
	}
	ov := config.MergeOverrides(e.opt.Override, labelOv)
	if ov != e.opt.Override {
		e.logf("label overrides for this issue: brain=%q model=%q thinking=%q", ov.Brain, ov.Model, ov.Thinking)
	}
	return config.ResolveBrainFor(e.opt.Agent, e.opt.Repo, e.prof, workflowName, ov)
}

func (e *Engine) driver() (brain.Driver, error) { return e.driverFor(e.BrainChoice()) }

func (e *Engine) driverFor(choice config.BrainChoice) (brain.Driver, error) {
	if e.opt.Driver != nil {
		return e.opt.Driver, nil
	}
	return brain.Get(choice.Name)
}

func (e *Engine) readSummary(dir string) (string, error) {
	return readArtifact(filepath.Join(dir, prompts.SummaryFile))
}

func (e *Engine) readReview(dir string) (string, error) {
	return readArtifact(filepath.Join(dir, prompts.ReviewFile))
}

func (e *Engine) readBlocked(dir string) (string, error) {
	return readArtifact(filepath.Join(dir, prompts.BlockedFile))
}

func readArtifact(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func (e *Engine) forbiddenCalls(logPath string) ([]string, error) {
	if len(e.prof.Protect.NeverRun) == 0 {
		return nil, nil
	}
	calls, err := checks.ScanToolCalls(logPath)
	if err != nil {
		return nil, err
	}
	return checks.ForbiddenCalls(calls, e.prof.Protect.NeverRun), nil
}

// preserveWork keeps partially finished work by committing it on the branch, so a
// blocked run leaves something to continue from instead of a dirty tree.
func (e *Engine) preserveWork(ctx context.Context, dir string, perms workflow.Perms) {
	if !perms.Write {
		return
	}
	ok, err := e.wts.CommitAll(ctx, dir, "chore(agent): work in progress (run did not finish)")
	if err != nil {
		e.logf("warning: could not preserve work: %v", err)
		return
	}
	if ok {
		e.logf("preserved in-progress work on the branch")
	}
}

// blocked records the failure once, labels it, unassigns the bot, and stops the
// retry loop.
func (e *Engine) blocked(res Result, perms workflow.Perms, err error) Result {
	return e.blockedWithDetail(res, perms, err.Error())
}

func (e *Engine) blockedWithDetail(res Result, perms workflow.Perms, detail string) Result {
	res.State, res.Detail = StateBlocked, firstLines(detail, 12)
	e.logf("blocked %s#%d: %s", res.Workflow, res.Number, res.Detail)

	if !e.opt.DryRun && perms.Comment {
		body := fmt.Sprintf("⚠️ **%s** could not finish this.\n\n```\n%s\n```\n\nRe-assign me (or remove `%s`) to try again.",
			e.opt.Repo.Bot, res.Detail, e.prof.Issue.BlockedLabel)
		_ = e.opt.GitHub.Comment(context.Background(), e.opt.Repo.Slug, res.Number, body)
	}
	if !e.opt.DryRun && perms.Label {
		_ = e.opt.GitHub.EnsureLabel(context.Background(), e.opt.Repo.Slug, e.prof.Issue.BlockedLabel)
		_ = e.opt.GitHub.AddLabel(context.Background(), e.opt.Repo.Slug, res.Number, e.prof.Issue.BlockedLabel)
		_ = e.opt.GitHub.RemoveLabel(context.Background(), e.opt.Repo.Slug, res.Number, e.prof.Issue.InProgressLabel)
		// The storm brake: an unassigned bot is not picked up again by accident.
		_ = e.opt.GitHub.Unassign(context.Background(), e.opt.Repo.Slug, res.Number, e.opt.Repo.Bot)
	}
	_ = e.store.SaveClaim(state.Claim{
		Repo: e.opt.Repo.Name, Workflow: res.Workflow, Number: res.Number,
		Kind: res.Kind, State: StateBlocked, Detail: res.Detail, Started: e.now(),
	})
	return res
}

// cleanupMerged removes worktrees whose PR is merged.
func (e *Engine) cleanupMerged(ctx context.Context) {
	removed, err := e.wts.Cleanup(ctx, func(branch string) bool {
		pr, found, err := e.opt.GitHub.FindPRByHead(ctx, e.opt.Repo.Slug, branch)
		if err != nil || !found {
			return false
		}
		return strings.EqualFold(pr.State, "MERGED")
	})
	if err != nil {
		e.logf("warning: cleanup: %v", err)
		return
	}
	for _, path := range removed {
		e.logf("removed merged worktree %s", path)
	}
}

// Status reports what is pending, in flight, and blocked.
func (e *Engine) Status(ctx context.Context) ([]StatusRow, error) {
	events, issues, prs, err := e.discover(ctx)
	if err != nil {
		return nil, err
	}
	byIssue := map[int]github.Issue{}
	for _, is := range issues {
		byIssue[is.Number] = is
	}
	_ = prs

	var rows []StatusRow
	for _, sel := range workflow.Plan(events, e.enabled(), e.labelDefaults()) {
		row := StatusRow{Repo: e.opt.Repo.Name, Kind: string(sel.Event.Kind),
			Number: sel.Event.Number, Workflow: sel.Workflow.Name(), State: "pending",
			Detail: byIssue[sel.Event.Number].Title}
		if r, ok := e.suppressed(sel); ok {
			row.State, row.Detail = r.State, r.Detail
		}
		rows = append(rows, row)
	}
	claims, err := e.store.Claims(e.opt.Repo.Name)
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		covered := false
		for _, row := range rows {
			if row.Number == c.Number && row.Workflow == c.Workflow {
				covered = true
			}
		}
		if covered {
			continue
		}
		rows = append(rows, StatusRow{Repo: e.opt.Repo.Name, Kind: c.Kind, Number: c.Number,
			Workflow: c.Workflow, State: c.State, Detail: c.Detail})
	}
	return rows, nil
}

// RunIssue runs one workflow for one issue, by hand.
func (e *Engine) RunIssue(ctx context.Context, number int, name string) (Result, error) {
	return e.runOne(ctx, number, name, workflow.IssueAssigned, func(n int, wfName string) (workflow.Selection, github.Issue, github.PR, error) {
		is, err := e.opt.GitHub.GetIssue(ctx, e.opt.Repo.Slug, n)
		if err != nil {
			return workflow.Selection{}, github.Issue{}, github.PR{}, err
		}
		wf, err := e.pickWorkflow(wfName, "implement")
		if err != nil {
			return workflow.Selection{}, github.Issue{}, github.PR{}, err
		}
		return workflow.Selection{Workflow: wf, Event: workflow.Event{Kind: workflow.IssueAssigned,
			Number: n, Title: is.Title, Labels: is.Labels}}, is, github.PR{}, nil
	})
}

// RunPR runs one workflow for one pull request, by hand.
func (e *Engine) RunPR(ctx context.Context, number int, name string) (Result, error) {
	return e.runOne(ctx, number, name, workflow.PROpened, func(n int, wfName string) (workflow.Selection, github.Issue, github.PR, error) {
		pr, err := e.opt.GitHub.GetPR(ctx, e.opt.Repo.Slug, n)
		if err != nil {
			return workflow.Selection{}, github.Issue{}, github.PR{}, err
		}
		wf, err := e.pickWorkflow(wfName, "review")
		if err != nil {
			return workflow.Selection{}, github.Issue{}, github.PR{}, err
		}
		return workflow.Selection{Workflow: wf, Event: workflow.Event{Kind: workflow.PROpened,
			Number: n, Title: pr.Title, Labels: pr.Labels}}, github.Issue{}, pr, nil
	})
}

func (e *Engine) runOne(ctx context.Context, number int, name string, _ workflow.Kind,
	load func(int, string) (workflow.Selection, github.Issue, github.PR, error)) (Result, error) {
	lock, err := e.store.TryLock(e.opt.Repo.Name)
	if err != nil {
		return Result{}, err
	}
	defer lock.Release()

	if err := e.wts.EnsureClone(ctx, e.opt.Repo.Slug); err != nil {
		return Result{}, err
	}
	if err := e.reloadProfile(); err != nil {
		return Result{}, err
	}
	sel, issue, pr, err := load(number, name)
	if err != nil {
		return Result{}, err
	}
	return e.dispatch(ctx, sel, issue, pr), nil
}

func (e *Engine) pickWorkflow(name, fallback string) (workflow.Workflow, error) {
	if name == "" {
		name = fallback
	}
	wf, err := workflow.Get(name)
	if err != nil {
		return nil, err
	}
	if !e.prof.Workflow(name).EnabledOr(true) {
		return nil, fmt.Errorf("workflow %q is disabled in %s", name, config.ProfilePath(e.opt.Repo.Path))
	}
	return wf, nil
}

// bestEffort swallows write errors: a broken or full log must never kill the
// command whose output it is recording.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	if b.w == nil {
		return len(p), nil
	}
	n, err := b.w.Write(p)
	if err != nil {
		return len(p), nil
	}
	return n, nil
}

// tokenFor resolves the bot credential for a repo from its configured env var.
func tokenFor(repo config.Repo) string {
	name := repo.TokenEnv
	if name == "" {
		name = "GH_TOKEN"
	}
	return os.Getenv(name)
}

func optSuffix(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func hasLabel(labels []string, want string) bool {
	if want == "" {
		return false
	}
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

func prTitle(number int, title string) string {
	if strings.TrimSpace(title) == "" {
		return fmt.Sprintf("Agent work for #%d", number)
	}
	return title
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
		lines = append(lines, "…")
	}
	return strings.Join(lines, "\n")
}

// dryChanged is the file set a dry run pretends changed: every check is selected
// so the report shows the full verification plan for the repo.
func dryChanged(prof *config.Profile) []string {
	var out []string
	for _, c := range prof.Checks {
		out = append(out, c.Paths...)
	}
	return out
}
