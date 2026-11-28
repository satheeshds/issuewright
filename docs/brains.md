# Brains

A *brain* is the CLI that thinks. issuewright owns everything around it —
worktree, prompt, checks, publishing — and knows nothing about which agent runs.

## The contract

```go
type Driver interface {
	Name() string
	Caps() Caps
	Run(ctx context.Context, spec Spec, log io.Writer) (Result, error)
}

type Caps struct {
	Resume       bool // can continue a previous session in the same directory
	Conversation bool // can open a *named* conversation and get its history back
	Model        bool // accepts a model override
	Thinking     bool // accepts a reasoning/thinking level
	Streaming    bool // emits structured events rather than plain text
}

type Spec struct {
	Prompt   string        // fully rendered: template + repo rules + issue
	Workdir  string        // the worktree the run owns
	Model    string        // "" = this driver's default
	Thinking string        // "" = this driver's default
	Timeout      time.Duration
	Continue     bool   // resume the previous session, if Caps.Resume
	Conversation string // this target's session id, if Caps.Conversation
}

type Result struct {
	Driver   string
	ExitCode int
	Duration time.Duration
	Summary  string  // final assistant text, when available
	CostUSD  float64 // best effort; zero means "not reported"
}
```

Two rules make the seam honest:

1. **`Caps` is a promise, not decoration.** Claiming `Resume` while not
   understanding a resume flag means the runner passes an argument the CLI
   rejects. Claiming `Thinking` when the flag only toggles display means the run
   log lies about what was applied. `Conversation` is the strongest of the four:
   it says "give me an id and I will come back with that thread's history", which
   is the difference between one agent per issue and a fresh agent every tick.
2. **A nonzero exit is a failure,** returned as an error. The engine turns that
   into `agent:blocked`, a comment, and an unassigned bot. A driver that swallows
   failures produces an agent that lies about success.

## Shipped drivers

| Driver | Invocation | Conversation | Resume | Model | Thinking | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| `pi` | `pi --print --mode json --approve [--model M] [--thinking T] [--session-id ID \| -c]` | ✅ | ✅ | ✅ | ✅ | JSONL events; parses the final message and the reported cost. `--approve` loads the repo's `AGENTS.md` and project rules. |
| `opencode` | `opencode run --auto [--model M] [-c]` | ❌ | ✅ | ✅ | ❌ | The second real driver; proves the seam is not theoretical. Its `--continue` is "the latest session here", not "the session with this id". |
| `exec` | `sh -c "$AGENT_EXEC_COMMAND"` with `{{PROMPT}}`, `{{PROMPT_FILE}}`, `{{WORKDIR}}` | ❌ | ❌ | ❌ | ❌ | Escape hatch for any CLI today. |
| `stub` | writes one deterministic file | ❌ | ✅ | ❌ | ❌ | Test driver. Releases are built with `-tags nostub`. |

When a driver claims `Conversation`, the engine hands it
`Spec.Conversation` — `iw-<repo>-<workflow>-<number>`, with `-r<n>` after a
restart — and never uses `-c` or the commit-summary fallback. When it does not,
nothing changes: the older `Continue` behaviour is what every other driver gets.

`issuewright list brains` prints the same table from the registry, so the docs
cannot rot without a test noticing.

## Two identities, often confused

- **GitHub identity** — who comments, pushes, and opens PRs. That is `GH_TOKEN`
  (an App installation token or a bot account's PAT) and the `bot` field in the
  fleet config. Pi has nothing to do with it.
- **Model identity** — whose subscription pays for the tokens. That is the
  credentials in the *service user's* `~/.pi/agent/auth.json`, written by
  `pi auth login` as that user. If the agent runs as `agent`, the Pi login in
  your own home directory is invisible to it.

`deploy/provision.sh --copy-pi-auth-from $USER` copies `auth.json`,
`settings.json`, and the installed Pi packages to the service user (0600) so the
first run works without an interactive login; deleting that `auth.json` revokes
it.

## Choosing one

Most specific wins:

```
--brain/--model/--thinking flags
  → a label on the issue (model:…, thinking:…, brain:…) when the profile allows it
  → .agent/agent.yaml  workflows.<name>.brain   (the reviewer can differ from the author)
  → .agent/agent.yaml  brain: {name, model, thinking}
    → [[repo]] in /srv/agent/config.toml
      → [agent] in /srv/agent/config.toml
        → driver default
```

Both shipped workflows use the same driver and model by default — the same
weights write the code and review it, so the same blind spots apply. Give
`workflows.review.brain` a different model (or driver) when that matters; the PR
body records which brain, model and thinking level actually produced each run.

Brains are **chosen, never silently swapped**. A failing driver fails the run:
`agent:blocked`, one comment, the log tail. Automatic failover would make the run
log lie about which model produced a change, and the log is the only audit trail
there is.

## Adding a brain

```bash
cd issuewright
issuewright new brain claude-code
```

That writes `internal/brain/claudecode/{claudecode.go,claudecode_test.go}`, which
registers itself in `init()`. Register it in `cmd/issuewright/main.go` with a
blank import:

```go
_ "github.com/satheeshds/issuewright/internal/brain/claudecode"
```

Then fill in:

1. `Caps()` — only what the CLI really supports.
2. `Run()` — map `Spec` onto flags. Two details that bite:
   - Set `Setpgid` and kill the **process group** on cancel, or a timeout leaves
     the agent's tool subprocesses holding the pipe and `Run` never returns.
   - Never hand a caller's `*bytes.Buffer` straight to `cmd.Stderr`: `io.Copy`
     will use `bytes.Buffer.ReadFrom`, which resets the buffer under any
     concurrent writer. Wrap it (`writeOnly`), as the pi driver does.
3. Watch for a CLI that finishes its work but does not exit. Pi signals
   `agent_settled` and then can sit in its event loop; the pi driver watches for
   that event and stops the process a few seconds later, treating the run as
   successful. Waiting for exit instead would burn the whole run timeout — it did,
   on the first live pilot. There is a regression test
   (`TestRunStopsAfterAgentSettled`) with a fake CLI that lingers.
4. The test — put a fake binary on `PATH` and assert the arguments, as
   [`internal/brain/pi/pi_test.go`](../internal/brain/pi/pi_test.go) does.

Roughly thirty lines. Nothing in the engine changes, and
`issuewright run --issue N --brain claude-code --dry-run --verbose` shows the
result before you spend a token.

## Why the first driver is `pi -p` and not `pi-durable`

`@earendil-works/pi-durable` is a TypeScript *library*, not a CLI. Node itself is
not the obstacle — `pi` is a Node CLI and the same `mise` installs both. The shape
is: Go cannot call a JS library in-process, so pi-durable has to be hosted by
something, and that something is a harness we would write and maintain.

What it would buy over `--session-id` is real but narrow:

| | `pi --session-id` (today) | pi-durable |
| --- | --- | --- |
| conversation history across ticks | ✅ | ✅ |
| mid-tool crash | the turn is lost; branch + transcript survive; the next tick retries | checkpoint per task; an interrupted tool is reported to the model and the run continues |
| a second comment while busy | waits for the tick; one turn in flight | queued or steered |
| exactly-once turn | the comment cursor, keyed by comment id | `requestId`, keyed by comment id |
| cost | one binary, one timer | a second process to run and back up |

The durability boundary here is still the **issue**: the worktree and branch are
the checkpoint, the session carries the conversation, and the tick is idempotent.
A turn killed mid-flight costs one turn, not the issue.

### The `durablepi` driver, if it is ever built

No code, no dependency, no bridge: this is the contract a future driver would
implement, so that nobody has to rediscover it.

**Shape.** A per-turn Node CLI, not a daemon:
`node turn.mjs --db <state>/<repo>/conversations/<workflow>-<n>.sqlite
--conversation iw-<repo>-<workflow>-<n> --input-file <prompt>`. It opens the
database, calls `harness.resume()`, submits the prompt with
`requestId` = the turn's id (the latest comment id, so a retried tick is the same
submission), waits for the answer, writes the JSONL the `eventTap` already
understands, and exits. A turn stays a blocking subprocess, so `Tick` does not
change. One SQLite file per conversation is the checkpoint; a killed process
reopens it.

**Contract.**

- `Caps.Conversation = true`. `Spec.Conversation` is the same
  `iw-<repo>-<workflow>-<n>` id, used as the directory/database name.
- `Run` returns a `brain.Result`; `CostUSD` comes from the harness's usage
  records, `Summary` from the last assistant entry.
- The artifacts (`SUMMARY.md`, `REVIEW.md`, `QUESTION.md`, `BLOCKED.md`) stay the
  channel back to the runner — they are the engine's contract, not the driver's,
  so a durable harness keeps writing them into `Spec.Workdir`.
- **Enforcement moves.** Today `never_run` is checked against the JSONL tool-call
  stream after the run. Inside a harness, the same list has to be a `beforeTool`
  hook that blocks the call before it runs; the profile's `write_denylist`,
  `secret_paths` and `propose_only` stay in the engine, since they are judged on
  the pushed diff. Two implementations of one rule is a real cost, and the reason
  this is opt-in.
- What is *not* needed: a shared app-state document, subagents, forks, background
  compaction. They are good ideas with no job here yet.

**What would make it worth building.** If a turn routinely runs long enough to be
killed by the machine's nightly suspend, or if humans start commenting faster than
one turn per ten minutes. Until then `--session-id` is the same conversation for a
fraction of the surface.
