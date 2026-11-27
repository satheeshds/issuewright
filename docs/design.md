# Why it looks like this

A record of the decisions, so the next person does not have to reverse-engineer
them from the code.

## The problem

Assigning an issue to a hosted coding agent is a good interface: write an issue,
walk away, come back to a pull request. What it costs you is control — of the
model, the machine, the rules the agent follows, and the data it can reach.

issuewright rebuilds that interface around a CLI you already use. The agent is
whatever `pi`, `opencode`, or your own tool does; the runner only knows how to
hand it a job and check the result.

## Decisions

### Three seams: trigger, workflow, brain

The obvious design is "wrap a model CLI in a loop that opens PRs". That collapses
under its second use case, because the difference between *implementing* and
*reviewing* is not who thinks — it is what is allowed to happen afterwards. A
reviewer that can write is not a reviewer.

So the runner has three independent extension points:

| Seam | Question | Registry |
| --- | --- | --- |
| Trigger | what woke us? | issue assigned, label, PR opened, mention |
| Workflow | what may happen? | `implement`, `review`, … |
| Brain | who thinks, with what settings? | `pi`, `opencode`, `exec`, `stub` |

`Perms` is the load-bearing part. A workflow declares `Write`, `Comment`,
`Label`, `Checks`; the engine refuses anything undeclared. A review run reaching
a `git push` is not a prompt instruction it can ignore — the call is never made.
There is a test for exactly that (`TestReviewWorkflowCannotWrite`).

Both registries are `database/sql`-shaped: a package per implementation, an
`init()` that registers, a `Names()`/`Get()` pair, and a scaffold command
(`issuewright new workflow|brain <name>`) that writes the package and a table
test. That is the whole cost of a new kind of runner.

### The profile lives in the target repo, not on the box

The runner needs to know which commands verify a change. Three options: guess
from the toolchain, configure it on the box, or commit it next to the code.

Guessing is why so many CI templates run the wrong thing. Box-side config drifts
silently and is invisible to review. A committed `.agent/agent.yaml` changes in
the same PR as the thing it describes, is reviewed by the people it constrains,
and `git blame` answers "who told the agent that".

The box keeps only what is genuinely machine-local: where the clones live, which
bot login, which token variable.

**Cost of the split:** the schema lives in the tool's repo while the file using it
lives in another. One integer (`version: 1`) is the entire contract; the binary
refuses a newer profile rather than guessing, and the upgrade order is: binary
first, profile second.

### Hard rules are enforced, not requested

A prompt saying "never edit generated files" is a suggestion. Three mechanisms
make it a rule:

- **`drift`** — after a check's commands run, its `drift` paths must be clean. A
  file that still differs was hand-edited instead of regenerated.
- **`write_denylist` / `secret_paths`** — checked against the changed-file list
  before anything is pushed.
- **`never_run`** — the run log is JSONL and contains every tool call, so the
  commands the agent actually executed can be scanned for forbidden prefixes.
  This is the difference between a policy and a hope.

One real bug came out of this: `git status --porcelain` collapses a new directory
to `db/`, so a path-level denylist missed every file inside it. `-uall` is now
pinned by a test.

### Durability boundary is the issue, not the turn

The box this runs on suspends every night, so a run *will* be killed mid-flight.

Rather than pay for turn-level durability (a durable-execution library, a Node
service, an experimental API), the checkpoint is coarse and free:

- the branch and worktree persist;
- the issue's session persists, and is named rather than guessed (`pi
  --session-id iw-<repo>-implement-<n>`);
- the tick is idempotent, so a killed run is one tick from progress;
- a failure is preserved as a commit, labelled, and unassigned.

The trade: a run killed mid-turn may repeat some work. For issue-sized jobs that
is cheaper than the alternative.

### One agent per issue, one per pull request

The first version threw the agent away with the process. Every tick was a fresh
model reading the issue from zero, and a reviewer that had already reviewed the
pull request could not say so. That is a bad collaborator: it re-derives, it
contradicts itself, and its earlier reasoning is not evidence for anything.

So a job has an **identity**: `<state>/<repo>/conversations/<workflow>-<n>.json`
holds the session id, the branch, the turns, the cost, the question it asked, the
comments it has answered, and (for a review) what it said last time. The state
file is small, greppable and deletable, like the claims next to it.

Three consequences, each deliberate:

- **A question is a turn, not a failure.** `QUESTION.md` is the non-terminal
  sibling of `BLOCKED.md`: the work is kept, a draft pull request opens, and the
  conversation waits under `agent:question` until a human answers. The alternative
  — blocking, which unassigns and stops — makes the bot ask once and give up,
  which is not how a colleague works.
- **The comment cursor is the exactly-once boundary.** A comment id is answered
  once, ever. That is why `requestId`-style idempotency was not worth a service:
  the same guarantee falls out of the cursor, keyed by the same id.
- **The reviewer keeps its review, but reviews the new revision cold.** Same
  session, so it remembers its own findings and must report fixed / still open /
  withdrawn; but every turn reads `base...HEAD` of *this* revision, so it cannot
  defend an earlier finding the diff has already answered. Memory without
  anchoring.

What is still refused: a background process, a queue, a scheduler. A turn is
still one blocking process inside one tick, and the labels still say what is
happening.

### A claim is a promise, and a promise is checkable

The bot speaks to a human, so everything it says is a commitment: "fixed in
b9ba799" tells the reader to stop looking. One live run made that commitment
false — the agent committed the fix inside a detached review worktree, the push
had nothing left to commit and was skipped, the reply had already gone out, and
the next tick deleted the worktree. The commit survived only as an unreachable
object.

Three rules came out of it, and each is enforced rather than asked for:

- **publish, then speak** — commits and pushes happen before any comment; a reply
  saying "done" cannot arrive before the branch is done;
- **verify the claim** — every commit id in a reply or report must resolve and be
  in the branch's history, or the run blocks and says which id it could not back
  up;
- **never delete unpublished work** — a worktree survives unless its PR is
  finished, its tree is clean and its commits are on the remote, and a review
  worktree holding a local commit refuses to be reset.

This is the same principle as the denylist and `never_run`: the interesting
failures are the ones where the tool believes itself.

### pi-durable is a driver, not a foundation

The conversation layer needs a session, which `pi --session-id` already gives.
pi-durable would add turn-level checkpoints, queued input and `requestId`
submissions — real, but not needed to talk to a human on GitHub, and it is a
library (Node) rather than a program, so adopting it means writing the harness
`pi` already is. It stays an opt-in driver behind `Caps.Conversation`
(`docs/brains.md`), and the durability boundary stays the issue.

### A timer, not a CI runner

A registered self-hosted Actions runner would give a re-run button and UI logs,
at the cost of runner registration, a workflow file on every repo's default
branch, and CI jobs competing with agent work for the same machine.

`OnCalendar=*:0/10` + `Persistent=true` gives the same trigger semantics —
including catching up after a suspend — with logs in journald and no repo-side
workflow at all. Because the tick is just a binary call, wrapping it in Actions
later is a ten-line YAML file, not a rewrite.

### No silent failover

If the configured brain fails, the run fails: `agent:blocked`, one comment, the
bot unassigned. Falling back to another model would make the run log lie about
which model produced a change, and the log is the only audit trail.

The interface permits `on_error = "switch:pi,opencode"`. It must be configured,
never implied.

### One job per repo per tick

Two runs writing to the same repo at once would fight over worktrees, labels, and
`main`. A `flock` per repo serialises them, and review — which is read-only and
short — is scheduled first so it does not queue behind a ninety-minute build.

## What is deliberately missing

- **Migrations.** Diffing a schema needs a live database. A runner should not
  have one. Schema issues produce a PR plus a note that a human must migrate.
- **Approving.** The agent may comment, never approve or merge. A human stays the
  gate on every merge.
- **A web UI.** `journalctl`, the JSONL logs, and `issuewright status` cover it.
