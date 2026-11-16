# issuewright

Assign a GitHub issue to a bot. Come back to a draft PR with the work done,
the repo's own checks run, and the repo's own rules obeyed.

`issuewright` is a small, config-driven runner that turns GitHub events into
agent runs. It knows nothing about your language, your CI, or your framework —
that lives in an `.agent/agent.yaml` profile committed in *your* repo.

```text
issue assigned to bot ──► issuewright tick ──► worktree ──► brain (pi|opencode|exec)
                                                              │
                          draft PR ◄── checks ◄───────────────┘
```

## What it is not

- Not a CI system, not a service, not a web app. One binary, one systemd timer.
- Not tied to one agent. The model CLI is a **brain driver**; `pi` ships first.
- Not tied to one kind of work. Implement, review, and (later) fix-CI and triage
  are **workflows** over the same plumbing — a reviewer is structurally unable
  to push.

## Install

```bash
go install github.com/satheeshds/issuewright@latest
```

## Wire a repo (3 steps)

1. `cd your-repo && issuewright init` — writes `.agent/agent.yaml` by sniffing
   `go.mod`, `package.json`, `pubspec.yaml`. Edit the checks it guessed.
2. Add the repo to `/srv/agent/config.toml` on the machine that runs the agent
   (see `deploy/config.toml.example`) and the bot token to `agent.env`.
3. `systemctl enable --now 'issuewright@your-repo.timer'`

Now assign an issue to the bot.

## Two seams

| Seam | Answers | Ships with |
| --- | --- | --- |
| Workflow | what may happen (write? comment? label?) and when | `implement`, `review` |
| Brain | who thinks, with which model and thinking level | `pi`, `opencode`, `exec`, `stub` |

Both are registries: adding one is a package plus a `Register` call.

```bash
issuewright new workflow triage    # scaffold + generated test + docs row
issuewright new brain claude       # same, for a model CLI
```

## Provision a box

```bash
sudo ./deploy/provision.sh --slug you/repo --bot 'you-bot[bot]'          # dry run
sudo ./deploy/provision.sh --apply --slug you/repo --bot 'you-bot[bot]'
```

Idempotent, and it stops at the things only a human can do (`gh auth login` as the
bot, `pi auth login`).

## Docs

- [`docs/README.md`](docs/README.md) — install, box layout, runbook, troubleshooting
- [`docs/brains.md`](docs/brains.md) — the driver contract
- [`docs/design.md`](docs/design.md) — why it looks like this
- [`examples/agent.yaml`](examples/agent.yaml) — a filled-in profile

## Status

Pre-1.0. `v0.1.0` is the first release; the profile schema is versioned (`version: 1`)
and the binary refuses a newer profile rather than guessing.
