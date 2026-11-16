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
- `pi -c` resumes the session that was interrupted;
- the tick is idempotent, so a killed run is one tick from progress;
- a failure is preserved as a commit, labelled, and unassigned.

The trade: a run killed mid-turn may repeat some work. For issue-sized jobs that
is cheaper than the alternative.

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
