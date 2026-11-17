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
One template instance per repo, so `issuewright@daily-fish.timer` and
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
  never_run: ["make migrate", "make db-push"]
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

- Migrations. A runner has no database. If the profile forbids `make migrate`
  (as it should), a schema-changing issue yields a PR with the schema and queries
  updated, and a note that a human must run migrations before merge.
- Mentions. `comment_mention` triggers exist in the workflow API but the CLI
  client does not poll for mentions yet; use `issuewright run --pr N` instead.
- One job per repo per tick. A long `implement` delays a `review`, since both
  take the repo lock.
- Trust. The agent runs as a normal user on the box with access to its own
  copy of the repos. Containerising it (see Pi's `containerization.md`) is the
  next hardening step, not something this tool does for you.
