#!/usr/bin/env bash
# provision.sh — prepare a machine to run issuewright for one or more repos.
#
# Idempotent. Dry-run by default; pass --apply to change anything.
#
#   sudo ./deploy/provision.sh --apply --slug you/repo --bot 'you-bot[bot]'
#   sudo ./deploy/provision.sh --apply --slug you/repo --bot bot --instance repo
#
# What it does NOT do, on purpose (each needs a human at a keyboard):
#   * `gh auth login` as the bot identity      (or drop a token into agent.env)
#   * `pi auth login` for the brain's provider
#   * creating the GitHub App / bot account
#   * creating the agent's clone of a repo     (printed as the next command)
set -euo pipefail

# --- defaults --------------------------------------------------------------
APPLY=0
PREFIX=/usr/local/bin
ROOT=/srv/agent
AGENT_USER=agent
AGENT_GROUP=agent
VERSION="${ISSUEWRIGHT_VERSION:-v0.1.0}"
SLUG=""
BOT=""
REPO_NAME=""
INSTANCE=""
LOCAL_BIN=""
SKIP_TOOLCHAINS=0
DOCKER_GROUP=1
COPY_PI_AUTH_FROM=""
CLONE=1
GH_APP_ID=""
GH_APP_KEY=""
SET_BOT=""
WEB=0

usage() {
	cat <<'USAGE'
provision.sh [--apply] [options]

  --apply                 actually change the machine (default: dry run)
  --slug owner/repo       repo the agent will work on (required with --apply)
  --bot LOGIN             the login issues are assigned to (required with --apply)
  --name NAME             instance name / agent clone directory (default: repo name)
  --instance NAME         also enable issuewright@NAME.timer
  --version TAG           issuewright release to install (default: v0.1.0)
  --prefix DIR            install the binary here (default: /usr/local/bin)
  --root DIR              state root (default: /srv/agent)
  --local-bin PATH        use this binary instead of downloading a release
  --skip-toolchains       do not install pi/opencode for the agent user
  --copy-pi-auth-from USER
                          copy that user's Pi credentials and settings to the
                          agent user (explicit: it copies a secret)
  --gh-app-id N           GitHub App ID: install the token minter + its timer
  --gh-app-key PATH       the App's private key, already on this machine
  --set-bot LOGIN         rewrite the bot login in an existing config.toml
  --web                   install and start the read-only status page
  --no-clone              do not create the agent's clone of the repo
  --no-docker-group       do not add the agent user to the docker group
  -h, --help              this text

Exit codes: 0 ok, 1 usage/precondition error, 2 a step failed.
USAGE
}

die() { echo "provision: $*" >&2; exit 2; }
note() { printf '  %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

run() {
	if [ "$APPLY" = 1 ]; then
		"$@"
	else
		printf '  [dry-run] %s\n' "$*"
	fi
}

# write_file PATH MODE <<<"content" — never overwrites an existing file.
write_file() {
	local path=$1 mode=$2 content
	content=$(cat)
	if [ -e "$path" ]; then
		note "exists, left alone: $path"
		return 0
	fi
	run mkdir -p "$(dirname "$path")"
	if [ "$APPLY" = 1 ]; then
		printf '%s' "$content" >"$path"
		chmod "$mode" "$path"
		note "wrote $path ($mode)"
	else
		note "[dry-run] would write $path ($mode):"
		printf '%s\n' "$content" | sed 's/^/      /'
	fi
}

# --- arguments -------------------------------------------------------------
while [ $# -gt 0 ]; do
	case "$1" in
	--apply) APPLY=1 ;;
	--slug) SLUG=${2:?--slug needs a value}; shift ;;
	--bot) BOT=${2:?--bot needs a value}; shift ;;
	--name) REPO_NAME=${2:?--name needs a value}; shift ;;
	--instance) INSTANCE=${2:?--instance needs a value}; shift ;;
	--version) VERSION=${2:?--version needs a value}; shift ;;
	--prefix) PREFIX=${2:?--prefix needs a value}; shift ;;
	--root) ROOT=${2:?--root needs a value}; shift ;;
	--local-bin) LOCAL_BIN=${2:?--local-bin needs a value}; shift ;;
	--skip-toolchains) SKIP_TOOLCHAINS=1 ;;
	--copy-pi-auth-from) COPY_PI_AUTH_FROM=${2:?--copy-pi-auth-from needs a user}; shift ;;
	--gh-app-id) GH_APP_ID=${2:?--gh-app-id needs a value}; shift ;;
	--gh-app-key) GH_APP_KEY=${2:?--gh-app-key needs a value}; shift ;;
	--set-bot) SET_BOT=${2:?--set-bot needs a login}; shift ;;
	--web) WEB=1 ;;
	--no-clone) CLONE=0 ;;
	--no-docker-group) DOCKER_GROUP=0 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "provision: unknown argument $1" >&2; usage; exit 1 ;;
	esac
	shift
done

if [ -z "$REPO_NAME" ] && [ -n "$SLUG" ]; then
	REPO_NAME=${SLUG##*/}
fi
if [ -z "$INSTANCE" ] && [ -n "$REPO_NAME" ]; then
	INSTANCE=$REPO_NAME
fi

# --- preflight -------------------------------------------------------------
step "preflight"
[ "$(id -u)" = 0 ] || [ "$APPLY" = 0 ] || die "run with sudo (or pass no --apply for a dry run)"
command -v systemctl >/dev/null || die "systemd is required"
command -v curl >/dev/null || note "curl missing: the release download will fail"
note "mode: $([ "$APPLY" = 1 ] && echo APPLY || echo 'dry run')"
note "version: $VERSION"
note "root: $ROOT"
[ -n "$SLUG" ] && note "repo: $SLUG (bot $BOT)" || note "repo: none given — layout only"

if [ "$APPLY" = 1 ] && [ -z "$SLUG" ]; then
	die "--slug is required with --apply (it goes into config.toml)"
fi
if [ "$APPLY" = 1 ] && [ -z "$BOT" ]; then
	die "--bot is required with --apply"
fi

# --- user and layout -------------------------------------------------------
step "service user"
if id "$AGENT_USER" >/dev/null 2>&1; then
	note "user exists: $AGENT_USER"
else
	run useradd --system --create-home --shell /bin/bash "$AGENT_USER"
	note "create system user $AGENT_USER"
fi

if [ "$DOCKER_GROUP" = 1 ] && getent group docker >/dev/null; then
	if id -nG "$AGENT_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
		note "already in the docker group (needed by `make` targets that use compose)"
	else
		run usermod -aG docker "$AGENT_USER"
		note "add $AGENT_USER to the docker group"
	fi
fi

step "layout under $ROOT"
for dir in "$ROOT" "$ROOT/repos" "$ROOT/work" "$ROOT/logs" "$ROOT/state"; do
	run install -d -m 755 -o "$AGENT_USER" -g "$AGENT_GROUP" "$dir"
done
note "repos/ work/ logs/ state/"

# --- binary ----------------------------------------------------------------
step "binary"
if [ -n "$LOCAL_BIN" ]; then
	[ -f "$LOCAL_BIN" ] || die "no such binary: $LOCAL_BIN"
	run install -m 755 "$LOCAL_BIN" "$PREFIX/issuewright"
else
	arch=$(uname -m)
	case "$arch" in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $arch" ;;
	esac
	url="https://github.com/satheeshds/issuewright/releases/download/${VERSION}/issuewright-linux-${arch}"
	note "download $url"
	if [ "$APPLY" = 1 ]; then
		tmp=$(mktemp)
		curl -fsSL "$url" -o "$tmp" || die "download failed: $url"
		install -m 755 "$tmp" "$PREFIX/issuewright"
		rm -f "$tmp"
	else
		note "[dry-run] would download and install to $PREFIX/issuewright"
	fi
fi
if [ "$APPLY" = 1 ]; then
	"$PREFIX/issuewright" version || die "the installed binary does not run"
fi

# --- systemd units ---------------------------------------------------------
step "systemd units"
if [ "$APPLY" = 1 ]; then
	"$PREFIX/issuewright" install --prefix "$PREFIX" >/dev/null
	systemctl daemon-reload
	note "installed issuewright@.service and issuewright@.timer"
else
	note "[dry-run] issuewright install --prefix $PREFIX && systemctl daemon-reload"
fi

# --- shell PATH for the service user ---------------------------------------
step "shell PATH for $AGENT_USER"
agent_home_profile=$(getent passwd "$AGENT_USER" | cut -d: -f6 || true)
[ -n "$agent_home_profile" ] || agent_home_profile=/home/$AGENT_USER
marker="# issuewright: toolchain PATH"
if [ -f "$agent_home_profile/.bashrc" ] && grep -q "$marker" "$agent_home_profile/.bashrc" 2>/dev/null; then
	note "already present: $agent_home_profile/.bashrc"
else
	if [ "$APPLY" = 1 ]; then
		printf '\n%s\nexport PATH=%s/bin:$HOME/.local/bin:$PATH\n' "$marker" "$ROOT" \
			>>"$agent_home_profile/.bashrc"
		chown "$AGENT_USER:$AGENT_GROUP" "$agent_home_profile/.bashrc"
		note "added $ROOT/bin to $agent_home_profile/.bashrc"
	else
		note "[dry-run] appended $ROOT/bin to $agent_home_profile/.bashrc"
	fi
fi

# --- toolchain PATH --------------------------------------------------------
# The unit runs with a minimal PATH, and agent.env's PATH wins over the unit's.
# Build it from where the tools actually are, so `go test` / `flutter test` in a
# check do not fail with 127.
agent_home_tc=$(getent passwd "$AGENT_USER" | cut -d: -f6 || true)
[ -n "$agent_home_tc" ] || agent_home_tc=/home/$AGENT_USER
tool_dirs=""
for tool in go gofmt flutter dart node npm pi opencode gh; do
	bin=$(command -v "$tool" 2>/dev/null || true)
	[ -n "$bin" ] && tool_dirs="$tool_dirs:$(dirname "$bin")"
done
# Well-known locations that a root shell's PATH would not reveal. Only existing
# directories are added, and only ones the agent user can actually read.
for dir in /usr/local/go/bin /usr/local/bin "$ROOT/bin" "$agent_home_tc/.local/bin" /opt/flutter/bin /snap/bin; do
	[ -d "$dir" ] || continue
	if [ "$APPLY" = 1 ] && ! sudo -u "$AGENT_USER" test -r "$dir"; then
		note "WARNING: $dir exists but $AGENT_USER cannot read it; checks that need it will fail"
		continue
	fi
	tool_dirs="$tool_dirs:$dir"
done
TOOLCHAIN_PATH=$(printf '%s' "/usr/local/bin:/usr/bin:/bin:$ROOT/bin$tool_dirs" |
	tr ':' '\n' | awk '!seen[$0]++' | paste -sd: -)
note "PATH for the agent: $TOOLCHAIN_PATH"

# --- configuration ---------------------------------------------------------
step "configuration"
write_file "$ROOT/config.toml" 644 <<EOF
# Written by deploy/provision.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
# One [[repo]] block per repo. Repo-specific rules live in that repo's
# .agent/agent.yaml, never here.
[agent]
workroot = "$ROOT/work"
logdir   = "$ROOT/logs"
repos_root = "$ROOT/repos"
brain    = "pi"
thinking = "high"
max_run_minutes = 90
poll_minutes    = 10

[[repo]]
name      = "${REPO_NAME:-CHANGEME}"
path      = "$ROOT/repos/${REPO_NAME:-CHANGEME}"
slug      = "${SLUG:-owner/CHANGEME}"
bot       = "${BOT:-CHANGEME}"
token_env = "GH_TOKEN"
EOF

write_file "$ROOT/agent.env" 600 <<'EOF'
# Loaded by issuewright@<instance>.service. Mode 0600, owned by agent.
# Set ONE of these: a GitHub App installation token, or a fine-grained PAT for a
# bot account. Without a token the agent uses the machine's `gh auth login`.
# GH_TOKEN=github_pat_...

# systemd units get a minimal PATH. Add wherever go/flutter live, or every check
# that needs them fails with "not found".
EOF
if [ "$APPLY" = 1 ] && [ -f "$ROOT/agent.env" ]; then
	{
		tmp=$(mktemp)
		if grep -qE '^PATH=' "$ROOT/agent.env"; then
			sed "s|^PATH=.*|PATH=$TOOLCHAIN_PATH|" "$ROOT/agent.env" >"$tmp"
		else
			cat "$ROOT/agent.env" >"$tmp"
			printf 'PATH=%s\n' "$TOOLCHAIN_PATH" >>"$tmp"
		fi
		cat "$tmp" >"$ROOT/agent.env"
		rm -f "$tmp"
		note "PATH set in $ROOT/agent.env"
	}
else
	note "[dry-run] would set PATH=$TOOLCHAIN_PATH in $ROOT/agent.env"
fi
cat >/dev/null <<'EOF'

# Only for the exec brain:
# AGENT_EXEC_COMMAND='mytool --dir {{WORKDIR}} --task {{PROMPT_FILE}}'
EOF
if [ "$APPLY" = 1 ]; then
	chown "$AGENT_USER:$AGENT_GROUP" "$ROOT/agent.env" "$ROOT/config.toml"
fi

# --- update the bot login in place ----------------------------------------
if [ -n "$SET_BOT" ] && [ -f "$ROOT/config.toml" ]; then
	step "bot login"
	if [ "$APPLY" = 1 ]; then
		sed -i "s|^bot[[:space:]]*=.*|bot       = \"$SET_BOT\"|" "$ROOT/config.toml"
		note "bot = \"$SET_BOT\""
	else
		note "[dry-run] set bot = \"$SET_BOT\" in $ROOT/config.toml"
	fi
fi

# --- GitHub App token minter ----------------------------------------------
if [ -n "$GH_APP_ID" ]; then
	step "GitHub App token"
	[ -n "$GH_APP_KEY" ] || die "--gh-app-id needs --gh-app-key" 1
	[ -f "$GH_APP_KEY" ] || die "no private key at $GH_APP_KEY" 1
	[ -n "$SLUG" ] || die "--gh-app-id needs --slug" 1

	# The minting script is a sibling of this one in the release tree; fetch it at
	# the same tag so both are the same version.
	script_url="https://raw.githubusercontent.com/satheeshds/issuewright/$VERSION/deploy/gh-app-token.sh"
	note "install $ROOT/bin/gh-app-token.sh"
	if [ "$APPLY" = 1 ]; then
		curl -fsSL "$script_url" -o "$ROOT/bin/gh-app-token.sh" ||
			die "could not download $script_url" 2
		chmod 755 "$ROOT/bin/gh-app-token.sh"
		install -m 600 -o root -g root "$GH_APP_KEY" "$ROOT/gh-app.pem"
	fi

	write_file "$ROOT/gh-app.env" 600 <<EOF2
# Read by issuewright-token.service (root). App installation tokens live 1 hour;
# the timer mints a fresh one every 30 minutes into GH_TOKEN_FILE.
GH_APP_ID=$GH_APP_ID
GH_APP_KEY=$ROOT/gh-app.pem
GH_REPO=$SLUG
GH_TOKEN_FILE=$ROOT/token.env
GH_TOKEN_OWNER=$AGENT_USER
EOF2

	note "mint one token now"
	if [ "$APPLY" = 1 ]; then
		"$ROOT/bin/gh-app-token.sh" || note "WARNING: minting failed; check the App ID, key and installation"
		systemctl daemon-reload
		systemctl enable --now issuewright-token.timer
		note "issuewright-token.timer enabled (every 30 minutes)"
	else
		note "[dry-run] $ROOT/bin/gh-app-token.sh && systemctl enable --now issuewright-token.timer"
	fi
fi

# --- toolchains ------------------------------------------------------------
if [ "$SKIP_TOOLCHAINS" = 1 ]; then
	step "toolchains skipped"
else
	step "toolchains for the $AGENT_USER user"
	# Host-level tools the agent's checks call. These are multi-GB or distro
	# packages, so they are reported rather than installed behind your back.
	for tool in go flutter docker gh jq node npm; do
		if command -v "$tool" >/dev/null; then
			note "host ok: $tool ($(command -v "$tool"))"
		else
			note "host MISSING: $tool — the checks that need it will fail"
		fi
	done

	# The brains are npm packages: install them for the service user, into a
	# prefix the unit's PATH already covers ($ROOT/bin).
	if command -v npm >/dev/null; then
		agent_home=$(getent passwd "$AGENT_USER" | cut -d: -f6 || true)
		[ -n "$agent_home" ] || agent_home=/home/$AGENT_USER
		for pkg in "@earendil-works/pi-coding-agent" "opencode-ai"; do
			if [ -x "$ROOT/bin/${pkg##*/}" ] || [ -x "$agent_home/.local/bin/${pkg##*/}" ]; then
				note "already installed: ${pkg##*/}"
				continue
			fi
			note "install $pkg for $AGENT_USER (npm -g --prefix $ROOT)"
			if [ "$APPLY" = 1 ]; then
				sudo -u "$AGENT_USER" env HOME="$agent_home" npm install -g 					--prefix "$ROOT" --allow-scripts=opencode-ai --allow-scripts=@google/genai,protobufjs 					"$pkg" || note "WARNING: installing $pkg failed; install it by hand"
			else
				note "[dry-run] sudo -u $AGENT_USER npm install -g --prefix $ROOT $pkg"
			fi
		done
		# The provider extension lives in the user's Pi config, not the prefix.
		if [ "$APPLY" = 1 ]; then
			if sudo -u "$AGENT_USER" env HOME="$agent_home" PATH="$ROOT/bin:$PATH" \
				pi --version >/dev/null 2>&1; then
				note "verified: pi runs as $AGENT_USER"
			else
				note "WARNING: pi does not run as $AGENT_USER; check $ROOT/bin"
			fi
		fi
		note "add the opencode-go provider to $AGENT_USER's Pi config"
		if [ "$APPLY" = 1 ]; then
			sudo -u "$AGENT_USER" env HOME="$agent_home" PATH="$ROOT/bin:$PATH" \
				pi install https://github.com/monotykamary/pi-opencode-go-provider ||
				note "WARNING: provider install failed (needs network + npm)"
		else
			note "[dry-run] sudo -u $AGENT_USER pi install https://github.com/monotykamary/pi-opencode-go-provider"
		fi
	else
		note "npm missing: cannot install the brains; install node/npm first"
	fi
fi

# --- Pi credentials for the agent user -------------------------------------
if [ -n "$COPY_PI_AUTH_FROM" ]; then
	step "Pi credentials from $COPY_PI_AUTH_FROM"
	src_home=$(getent passwd "$COPY_PI_AUTH_FROM" | cut -d: -f6 || true)
	[ -n "$src_home" ] || die "no such user: $COPY_PI_AUTH_FROM"
	agent_home=$(getent passwd "$AGENT_USER" | cut -d: -f6 || true)
	[ -n "$agent_home" ] || agent_home=/home/$AGENT_USER
	note "copying $src_home/.pi/agent/{auth.json,settings.json,git,npm} → $agent_home/.pi/agent"
	note "this hands the $AGENT_USER user your model-subscription credentials (0600)"
	if [ "$APPLY" = 1 ]; then
		sudo -u root install -d -m 700 -o "$AGENT_USER" -g "$AGENT_GROUP" "$agent_home/.pi/agent"
		for item in auth.json settings.json git npm; do
			[ -e "$src_home/.pi/agent/$item" ] || continue
			cp -a "$src_home/.pi/agent/$item" "$agent_home/.pi/agent/"
		done
		chown -R "$AGENT_USER:$AGENT_GROUP" "$agent_home/.pi/agent"
		chmod 600 "$agent_home/.pi/agent/auth.json" 2>/dev/null || true
		note "copied; revoke by deleting $agent_home/.pi/agent/auth.json"
	else
		note "[dry-run] would copy those files and chown them to $AGENT_USER"
	fi
else
	step "Pi credentials"
	note "none copied. Give the brain credentials one of two ways:"
	note "  sudo -u $AGENT_USER pi auth login                       (interactive)"
	note "  --copy-pi-auth-from \$USER                              (copy yours)"
fi

# --- clone -----------------------------------------------------------------
step "agent clone"
clone="$ROOT/repos/${REPO_NAME:-CHANGEME}"
token=""
if [ -f "$ROOT/agent.env" ]; then
	# Read GH_TOKEN from agent.env when the operator put one there; never printed.
	token=$(grep -E '^GH_TOKEN=' "$ROOT/agent.env" 2>/dev/null | tail -1 | cut -d= -f2- || true)
fi
if [ -d "$clone/.git" ]; then
	note "clone exists: $clone"
elif [ "$CLONE" = 0 ]; then
	note "skipped (--no-clone). The agent must never use a deploy checkout."
elif [ -z "$SLUG" ]; then
	note "no --slug given; not cloning"
elif [ -z "$token" ]; then
	note "no GH_TOKEN in $ROOT/agent.env, so the clone needs credentials first:"
	note "  sudo -u $AGENT_USER gh auth login && sudo -u $AGENT_USER git clone https://github.com/$SLUG.git $clone"
else
	agent_home=$(getent passwd "$AGENT_USER" | cut -d: -f6 || true)
	[ -n "$agent_home" ] || agent_home=/home/$AGENT_USER
	note "clone https://github.com/$SLUG.git as $AGENT_USER (using the token in agent.env, not printed)"
	if [ "$APPLY" = 1 ]; then
		sudo -u "$AGENT_USER" env HOME="$agent_home" GH_TOKEN="$token" 			git clone --quiet "https://github.com/$SLUG.git" "$clone" &&
			note "cloned $clone" ||
			note "WARNING: clone failed — check the token's repo access"
		# Make later fetches/pushes use the same token.
		sudo -u "$AGENT_USER" env HOME="$agent_home" 			git config --global "credential.https://github.com.helper" \
			'!/usr/bin/gh auth git-credential' 2>/dev/null || true
	else
		note "[dry-run] would clone into $clone"
	fi
fi

# --- status page -----------------------------------------------------------
if [ "$WEB" = 1 ]; then
	step "status page"
	if [ "$APPLY" = 1 ]; then
		systemctl enable --now issuewright-web.service
		note "issuewright-web.service on http://127.0.0.1:3040 (AGENT_WEB_BIND overrides)"
		note "publish it with one of:"
		note "  tailscale serve --bg --set-path /agent http://127.0.0.1:3040"
		note "  ssh -L 3040:localhost:3040 <box>            (then http://localhost:3040)"
		note "  issuewright serve --bind 0.0.0.0:3040       (LAN only)"
	else
		note "[dry-run] systemctl enable --now issuewright-web.service"
	fi
fi

# --- timer -----------------------------------------------------------------
step "timer"
if [ -n "$INSTANCE" ] && [ "$INSTANCE" != CHANGEME ]; then
	if [ "$APPLY" = 1 ]; then
		systemctl enable --now "issuewright@$INSTANCE.timer"
		systemctl list-timers "issuewright@$INSTANCE.timer" --no-pager | head -3
	else
		note "[dry-run] systemctl enable --now issuewright@$INSTANCE.timer"
	fi
else
	note "next: systemctl enable --now 'issuewright@<name>.timer'"
fi

# --- what a human must still do -------------------------------------------
cat <<EOF

== still yours to do
  1. bot identity
       a GitHub App (issues/contents/pull_requests: write, metadata: read),
       installed on ${SLUG:-owner/repo}; or a PAT for a bot account.
       Then, as $AGENT_USER:
         sudo -u $AGENT_USER -H gh auth login     # or put GH_TOKEN in $ROOT/agent.env
       (with GH_TOKEN in agent.env, re-running this script clones the repo)
       The login must match the bot field in $ROOT/config.toml.
  2. brain credentials
         sudo -u $AGENT_USER -H env PATH=$ROOT/bin:/usr/local/bin:/usr/bin:/bin pi auth login
       (plain "sudo -u $AGENT_USER pi" fails: sudo's secure_path does not include
        $ROOT/bin. "sudo -u $AGENT_USER -i" works too, now that .bashrc has it.)
       Or copy yours instead of logging in again:
         --copy-pi-auth-from <your-user>
       Without this, every run blocks with an authentication error (which is
       the intended behaviour: a brain that cannot think must not fake success).
  3. the clone, if it is not there yet (printed above).
  4. first run, by hand, before trusting the timer:
         sudo -u $AGENT_USER issuewright run --repo ${REPO_NAME:-CHANGEME} --issue <n> --dry-run --verbose
         sudo -u $AGENT_USER issuewright run --repo ${REPO_NAME:-CHANGEME} --issue <n>
  5. the suspend window: this box sleeps 21:30-08:00. The timer is
     Persistent=true, so a missed tick fires on wake; a run killed mid-flight
     resumes with \`pi -c\` on the next tick.
EOF

step "done"
if [ "$APPLY" = 0 ]; then
	echo "  dry run only — re-run with --apply to make these changes"
fi
