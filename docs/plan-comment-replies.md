# Plan: replying to review comments

Status: **plan, nothing implemented.** Phase 1 is small; phase 2 needs a decision
from the operator (see *Open questions*).

## What is being asked for

After the bot posts a review, a human replies to it — "this is wrong", "why did you
say that", "fix it then". Today those comments are invisible to the runner: it posts
reviews and never looks back. This plan makes it answer.

Two capabilities, deliberately separated:

| Capability | What it does | Perms needed |
| --- | --- | --- |
| **reply** | answers the comment: agrees, explains, points at the code, or concedes | `Comment` |
| **fix** | pushes a commit that addresses the comment, then replies saying so | `Write` + `Comment` |

Reply-only is safe and useful; fix is the Copilot behaviour and needs a tighter
trigger.

## Facts established (probed, not assumed)

One GraphQL query returns everything needed, including thread state that REST does
not expose:

```graphql
pullRequest(number: 47) {
  reviewThreads(first: 50) {
    nodes {
      isResolved isOutdated
      comments(first: 20) { nodes { databaseId body author{login} path line createdAt } }
    }
  }
  comments(last: 20) { nodes { databaseId body author{login} createdAt } }
}
```

- `databaseId` is the REST id — replies go to
  `POST /repos/{o}/{r}/pulls/{n}/comments/{databaseId}/replies` with `{body}`.
- `isResolved` / `isOutdated` exist only in GraphQL, so this query is the way to
  ignore settled threads.
- **The bot's own login appears without the `[bot]` suffix** (`satheesh-pi-agent` in
  GraphQL, `satheesh-pi-agent[bot]` in PR JSON) — any "is this ours?" test must
  normalise both, or the runner will reply to itself forever.
- On PR #47 the probe showed 4 unresolved threads, all authored by the bot, none
  answered yet: a real first target.

## Which comments count as a question worth answering

Three candidates; this plan uses the last two, and leaves the first opt-in:

1. **any human comment on a PR we reviewed** — chatty, risks replies nobody asked
   for;
2. **a comment that mentions the bot** (`@satheesh-pi-agent …`) — explicit;
3. **a reply inside a thread whose first comment is ours** — this is literally
   "reply back on comments", needs no syntax, and cannot fire on someone else's
   conversation.

Rule: a comment is actionable when it is authored by a human, is not ours, the
thread is unresolved, and it either mentions the bot or sits in a bot-started
thread. Comments we have already answered are excluded by the cursor (below), not
by heuristics.

## Design

### A new workflow, not a mode of `review`

`internal/workflow/respond`:

```go
Name()      "respond"
Triggers()  []Trigger{{Kind: workflow.ReviewComment}}
Perms()     {Comment: true, Checks: true}          // phase 1
            {Comment: true, Checks: true, Write: fixMode}   // phase 2
Prompt(job) → prompts/workflows/respond.md
```

Same seams as the other two workflows: a new kind of work is a package plus a
`Register`. Reusing `review` would mean one workflow with two permission sets and
two prompt shapes — exactly the tangle the `Perms` split exists to avoid.

### Discovery

`internal/engine/discoverComments` lists the PRs the runner has already touched
(any claim, or a `review:done` label), then one GraphQL query per PR for threads
and issue comments. Kept out of the per-tick hot path: at most `N` PRs
(config, default 10), newest first.

### State: a cursor, not a label

Labels suit things a human toggles (review). A comment is not a toggle, so the
state is a cursor:

```text
state/<repo>/respond-<pr>.json
{ "last_comment_id": 4631614900, "answered": [4631614885, 4631614900], "mode": "reply" }
```

- advance only after a successful post;
- `answered` is a set, because threads can be answered out of order;
- capped at the last ~200 ids, so the file does not grow forever.

This gives idempotency (a killed run does not re-reply) without adding a fifth
family of labels.

### Reading and posting

`internal/github` gains three methods, each one `gh`/GraphQL call:

```go
ListCommentThreads(ctx, slug string, pr int) ([]CommentThread, error)  // GraphQL, one query
ReplyToReviewComment(ctx, slug string, pr, commentID int, body string) error  // REST replies
ReplyToIssueComment(ctx, slug string, pr, commentID int, body string) error   // REST, quoted
```

plus `reactToComment` for the cheap acknowledgement below.

`CommentThread{ID, IsResolved, IsOutdated, Comments []Comment}` and
`Comment{ID, Author, Body, Path, Line, CreatedAt}`, with
`Comment.IsFrom(botLogin string) bool` doing the `[bot]` normalisation — that
helper is the difference between a working agent and a mutual reply loop.

### The prompt

`prompts/workflows/respond.md` asks for front matter, like `review`:

```yaml
---
replies:
  - in_reply_to: 4631614885
    body: >
      Conceded — that clause was already fixed upstream. Dropped in abc1234.
changes:
  - path: openwiki/INSTRUCTIONS.md
    summary: dropped the stale-layout clause
---
## Summary
...
```

The runner posts `replies` in order and, in fix mode only, pushes the branch and
mentions the commit in each reply. A reply with no `in_reply_to` that matches a
known comment is dropped rather than posted into the wrong thread — the ordering
mistake that would be most embarrassing.

### Guardrails

| Risk | Guard |
| --- | --- |
| replying to itself | ignore any author matching the bot login, with or without `[bot]` |
| infinite ping-pong | cursor: one reply per comment, ever; a comment we already answered never re-opens |
| flooding | max 5 replies per run, max 10 PRs per tick |
| code pushed without being asked | `fix` only when the PR carries an explicit label (`agent:fix`, unset by default) |
| pushing to someone else's branch | reuses the existing check: branch must start with the profile's `branch_prefix` |
| answering a settled thread | skip `isResolved: true`, and `isOutdated: true` unless the comment is the newest |
| silent failure | a failed reply unlabels nothing; the cursor is *not* advanced, so the next tick retries once, then blocks with a comment |

Acknowledgement, optionally: react 👀 to an actionable comment
(`POST /issues/comments/{id}/reactions`). Cheaper and less noisy than a "working on
it" comment, and it tells the human the bot saw them.

### Tick shape

```text
tick
 └─ discover issues / PRs to review           (existing)
 └─ discover comment threads on touched PRs    (new)
      └─ respond job per PR (one run, all actionable comments in it)
           ├─ claim: 👀 reaction on each actionable comment
           ├─ run: brain reads the threads + diff, writes replies (+ optional changes)
           ├─ verify: checks, if the run changed code
           ├─ publish: reply mode → replies only
           │            fix mode  → commit, push branch, replies naming the commit
           └─ finish: advance cursor, one summary comment if anything changed
```

One run per PR, not per comment: a reviewer who leaves four comments gets one
coherent answer, and the run cost is one model call rather than four.

## Files

```text
internal/findings/replies.go          # parse the replies front matter (reuses findings.Parse style)
internal/github/comments.go           # GraphQL thread query, REST replies, reactions
internal/github/fake.go               # + CommentThreads, Replies, Reactions + call recording
internal/workflow/respond/respond.go  # the workflow
prompts/workflows/respond.md          # the prompt
internal/engine/discover_comments.go  # which PRs to look at, which comments are actionable
internal/engine/respond.go            # claim → run → post → cursor
internal/state/cursor.go              # per-PR cursor, capped
docs/README.md                        # the lifecycle + how to ask for a fix
```

## Tests

- `internal/github`: a canned GraphQL response parses into threads; a reply hits
  the right REST path with the right body; the `[bot]` normalisation (a comment by
  `satheesh-pi-agent` **is** ours).
- `internal/engine`:
  - a human reply in a bot thread produces exactly one reply, and a second tick
    after that posts nothing (cursor);
  - a comment mentioning the bot in a *human* thread is answered;
  - a resolved thread is skipped;
  - a comment that is ours is never answered;
  - two comments in one PR → one run, two replies, both with the right
    `in_reply_to`;
  - a reply with an unknown `in_reply_to` is dropped, and the run blocks rather
    than posting it somewhere else;
  - fix mode without the label never pushes; with the label, pushes only to a
    branch under `branch_prefix`.
- `internal/workflow/respond`: `Perms` claims `Comment` and **not** `Write` in
  reply mode — the same assertion style that guards the reviewer.

## Rollout

1. **Reply only.** Ship `respond` with `Perms{Comment, Checks}`, no code writes.
   Pilot on PR #47's four unresolved threads by replying to one as a human.
2. **Fix behind a label.** Add `agent:fix`; when present, `Write` is granted and the
   run may push to its own branch. Pilot by asking for a one-line change.
3. **Thread hygiene (optional).** Resolve the bot's own thread once its concern is
   addressed (`resolveReviewThread`), never a thread a human opened.

## Open questions

1. **Trigger.** Mentions + bot-thread replies only (proposed), or *any* human
   comment on a reviewed PR?
2. **Autofix.** Reply-only until someone adds a label (proposed), or push changes
   whenever the comment asks for one?
3. **Scope.** One run per PR per tick (proposed) — or per comment, so a long thread
   cannot starve the next review?
4. **Acknowledgement.** 👀 reaction (proposed) or a short "looking at this" comment?
5. **Resolving threads** the bot opened, once addressed — phase 3, or not at all?
