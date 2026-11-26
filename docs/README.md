# issuewright

Assign a GitHub issue to a bot. Come back to a draft PR with the work done, the
repo's own checks run, and the repo's own rules obeyed.

This is the operator's manual. The design rationale is in
[design.md](design.md), the brain driver contract in [brains.md](brains.md).

## The shape of it

```text
        issue assigned to the bot            PR opened / bot mentioned
                    │                                   │
                    └────────────► tick ◄───────────────┘
                                     │
                            workflow (implement | review)
                                     │   may it write?  ← Perms, enforced
                                     ▼
                        worktree ──► brain (pi | opencode | exec)
                                     │
                        checks ◄─────┘   the repo's own commands, chosen by path
                                     │
                     draft PR ───────┴──── review comment
```

Three pieces, deliberately separate:

| Piece | Lives in | Changes when |
| --- | --- | --- |
| the runner | this repo (a binary) | the tool changes |
| the profile | `.agent/agent.yaml` in your repo | your repo changes |
| the fleet config | `/srv/agent/config.toml` on the box | you add a repo |

## Install

On the machine that will do the work (the "box"):

```bash
go install github.com/satheeshds/issuewright@latest     # → $(go env GOPATH)/bin
sudo install -m755 "$(go env GOPATH)/bin/issuewright" /usr/local/bin/
sudo issuewright install                                # writes the systemd units
```

`issuewright install` writes `/etc/systemd/system/issuewright@.{service,timer}`.
One template instance per repo, so `issuewright@my-repo.timer` and
`issuewright@other-repo.timer` are independent.

### One command instead

`deploy/provision.sh` does everything below (idempotent, dry-run by default):

```bash
sudo ./deploy/provision.sh --slug you/repo --bot 'you-bot[bot]'          # show the plan
sudo ./deploy/provision.sh --apply --slug you/repo --bot 'you-bot[bot]'  # do it
```

It creates the service user, the `/srv/agent` tree, installs the pinned release,
writes `config.toml` and `agent.env` (never overwriting either), installs the
units, and prints the handful of steps that need a human — `gh auth login` as the
bot, `pi auth login`, and the agent's clone. The rest of this page is what it
does and how to do it by hand.

### Box layout

```text
/srv/agent/
├── config.toml            # fleet: which repos, which bots, brain defaults
├── agent.env              # tokens, mode 0600, loaded by systemd
├── repos/<repo>/          # a clone the agent owns (never a deploy checkout)
├── work/<repo>/issue-39/  # one worktree per issue
├── work/<repo>/review-7/  # one detached worktree per reviewed PR
├── logs/<repo>/.jsonl    # one run log per attempt
└── state/                 # locks, claim records
```

Create it once:

```bash
sudo useradd --system --create-home --shell /bin/bash agent
sudo install -d -o agent -g agent /srv/agent
sudo -u agent git clone https://github.com/<owner>/<repo> /srv/agent/repos/<repo>
sudo install -m600 -o agent -g agent agent.env /srv/agent/agent.env
```

### `agent.env`

```bash
# Token for the bot identity (a GitHub App installation token or a PAT).
GH_TOKEN=github_pat_...

# systemd gives a unit a minimal PATH. If go/flutter/pi live outside
# /usr/local/bin:/usr/bin:/bin, say so here or every check fails with 127.
PATH=/usr/local/bin:/usr/bin:/bin:/srv/agent/bin

# Only needed for the exec brain.
# AGENT_EXEC_COMMAND='mytool --dir {{WORKDIR}} --task {{PROMPT_FILE}}'

# Optional one-off overrides.
# AGENT_BRAIN=pi
# AGENT_THINKING=high
```

`chmod 600`, owned by `agent`. The `agent` user needs no other credentials — in
particular it does not need your database or `.env`.

## Wire a repo

1. Profile. In the repo:

   ```bash
   cd your-repo
   issuewright init            # sniffs go.mod / package.json / pubspec.yaml
   $EDITOR .agent/agent.yaml   # fix the checks, add the hard rules
   git add .agent && git commit -m "chore: add issuewright profile"
   ```

2. Fleet config. Add the repo to `/srv/agent/config.toml`:

   ```toml
   [[repo]]
   name      = "your-repo"
   path      = "/srv/agent/repos/your-repo"
   slug      = "you/your-repo"
   bot       = "your-bot[bot]"
   token_env = "GH_TOKEN"
   ```

3. Timer.

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now 'issuewright@your-repo.timer'
   systemctl list-timers 'issuewright@'
   ```

Then assign an issue to the bot. Within ten minutes (one tick) you should see a
claim comment, the `agent:in-progress` label, and eventually a draft PR.

## The bot identity

Two options, identical from the runner's point of view — it matches on the
`bot` string in the fleet config.

GitHub App (preferred). Create an App with `issues: write`,
`contents: write`, `pull_requests: write`, `metadata: read`; install it on the
repo. Its bot user (`name[bot]`) is assignable to issues. Use an installation
token in `agent.env`; installation tokens expire, so either mint one in a wrapper
or use `gh auth login` as a machine user for now.

Fine-grained PAT (zero registration). Create a second GitHub account, add it
as a collaborator, give it a fine-grained PAT with the same scopes, put the PAT
in `agent.env`, and assign issues to that account.

Either way the token must be a user or app token, not `GITHUB_TOKEN`: pull
requests opened with `GITHUB_TOKEN` do not trigger the repo's own CI.

## The profile

`issuewright init` writes a starter. The parts that matter:

```yaml
checks:                     # paths → commands; every matching check runs
  - name: go
    paths: ["apps/api//.go"]
    run: ["cd apps/api && go test ./... && go build ./..."]

  - name: codegen
    paths: ["queries/", "handlers/"]
    run: ["make sqlc", "make openapi"]
    drift: ["db/", "openapi/"]      # tooling may rewrite these, hands may not

protect:
  write_denylist: ["db/", "openapi/"]   # agent may not edit them
  never_run: ["make db-migrate", "make db-push"]
  secret_paths: [".env", ".env."]
```

- `drift` is a gate: after the check's commands run, those paths must be clean.
  A file that still differs was edited by hand instead of regenerated, and the
  run is blocked.
- `never_run` is enforced by scanning the run log for the commands the agent
  actually executed, not by asking it nicely in the prompt.
- `secret_paths` blocks publishing a diff that touches them.

A check can be `advisory: true` when the repo's own baseline is red (a test suite
with 33 known failures, an analyzer with 186 infos). Its result is still reported
in the PR body — marked ⚠️ — but it does not block the run. Use it instead of
deleting the check, and instead of pretending the baseline is green.

Everything in the profile is optional. A repo with no profile runs on detected
defaults — that is the point of `init` being an upgrade rather than an entry fee.

### Disabling a workflow

```yaml
workflows:
  review:
    enabled: false     # this repo only gets `implement`
```

## The status page

`issuewright serve` renders what the runner is doing on this box, on one page that
works on a phone as well as a laptop:

```bash
issuewright serve --bind 127.0.0.1:3040          # or --bind 0.0.0.0:3040 on a LAN
sudo issuewright install && sudo systemctl enable --now issuewright-web   # as a unit
```

| Block | Answers |
| --- | --- |
| **Health** | the token's remaining life, every timer with its next elapse and when it last fired |
| **Running** | the claim that is in flight, how long, on which branch, and the last few commands it ran |
| **Stuck** | every blocked job with the reason it stopped, the tail of the failing run, **the label that unblocks it**, and a link to the issue or PR |
| **Recently finished** | the last few jobs, so "did it land?" needs no `journalctl` |
| **Recent runs** | workflow, issue, duration, cost, tool count, and a link to the full log |

It reads only what the runner already writes — claims in `/srv/agent/state`, JSONL run
logs, the token expiry, `systemctl` for the timers — so it loads instantly and works
with the network down.

**It does not nag about finished work.** A claim is dropped by the next tick when its
issue is closed or its pull request is merged or closed — discovery only lists open
objects, and a claim missing from those lists is confirmed with one API call before it
is deleted, so a job that merely lost its label is not silently hidden.

**Read-only, by design.** There is no button that starts a run. The labels are the
only switch, and the page tells you which one:

| Stuck job | What unblocks it |
| --- | --- |
| blocked issue | remove `agent:blocked` (the next tick retries) |
| failed review | add `review:go` back |
| blocked respond | nothing — the cursor only advances on a successful reply, so it retries itself |

A `POST` to any endpoint returns **405** with that explanation. A web endpoint that
ran jobs would be a second scheduler and a fourth source of truth next to the label,
the claim and the cursor — which is exactly what caused three silent stalls already.

### Publishing it (pick one)

| Where | How |
| --- | --- |
| tailnet (this box) | `tailscale serve --bg --set-path /agent http://127.0.0.1:3040` → `https://<host>.<tailnet>.ts.net/agent` |
| LAN | `issuewright serve --bind 0.0.0.0:3040`, same trust model as the admin UI |
| any box, private | `issuewright serve` on loopback + `ssh -L 3040:localhost:3040 <box>` |
| public (not advised) | put it behind Caddy/nginx with auth |

The unit binds loopback on purpose, and the address is a default rather than a
constant: `AGENT_WEB_BIND` in `agent.env` overrides it. The first deployment picked
3030 and collided with an unrelated Pi session on the same box — the server now binds
before it announces anything, so a taken port fails with
`cannot listen on 127.0.0.1:3030: address already in use` instead of a restart loop
that looks like it started. Tailscale is a recipe, not a dependency — nothing about the page needs it.

## What happens on a run

1. Discover. `tick` polls issues assigned to the bot and PRs asking for its
   review. Nothing else.
2. Plan. Each event is matched against the enabled workflows' triggers.
   Review is scheduled before implement so a short review is not stuck behind a
   long build.
3. Skip. Anything already labelled `agent:in-progress` / `agent:blocked` /
   `agent:done`, or with a blocked claim, is left alone. This is what makes an
   interrupted tick cheap.
4. Claim. `agent:in-progress` label plus one comment naming the branch.
5. Run. The workflow renders a prompt (its template + your `.agent/rules.md`
   + the issue), the brain runs it in the worktree, and the JSONL stream is teed
   to `logs/<repo>/`.
6. Verify. Changed paths select the checks; the hard rules are applied.
7. Publish. `implement`: commit leftovers, push `agent/issue-<n>`, open or
   update a draft PR with `Closes #<n>`. `review`: post one review comment.
   A workflow without `Perms().Write` cannot reach a push at all.
8. Finish. `agent:done`, a comment with the PR link and check results — or
   `agent:blocked`, one comment with the reason, and the bot unassigned so it is
   not retried by accident.

### The agent's clone

When a token is configured (`token_env` in the fleet file, usually `GH_TOKEN`),
the clone goes over **HTTPS** with `gh` as the credential helper — never through
the account's default git protocol. `gh repo clone` follows whatever that account
uses, and an SSH account on a machine that has never talked to GitHub fails with
`Host key verification failed` on every tick. Without a token, the clone falls
back to `gh repo clone`, which needs the machine's own credentials.

The clone under `/srv/agent/repos/<repo>` is not a workspace — worktrees do the
work — so every tick fetches it and fast-forwards it to `origin/<base_branch>`.
That is how a profile edit on the base branch reaches the runner. A clone with
local changes is left alone and the run is refused loudly, because silently
resetting it would delete someone's work.

### Answering comments

A comment on a pull request is answered when it either **mentions the bot** or
**replies in a thread the bot opened**. The bot's login is always a handle, in both
spellings (`@name` and `@name[bot]`); add shorthands at whichever level suits:

| Scope | Where | Example |
| --- | --- | --- |
| every repo on the machine | `[agent] mentions` in `/srv/agent/config.toml` | `mentions = ["@agent"]` |
| one repo | `[[repo]] mentions` in the same file | `mentions = ["@fish-bot"]` |
| one repo's rules | `issue.mentions` in `.agent/agent.yaml` | `mentions: ["@agent"]` |

All three are merged (deduplicated, case-insensitive, `@` optional in config), so a
fleet-wide `@agent` plus a repo-specific alias both work. Everything else is left alone — a human
conversation is not an invitation.

```
human comment ──▶ 👀 reaction ──▶ one run per PR ──▶ replies (+ a commit, if asked and allowed)
                                                       └─▶ resolve the bot's own threads
```

- **One run per PR**, not per comment: four comments get one coherent answer.
- **Answers land where the comment is** — a review thread reply for an inline
  comment, a quoted conversation comment otherwise.
- **No loops**: the bot never answers itself or another bot, and a cursor
  (`state/<repo>/respond-<pr>.json`) records every answered comment so a killed tick
  cannot reply twice.
- **Fixes only on its own branch.** If a comment asks for a change and the branch
  starts with `branch_prefix`, the run commits and pushes. On a human's branch it
  never pushes: the patch is posted as a `diff` block for them to apply.
- **Resolves its own threads** once answered, never a thread a human opened.
- A reply naming a comment id the run was not given is dropped, not guessed — an
  answer in the wrong thread is worse than no answer.

The `respond` workflow's baseline permissions are `Comment` + `Checks`; `Write` is
granted per job, only when the branch is the runner's own (a `PermsFor` override), so
"may it push?" is answered by the engine, not by the prompt.

### Inline review comments

The reviewer writes `REVIEW.md` as YAML front matter plus prose:

```yaml
---
title: adds telemetry
findings:
  - severity: blocking          # blocking | non-blocking | question
    path: lib/telemetry.dart    # repo-relative, as it appears in the diff
    line: 30                    # line in the new file
    message: route arguments leak customer coordinates
    suggestion: |               # optional exact replacement
      Telemetry.routeObserver(redact: true)
---
## Summary
...
```

Each finding becomes a comment on that line (posted through the reviews API —
`gh pr review` cannot do inline comments). The diff is read locally first, so the
runner knows which lines are commentable:

- line inside a diff hunk → **inline** comment on that line
- file in the diff, line not in a hunk → comment attached to the **file**
- file not in the diff → listed in the review **body**
- no front matter at all → one body comment, as before

Nothing is dropped, and a malformed front matter is reported and posted verbatim
instead of failing the review.

### Triggering a review

Three ways, in order of how well they work:

| Trigger | Notes |
| --- | --- |
| **`review:go` label** on the PR | works everywhere, including repos where GitHub refuses to request review from a bot (`Could not resolve user with login …`) |
| PR opened with the bot as **requested reviewer** | only where the bot is a resolveable reviewer; requires the App to see the repo |
| `issuewright run --pr N --workflow review` | manual, any time |

**The PR's labels are the state machine.** The runner only ever looks for the
trigger label, and moves it through the lifecycle as it works:

```
review:go   ──claim──▶   review:in-progress   ──finish──▶   review:done
                                          └──failure──▶   review:blocked
```

- adding `review:go` is the only thing that asks for a review;
- the trigger is **consumed** on claim, so a review cannot be started twice — no
  hidden state, nothing to delete;
- to review again (a new commit, a changed prompt, another model) just re-add
  `review:go`; `review:blocked` means it failed and is worth a look first.

The labels are configurable (`issue.review_label`, `review_in_progress_label`,
`review_done_label`, `review_blocked_label`) and created on first use. The other
triggers — a requested review, a mention — have no label to reset, so they keep the
once-per-head-commit guard; `--force` overrides any previous claim for one run:

```bash
issuewright run --repo <repo> --pr 47 --workflow review --force
``` There is a test for exactly that (`TestReviewSkipsTheSameRevisionAndRepeatsOnNewOnes`).

### Per-issue overrides by label

```
model:provider/strong     run this issue on another model
thinking:low              less deliberation (cheaper) for a small task
brain:opencode            use a different driver for this run
```

Resolution, most specific first: **CLI flags → label → `workflows.<name>.brain:` →
profile `brain:` → `[[repo]]` → `[agent]` → driver default.** Unknown brains or thinking levels are logged and
dropped; the run continues on the configured value. The PR body records which
brain, model and thinking level actually produced the work.

### Resuming after an interruption

The branch and the worktree are the checkpoint. A tick that was killed mid-run
(redeploy, suspend, reboot) finds the worktree on the next tick and asks the brain
to continue rather than start over (`pi -c`, which resumes the session that was
interrupted). Brains without resume support get a prompt that summarises the
existing commits instead.

## Running one job by hand

```bash
issuewright status                     # what is pending, in flight, blocked
issuewright status --json | jq .       # same, machine-readable

issuewright run --repo your-repo --issue 39 --dry-run --verbose
issuewright run --repo your-repo --issue 39
issuewright run --repo your-repo --pr 7 --workflow review
issuewright run --repo your-repo --pr 7 --workflow review --force   # again, same commit
```

`--dry-run` resolves everything — prompt, brain, model, branch, checks — and
changes nothing. `--verbose` prints the full prompt, which is the fastest way to
understand what your profile actually produces.

## Logs

```bash
journalctl -u 'issuewright@your-repo' -n 200 --no-pager
ls -t /srv/agent/logs/your-repo/.jsonl | head
jq -c 'select(.type=="tool_execution_start") | .args.command' <the newest log>
cat /srv/agent/state/your-repo/.json   # claims: what is in flight
```

The run log is the audit trail: which workflow, which brain, which model, every
tool call, and the reported cost if the brain reports one.

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| `nothing pending` every tick | Issue not assigned to the `bot` string in the fleet config, or its label is `agent:blocked`/`agent:done`. `issuewright status` shows which. |
| `another tick holds the lock` | A previous run is still going, or a stale process. `cat /srv/agent/state/<repo>.lock`; the lock is released when the process exits. |
| `unknown brain "x"` | The name is not registered. `issuewright list brains`. |
| `profile version 2 is newer than this binary supports` | Upgrade the binary first, then bump the profile. |
| CI never runs on the agent's PR | The token is a `GITHUB_TOKEN`, not an App/PAT token. |
| Run blocks with `timed out after 90m` | Raise `max_run_minutes` in `[agent]`, or split the issue. The WIP is committed on the branch. |
| Run blocks with `these generated files still differ` | The agent hand-edited a `drift` path instead of running the generator. |
| Run blocks with `ran commands the profile forbids` | A `never_run` command appears in the log. That is the rule doing its job. |
| A review never arrives | Review needs the PR to request the bot as reviewer, or `issuewright run --pr N --workflow review`. |

## Known limitations

- CI on the agent's PR reflects the repo's baseline. The PR triggers the
  repo's own workflows (that is the point of a user/App token), so pre-existing
  CI failures show up there too. Fix CI or the agent's PRs will always look red.

- Migrations. A runner has no database. If the profile forbids the repo's
  migration command (as it should), a schema-changing issue yields a PR with the
  schema and queries updated, and a note that a human must migrate before merge.
- Mentions. `comment_mention` triggers exist in the workflow API but the CLI
  client does not poll for mentions yet; use `issuewright run --pr N` instead.
- One job per repo per tick. A long `implement` delays a `review`, since both
  take the repo lock.
- Trust. The agent runs as a normal user on the box with access to its own
  copy of the repos. Containerising it (see Pi's `containerization.md`) is the
  next hardening step, not something this tool does for you.
