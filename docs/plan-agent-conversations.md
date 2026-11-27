# Plan: one agent per issue, one agent per PR (conversations over GitHub)

Status: **skeleton, round 1.** Open decisions at the bottom; nothing agreed yet.

## What is being asked for

Today every tick builds a job, spawns a brain process, and throws the thread away
afterwards. The ask is the opposite:

1. **One agent per issue.** The implement run that starts an issue is the *same*
   agent for the whole life of that issue: the first attempt, every retry, every
   "please change X" after the PR exists. It remembers what it decided and why.
2. **It can ask, and a human can answer, on the issue itself.** A clarifying
   question is a GitHub comment. The human replies; the same agent continues with
   the reply in context, instead of a fresh agent re-reading the issue from zero.
3. **One agent per PR.** The reviewer that first looked at a PR is the reviewer
   for that PR's lifecycle: new pushes, replies to its own threads, follow-ups. It
   remembers what it already flagged instead of re-deriving it every tick.
4. **Conversational through GitHub** — the issue/PR is the transcript surface, not
   a run log.

And the question behind it: can **pi-durable** be the thing that holds that
history, instead of us building it?

## What exists today (read from the code, not assumed)

| Thing | Today | File |
| --- | --- | --- |
| Implement continuity | worktree + branch persist; `resume` → `pi -c` ("most recent session in this dir") | `internal/engine/engine.go:dispatch`, `internal/worktree/worktree.go:EnsureIssue` |
| Review continuity | none: detached worktree at `review-<n>`, new process, no resume | `worktree.go:EnsureReview`, `engine.go:dispatch` |
| Comment replies | `respond` workflow: one run per PR per tick, per-PR cursor, replies to mentions and bot-started threads, push only to own branch | `internal/engine/respond.go`, `internal/workflow/respond/respond.go`, `internal/state/cursor.go` |
| Questions from the agent | terminal: `BLOCKED.md` → comment → `agent:blocked` → unassign → stop | `prompts.go`, `engine.go:blocked` |
| Issue comments | **not read at all.** Discovery lists assigned/labelled issues, review-requested PRs, labelled PRs, and comment threads on *touched PRs* | `engine.go:discover`, `respond.go:findCommentWork` |
| `CommentMention` trigger | declared by `review` but nothing ever discovers it — dead trigger today | `workflow/review/review.go:Triggers` |
| Session identity | implicit and fuzzy (`-c` = "latest session in this directory"); nothing records which session belongs to which issue/PR | `brain/pi/pi.go` (adds `-c` when `Spec.Continue`) |
| Durable state | plain files per claim under `<workroot>/../state/<repo>/` | `internal/state/state.go` |

So the plumbing for *reading and replying to GitHub comments* already exists in
`respond`; what is missing is **identity** (which conversation a comment belongs
to) and **turns** (a comment starting a turn on an existing conversation), plus a
non-terminal way for the agent to ask a question.

## The pi-durable question

**What it is.** `@earendil-works/pi-durable` (npm, 1.6.1 — same version line as
the installed `pi`) is Earendil's harness library: one conversation = storage +
tasks. Every model call and tool call is a durable task with a checkpoint written
*before* it runs; a killed process reopens the storage and continues, rerunning
tools only when the tool declares itself replay-safe. Submissions take a
`requestId` and are exactly-once. Input queues while the agent is busy (follow-up
or steer). Transcript is forkable at any entry. Compaction runs in the background.
Durable *jobs* have named phases and can park in an `approve` phase until a person
answers. It is TypeScript/Node (storage backends use no Node APIs).

**What it would buy issuewright, mapped to this plan:**

| Need here | pi-durable primitive |
| --- | --- |
| one agent per issue/PR, all history | conversation (root/fork), transcript + `search_history` |
| a human reply is a turn | `POST .../messages {content, requestId}` — keyed by comment id ⇒ exactly once |
| agent asks and waits | durable job with an `approval` phase, or just "no turn queued yet" |
| a second comment while working | queue with `followUp`/`steer`, `DELETE .../inbox/:id` |
| killed mid-tool on the nightly suspend | checkpoint per task; interrupted tools reported to the model |
| reviewer + author side by side | separate conversations, own model, read-only tool set, own cwd |
| audit trail | stored entries, normalized transcript (markdown export) |

**What it costs.** It is a Node library, not a service: there is no Go binding and
no shipped HTTP server. The public server-shaped thing is a reference
implementation (disler/`durable-pi`: `dpi-backend`, a `/v1` HTTP+SSE API with
sessions → agents → messages, exactly-once via `requestId`, SSE snapshots). To use
it from this Go binary we would either

- run a bridge service (a few hundred lines of Node around `Harness.open`), as a
  second systemd unit on the box, and give issuewright a `durablepi` brain that
  speaks HTTP instead of `exec`; or
- port nothing and instead keep pi's own session file as the store (below).

It is also, by its own announcement, **experimental** ("an experimental new
package"), explicitly not a replacement for the pi CLI. `docs/design.md` says the
durability boundary is the issue, not the turn, and that the bot's own run log is
the audit trail — a bridge service is a second store and a second failure mode to
operate.

**Verdict to argue about (my recommendation):** the conversational behaviour does
not depend on pi-durable at all — it is an engine/workflow/state change that works
with any brain that can resume. So build the conversation layer brain-agnostically
(phase 1), with per-target session identity passed through the existing
`brain.Spec`, and make pi-durable an **opt-in driver behind the existing brain
seam** (phase 3) rather than the foundation. If phase 3 happens, the seams do not
move: `brain.Caps` grows "queued input", `Spec` grows a session key, and the
`durablepi` driver talks to the bridge. Nobody using `pi` or `opencode` notices.

The cheap half of pi-durable that ships *today* with no new dependency: `pi
--session-id <id>` — "opens the current directory's existing session with the ID,
or creates it otherwise" (verified against the installed `pi 1.6.1`). Worktree
paths are already stable per number (`issue-<n>`, `review-<n>`), so a deterministic
session id per target is a real, durable conversation across ticks — everything
except mid-turn checkpoints, exactly-once turns and input queueing.

## Sketch (phase 1, if the recommendation holds)

- **Conversation identity**, stored in state, not derived only:
  `<state>/<repo>/conversations/<workflow>-<number>.json`
  `{session_id, cwd, branch, turns, state: working|asking|done|blocked, last_comment_id, opened_at, last_turn_at}`
  Session id is deterministic (`iw-<repo>-impl-12`) and recorded, so a lost state
  file can be rebuilt from the id and a wrong one can be corrected by hand.
- **`brain.Spec.Conversation`** (`{ID string, Turn int}`) — `pi` renders it as
  `--session-id`, `opencode` keeps `-c`, drivers without `Caps.Conversation` fall
  back to today's commit-summary prompt.
- **New artifact `QUESTION.md`** — the non-terminal sibling of `BLOCKED.md`. The
  engine posts it as a comment, labels the issue `agent:question`, saves the
  conversation as `asking`, and stops ticking that target until a human comments.
  `BLOCKED.md` stays for "I cannot continue".
- **Turn triggers.** A new discovery step over *open* conversations only (bounded
  by the state dir, not a repo-wide comment scan): a human comment in the issue or
  PR thread that either mentions the bot or answers a comment the bot wrote starts
  a turn on that conversation. The existing cursor (`state/cursor.go`) already
  gives one-turn-per-comment semantics; `requestId`-style idempotency is free
  because the cursor is keyed by comment id.
- **Who answers what.** The implement conversation answers comments on its own
  issue and its own PR (absorbing `respond`'s reply path); the review conversation
  answers comments in threads it opened. `respond` either becomes that shared turn
  machinery or stays as the stateless fallback — open question 3.
- **Review lifecycle.** Session id per PR; a new head commit is a new turn on the
  same review conversation, so it can say "my earlier point at foo.go:40 still
  stands" instead of reviewing the diff cold.
- **Closure.** PR merged/closed or issue closed → conversation archived
  (`state/<repo>/conversations/archive/`), worktree cleanup as today. A turn budget
  per conversation and a `@bot restart` command that forks a fresh session are the
  brakes.

## Files (first cut)

```text
internal/state/conversation.go        # load/save/archive, turn counter, session id
internal/brain/brain.go               # Spec.Conversation, Caps.Conversation, Caps.Queue
internal/brain/pi/pi.go               # --session-id from the conversation key
internal/brain/opencode/opencode.go   # map the key onto its own session id (if it has one)
internal/engine/discover.go           # extract discovery; add the conversation turn scan
internal/engine/turn.go               # a turn: resume → run → question/summary → state
internal/engine/engine.go             # dispatch calls turn; asking is not blocked
internal/github/client.go, fake.go    # issue comment threads (GraphQL already exists for PRs)
internal/workflow/implement, review   # Prompt(job) gets the conversation + last turn
prompts/workflows/implement.md        # QUESTION.md contract; "you are continuing this issue"
prompts/workflows/review.md           # "you reviewed this PR before; here is your last review"
docs/design.md                        # the durability-boundary decision, revisited
docs/README.md                        # the lifecycle, as a human sees it
```

## Steps

- [ ] Answer the open questions below; then this section becomes the real one.

## Verification

- engine tests: a human comment on an issue with an open conversation starts one
  turn with the *same* session id; a second tick posts nothing (cursor).
- a `QUESTION.md` run posts one comment, labels `agent:question`, leaves the claim
  in `asking`, and does not unassign or block.
- reply to that comment → next turn has the question and the answer in history
  (asserted via the fake brain's recorded `Spec.Conversation` + prompt).
- review: two pushes to one PR produce two turns with one session id, and the
  second prompt carries the first review.
- `pi --session-id` smoke test on the box: two `--print` runs with the same id in
  one directory, second run recalls the first (`--print "what did I just ask"`).
- `issuewright status` shows a conversation's state and turn count.

## Open questions

1. **pi-durable: default or opt-in?** Recommendation: opt-in driver behind the
   brain seam (phase 3), conversation layer built brain-agnostically now. The
   alternative is to make a Node bridge the default brain — that buys exactly-once
   turns and mid-tool recovery today, at the price of a second service to run,
   supervise and back up.
2. **Ask-and-wait, or ask-and-continue?** Recommendation: the agent may keep doing
   the parts it is sure about, and asks at the end (one comment, one label); it
   waits only when the answer changes the design.
3. **Who answers PR review comments — the author agent or `respond`?**
   Recommendation: the author conversation (it remembers why the code is the way
   it is); `respond` retires to a fallback for PRs whose conversation is unknown.
4. **Re-review on every push in the same conversation, or only when asked?**
   Recommendation: same conversation, and only when asked (review label /
   re-request), with the previous review in context.
5. **Brakes.** Max turns per conversation (10?), max turns per hour, and a
   one-comment `@bot restart` that forks a clean session? Recommendation: 10 turns,
   `@bot restart` for a fresh thread, archived on merge/close.
