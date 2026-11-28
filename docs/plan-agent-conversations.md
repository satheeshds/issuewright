# Plan: one agent per issue, one agent per PR

Status: **implemented.** All ten steps are in `main`: conversations in state, the
turn loop, `QUESTION.md`, the author agent answering its own pull request, the
reviewer keeping one conversation, the question label and turn budget, archiving,
`status`, and the `durablepi` contract in `docs/brains.md`. The decisions and the
calls made while building are below.

## What is being asked for

Every tick today builds a job, spawns a brain process, and throws the thread away.
The ask is the opposite:

1. **One agent per issue** for the issue's whole life: first attempt, retries,
   and every "please change X" after the PR exists.
2. **It can ask, and a human answers, on the issue itself.** The question is a
   comment; the reply continues the same agent, not a fresh one.
3. **One agent per PR** for the review: new pushes, replies to its own threads.
4. The **issue/PR is the transcript**, not a run log.

Decided: **pi-durable stays opt-in**, behind the existing brain seam. The
conversation layer is built brain-agnostically and works with `pi` (via
`--session-id`), `opencode`, or a future `durablepi` driver.

## What exists today (read from the code)

| Thing | Today | File |
| --- | --- | --- |
| Implement continuity | worktree + branch persist; `resume` → `pi -c` ("latest session in this dir") | `engine.go:dispatch`, `worktree.go:EnsureIssue` |
| Review continuity | none: detached worktree at `review-<n>`, new process, no resume | `worktree.go:EnsureReview` |
| Comment replies | `respond`: one run per PR per tick, per-PR cursor, mentions + bot-started threads, push only to own branch | `engine/respond.go`, `workflow/respond/respond.go`, `state/cursor.go` |
| Agent asks a question | terminal: `BLOCKED.md` → comment → `agent:blocked` → unassign | `prompts.go`, `engine.go:blocked` |
| Issue comments | **never read.** Discovery = assigned/labelled issues, review-requested/labelled PRs, PR threads on touched PRs | `engine.go:discover`, `respond.go:findCommentWork` |
| `CommentMention` | declared by `review`, never discovered — dead trigger | `workflow/review/review.go` |
| Conversation identity | none; nothing records which session belongs to which issue/PR | — |
| Durable state | plain files, one per claim: `<state>/<repo>/<workflow>-<n>.json` | `state/state.go` |

The GitHub comment plumbing already exists in `respond` (GraphQL threads,
replies, reactions, thread resolution, cursor). Missing: **identity** and
**turns**.

## Decision log (round 1)

1. pi-durable = **opt-in driver**, not the default brain. (Tradeoffs in the next
   section.)
2. **Ask at the end**: the agent does everything it is sure about, then asks.
3. **The author agent answers review comments on its own PR**; `respond` becomes
   the fallback for PRs with no known conversation.
4. **Hybrid re-review** (see "Review per PR", below).
5. Brakes: turn budget, `@bot restart`, archive on merge/close.
6. **Ask at the end publishes a draft PR** — the question and the work land
   together, so the human can read the diff before answering.
7. A **human review requesting changes** on the bot's own PR starts an author
   turn; failing checks do not (that is a future `fix-ci` trigger).
8. **Phase 3 is documentation only**: the `durablepi` driver contract is written
   down, no bridge code, until it is asked for.

### Tradeoffs: opt-in driver vs default brain

| | Opt-in driver (chosen) | Default brain (pi-durable first) |
| --- | --- | --- |
| Runtime on the box | one binary + one timer (unchanged) | binary + Node bridge service + its own SQLite + a second systemd unit + version pinning (`pi-durable` is experimental) |
| Tick model | unchanged: a turn is a blocking process, `Tick` stays a loop over `Plan()` | inverts: a turn is a submission that outlives the tick, so `Tick` becomes submit-if-idle + collect-finished, and "one job per repo per tick" (the flock) has to be rethought |
| Mid-tool crash (nightly suspend) | turn is lost; branch + transcript survive; next tick retries | checkpoint per task, interrupted tool reported to the model, run continues |
| Exactly-once turns | free: the comment cursor is keyed by comment id | free: `requestId` = comment id |
| Comment mid-run | waits for the next tick; at most one turn in flight | queued / steer while busy |
| Enforcement (`never_run`, denylist, `propose_only`) | already enforced by the engine from the JSONL run log — no change | tool calls happen inside the harness: `never_run` must move to a `beforeTool` hook, and for a while there are two enforcement implementations |
| Reviewer read-only | engine refuses `Write` (`Perms`, tested) | also `readOnly` tool filtering in the harness — available but only via the bridge |
| Testing | fake `gh` + fake brain, whole pipeline offline | needs an HTTP fake for the bridge; the engine suite stops being one-process |
| Failure modes | a crash costs one turn | a stuck inbox/`busy` conversation needs draining; more state to reconcile |

What pi-durable would genuinely buy that we cannot cheaply fake: mid-tool
recovery, steering while busy, forks/subagents, `search_history` across
compaction. None of those are needed for the GitHub conversation UX, which is why
it is phase 3 and not the foundation. The cheap half ships today: `pi 1.6.1` has
`--session-id <id>` ("opens the current directory's existing session with the ID,
or creates it otherwise") and worktree paths are already stable per number
(`issue-<n>`, `review-<n>`).

### Why a bridge at all, and which kind

The runtime is not the problem. Node is already on the box (`pi` itself is a Node
CLI: its bin is `dist/bundle/cli.js`, and `node v26` is installed by the same
`mise` that installs `pi`). The problem is the **shape**: `@earendil-works/pi-durable`
is a library, not a program. Go cannot call into a JS library in-process, so
something has to host it. Three shapes, cheapest first:

| Shape | What it is | Keeps the tick model? | Cost |
| --- | --- | --- | --- |
| **Per-turn Node CLI** (the shape to document) | `node turn.mjs --db <state>/conversations/agent.sqlite --conversation iw-repo-implement-12 --input-file q.md`, prints JSONL, exits. The driver is a `Run()` like any other; SQLite is the conversation, `resume()` on open picks up a killed turn | yes — a turn stays a blocking subprocess, exactly like `pi --print` today | we write and maintain the harness: tools, prompt sections, model config, extensions. That is the slice `pi` already gives us for free |
| **Reference daemon** (disler/`durable-pi`: `dpi-backend`) | a Node service, `/v1` HTTP+SSE: sessions → agents → messages, `requestId`, inbox/steer, jobs with an approval phase | no — turns outlive the tick, so `Tick` becomes submit-if-idle + collect | a service to run, supervise and back up, built by a third party as a playground, not a supported product |
| In-process (cgo into `libnode`, or a Go reimplementation) | — | — | not worth discussing |

So the honest statement of phase 3's price: **not** a runtime, and not
necessarily a daemon — it is that adopting pi-durable means writing the harness
ourselves, and that it is explicitly experimental. `pi --session-id` gives us the
conversation; pi-durable would give us the turn-level checkpoint. That is the
trade, and it is why it is opt-in and last.

## Design

### Conversations

One record per (workflow, number), in the existing state dir:

```text
<state>/<repo>/conversations/implement-12.json
{
  "workflow": "implement", "number": 12,
  "session_id": "iw-issuewright-implement-12",
  "cwd": "/srv/agent/work/issuewright/issue-12",
  "branch": "agent/issue-12",
  "state": "working",              // working | asking | done | blocked | archived
  "turns": 3, "opened_at": "...", "last_turn_at": "...",
  "last_comment_id": 4631614900,   // the turn cursor
  "reviewed_sha": "", "last_review": "",   // review conversations only
  "cost_usd": 0.42
}
```

The session id is deterministic and recorded: a lost file can be reconstructed,
a wrong one corrected by hand. `pi` gets it as `--session-id`; a driver without
that capability keeps today's `-c` + commit-summary prompt (`Caps` already exists
for exactly this adaptation).

### A turn

```text
tick → discover conversations with new human comments (state dir, not a repo scan)
     → one turn per conversation, all pending comments batched
        1. ensure the worktree (issue-<n> or review-<n>), fetch, advance to head if needed
        2. build the prompt: base template + conversation banner + pending comments + (review) last review
        3. run the brain with the conversation's session id  → JSONL run log, as today
        4. artifacts: SUMMARY.md (report) / QUESTION.md (ask, non-terminal) / BLOCKED.md (stop)
        5. checks + denylist + propose_only, unchanged
        6. publish: implement → draft PR / update PR body; review → review with inline findings; reply mode → comments
        7. advance the cursor, save the conversation, mirror state into labels
```

`QUESTION.md` is the new, non-terminal artifact. Ordering is "ask at the end":
the run finishes its work first, the question is the last thing it writes. When a
question and passing work arrive together, the draft PR is opened/updated first
and the question is posted on the issue with a link to it, so the human reads the
diff next to the question. The conversation goes to `asking`, the issue gets
`agent:question`, and **nothing ticks that target until a human comments** —
`suppressed()` grows a case for it, mirroring today's "blocked claim is only
authoritative while the label is present".

If the same run also fails its checks, the failure wins: block as today and quote
the question inside the block comment (see *Calls I made*).

### Which comments start a turn

Runs from the state dir, one `ListComments(since: last_comment_id)` per open
conversation — no repo-wide comment crawl, and no cost for archived
conversations. A comment is a turn-start when: authored by a human (the existing
`Comment.IsFrom` normalises `[bot]`), not ours, not already in the cursor, and
either

- the conversation is `asking` (any human comment is the answer), or
- it mentions the bot (`config.Mentions`), or
- it is a reply in a review thread the bot opened (reuse `ownedThreads`), or
- it is a **review whose state is `CHANGES_REQUESTED`** on a PR the bot's own
  branch produced (decision 7: the same gesture as a change request, and the
  review body is the comment the turn answers).

One turn per conversation per tick, all pending comments in it — same shape as
`respond`'s one-run-per-PR.

### Author agent answers its own PR

The implement conversation covers comments on its issue *and* on the PR its
branch produced. `findCommentWork` skips PRs owned by a known conversation; the
conversation turn takes them. `respond` stays compiled in as the fallback for
PRs whose branch has no conversation (older PRs, humans' PRs) — it keeps its
`OwnBranch` write rule unchanged.

### Review per PR (decision 4)

Hybrid, not blind resume:

| | Independent per revision (today) | Same conversation every push |
| --- | --- | --- |
| Prompt context | diff only | diff + its own earlier review + the author's replies |
| Can say "I flagged this last push, still open" | no | yes |
| Duplicate findings across pushes | yes, re-posted cold | de-duplicated |
| Consistency of standards/tone | can contradict itself | stable |
| "Fresh eyes" bias | full | lost — a primed reviewer glosses over what it already decided |
| Cost per review | base | base + history (prompt cache helps) |
| Anchoring risk | none | defends an earlier wrong finding |

So: same conversation (memory + the last review stored), but every turn reviews
*this* revision from `base...HEAD`, and the prompt requires a short
"previous findings: fixed / still open / withdrawn" section before new findings.
`reviewed_sha` swaps for the old `reviewed:<sha>` claim detail; the label
lifecycle (`review:go → review:in-progress → review:done|blocked`) is unchanged,
so "review this again" stays a human gesture.

### Brakes

- `MaxTurns` per conversation (default 10) → one comment, `agent:blocked`.
- `@bot restart` in a comment → archive the conversation, start a new session id
  suffix (`-r2`), one comment.
- Merge or close archives the conversation (`conversations/archive/`); worktree
  cleanup is unchanged.
- Per-turn the existing `MaxRunMinutes` timeout applies; conversation `cost_usd`
  accumulates from `brain.Result.CostUSD` so `issuewright status` can show it.

## Files (first cut)

```text
internal/state/conversation.go     # load/save/archive/list, turn counter, cost
internal/config/profile.go         # conversation settings: max_turns, question label
internal/brain/brain.go            # Spec.Conversation, Caps.Conversation
internal/brain/pi/pi.go            # --session-id from the conversation key
internal/brain/opencode/opencode.go# keep -c; declare Caps.Conversation=false
internal/github/client.go, fake.go # ListComments(slug, number, since) + post to issue
internal/engine/discover.go        # split discovery out of engine.go; add the turn scan
internal/engine/turn.go            # conversation turn: prepare → run → artifacts → publish
internal/engine/engine.go          # dispatch delegates; asking suppressed; publish path shared
internal/workflow/workflow.go      # Job: Conversation, Comments, LastReview; Prompt shape
internal/workflow/implement, review# prompt inputs for a continuing turn
prompts/prompts.go                 # QuestionFile = "QUESTION.md"
prompts/workflows/implement.md     # "you are continuing #12"; QUESTION.md contract; ask at the end
prompts/workflows/review.md        # "you reviewed this PR before"; previous-findings section
docs/README.md                     # the lifecycle a human sees, labels, @bot restart
docs/design.md                     # revise "durability boundary is the issue, not the turn"
examples/agent.yaml                # new labels + max_turns, commented
```

## Steps

- [x] `state.Conversation` + tests (round-trip, archive, deterministic id, turns, cost).
- [x] `brain.Spec.Conversation` / `Caps.Conversation`; pi passes `--session-id`;
      smoke test on the box that two `--print` runs with one id share history.
- [x] `prompts.QuestionFile`, `QUESTION.md` in the implement and review prompts;
      engine reads it beside SUMMARY/BLOCKED.
- [x] `github.ListComments(since)` + `github.Fake` recording; issue-comment
      actionability (`asking` / mention / owned thread) with tests.
- [x] `engine/turn.go`: turn scan from conversation state, one turn per
      conversation per tick, cursor advance, cost accumulation.
- [x] `suppressed()`: `asking` is not re-dispatched; `agent:question` added to the
      label set (config default, `examples/agent.yaml`, README table).
- [x] Author conversation answers PR comments: `findCommentWork` skips owned PRs;
      `respond` stays as fallback (test both paths).
- [x] Review conversation: `reviewed_sha`, `last_review`, previous-findings
      section, re-review on a new push in the same session.
- [x] `@bot restart`, `MaxTurns`, archive on merge/close, `status` shows
      conversation state + turns + cost.
- [x] Phase 3 (documentation only, decision 8): a `durablepi` section in
      `docs/brains.md` — the driver contract (`Caps.Conversation`, a conversation
      key, an input keyed by comment id), the per-turn Node CLI shape as the
      reference bridge, the SQLite-per-conversation layout, and what it would buy
      over `--session-id` (mid-turn checkpoint, exactly-once turn, steering). No
      bridge code, no dependency until asked.

## Verification

Engine tests (fake `gh` + fake brain, as the suite already works):

- a human comment on an issue with an open conversation → exactly one turn, and
  the brain was handed the *same* `Spec.Conversation` id; a second tick posts
  nothing (cursor).
- `QUESTION.md` → one comment, `agent:question`, conversation `asking`, no
  unassign, no `agent:done`, and the next tick does nothing.
- reply to the question → the turn's prompt contains the question and the answer.
- two comments in one issue before a tick → one turn, both comments in it.
- a comment by the bot or another bot never starts a turn; a resolved thread
  never starts one (reuse `respond` tests).
- author conversation: a human review comment on its own PR → turn on the author
  session, not a `respond` run; a human comment on a foreign PR → `respond`.
- review: two pushes → two turns, one session id, second prompt carries the first
  review; a push with no new review request does nothing.
- `MaxTurns` reached → blocked comment, no further turns; `@bot restart` → new
  session id.

Box checks:

- `pi --print --session-id smoke-1 "remember the number 41"` then
  `pi --print --session-id smoke-1 "what number?"` → answers 41.
- End to end on a test repo: assign an issue whose body is deliberately
  ambiguous → agent asks at the end with a draft PR open → answer in the issue →
  second run continues the same session and updates the same PR.

## Calls I made

Settled by you, so nothing to check unless I read them wrong: opt-in `durablepi`
(1), ask at the end (2), the author agent answers its own PR (3), hybrid re-review
(4), brakes and `@bot restart` (5), draft PR on ask (6), a `CHANGES_REQUESTED`
review starts an author turn (7), phase 3 is docs only (8).

Mine, stated so they can be corrected while reading:

- **A failed check beats a question.** If a run asks and also fails checks, the
  conversation blocks as today and the block comment quotes the question, so a
  human sees both in one place instead of a question that looks answered-able
  while the branch is red.
- **Labels mirror the conversation, they do not own it.** The conversation file
  is the state; `agent:in-progress` / `agent:question` / `agent:blocked` /
  `agent:done` are the human-visible mirror. Where they disagree (a human removed
  a label by hand) the label wins, because that is how a human says "retry".
- **One turn per conversation per tick**, batching every pending comment, so four
  comments get one coherent answer and one bill.
- **`MaxTurns` default 10**, counted per conversation, reset only by
  `@bot restart`.
- **`respond` stays compiled in** as the fallback for PRs with no conversation,
  rather than being deleted: it is the tested path for foreign PRs.
