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
	Resume    bool // can continue a previous session in the same directory
	Model     bool // accepts a model override
	Thinking  bool // accepts a reasoning/thinking level
	Streaming bool // emits structured events rather than plain text
}

type Spec struct {
	Prompt   string        // fully rendered: template + repo rules + issue
	Workdir  string        // the worktree the run owns
	Model    string        // "" = this driver's default
	Thinking string        // "" = this driver's default
	Timeout  time.Duration
	Continue bool          // resume the previous session, if Caps.Resume
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
   log lies about what was applied.
2. **A nonzero exit is a failure,** returned as an error. The engine turns that
   into `agent:blocked`, a comment, and an unassigned bot. A driver that swallows
   failures produces an agent that lies about success.

## Shipped drivers

| Driver | Invocation | Resume | Model | Thinking | Notes |
| --- | --- | --- | --- | --- | --- |
| `pi` | `pi --print --mode json --approve [--model M] [--thinking T] [-c]` | ✅ | ✅ | ✅ | JSONL events; parses the final message and the reported cost. `--approve` loads the repo's `AGENTS.md` and project rules. |
| `opencode` | `opencode run --auto [--model M] [-c]` | ✅ | ✅ | ❌ | The second real driver; proves the seam is not theoretical. |
| `exec` | `sh -c "$AGENT_EXEC_COMMAND"` with `{{PROMPT}}`, `{{PROMPT_FILE}}`, `{{WORKDIR}}` | ❌ | ❌ | ❌ | Escape hatch for any CLI today. |
| `stub` | writes one deterministic file | ✅ | ❌ | ❌ | Test driver. Releases are built with `-tags nostub`. |

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
  → .agent/agent.yaml  brain: {name, model, thinking}
    → [[repo]] in /srv/agent/config.toml
      → [agent] in /srv/agent/config.toml
        → driver default
```

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

`@earendil-works/pi-durable` is a TypeScript *library*, not a CLI. Using it means
building the agent as a Node service on an experimental API, and its durability
boundary is the model turn — finer than this tool needs.

The durability boundary here is the **issue**: the worktree and branch are the
checkpoint, Pi persists sessions per directory so `-c` continues an interrupted
run, and the tick is idempotent. If turn-level resume ever matters, it becomes a
`pi-durable` driver behind this same interface — which is exactly why the
interface exists.
