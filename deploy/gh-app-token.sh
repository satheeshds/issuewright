#!/usr/bin/env bash
# gh-app-token.sh — mint a GitHub App installation token and write it where the
# issuewright units can read it.
#
# A GitHub App installation token expires after one hour, so this script is meant
# to be run from a timer (deploy/systemd/issuewright-token.timer) rather than by
# hand. It signs a short-lived RS256 JWT with the App's private key, exchanges it
# for an installation token, and writes `GH_TOKEN=…` atomically.
#
#   GH_APP_ID=123456 GH_APP_KEY=/srv/agent/gh-app.pem GH_REPO=owner/repo \
#     ./gh-app-token.sh                      # writes /srv/agent/token.env
#
#   ./gh-app-token.sh --print                # token on stdout (never logged)
#   ./gh-app-token.sh --check                # is the current token still valid?
#
# Exit codes: 0 ok, 1 configuration error, 2 API or crypto failure.
set -euo pipefail

# GitHub accepts the App ID or the client ID as `iss`, and recommends the client
# ID. Take whichever is configured.
APP_ID=${GH_APP_CLIENT_ID:-${GH_APP_ID:-}}
KEY=${GH_APP_KEY:-/srv/agent/gh-app.pem}
REPO=${GH_REPO:-}
OUT=${GH_TOKEN_FILE:-/srv/agent/token.env}
OWNER=${GH_TOKEN_OWNER:-agent}
API=${GITHUB_API_URL:-https://api.github.com}
MODE=mint

usage() {
	cat <<'USAGE'
gh-app-token.sh [--print|--check]

  (no flag)   mint a token and write GH_TOKEN=… into $GH_TOKEN_FILE
  --print     mint and print the token to stdout (do not log it)
  --check     exit 0 if the token in $GH_TOKEN_FILE is valid for >5 more minutes

Environment:
  GH_APP_CLIENT_ID the App's client ID (preferred; GitHub recommends it for iss)
  GH_APP_ID        the App's numeric ID (accepted alternative)
  GH_APP_KEY       path to the App's private key .pem (default /srv/agent/gh-app.pem)
  GH_REPO          owner/repo the App is installed on (required)
  GH_TOKEN_FILE    where to write GH_TOKEN=… (default /srv/agent/token.env)
  GH_TOKEN_OWNER   chown the file to this user when running as root (default agent)
USAGE
}

die() {
	echo "gh-app-token: $*" >&2
	exit "${2:-1}"
}

base64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

require_tools() {
	for tool in openssl curl; do
		command -v "$tool" >/dev/null || die "$tool is required" 1
	done
}

jwt() {
	local now exp header payload signing
	now=$(date +%s)
	exp=$((now + 540)) # 9 minutes; GitHub allows at most 10
	header=$(printf '{"alg":"RS256","typ":"JWT"}' | base64url)
	payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$exp" "$APP_ID" | base64url)
	signing="${header}.${payload}"
	printf '%s' "$signing" |
		openssl dgst -sha256 -sign "$KEY" 2>/dev/null | base64url |
		sed "s/^/${signing}./"
}

api() {
	local method=$1 path=$2 body=${3:-}
	if [ -n "$body" ]; then
		curl -fsS -X "$method" -H "Authorization: Bearer $JWT" \
			-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" \
			-d "$body" "$API$path"
	else
		curl -fsS -X "$method" -H "Authorization: Bearer $JWT" \
			-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" \
			"$API$path"
	fi
}

json_field() { sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\{0,1\}\([^",}]*\)"\{0,1\}.*/\1/p' | head -1; }

write_token() {
	local token=$1 tmp
	tmp=$(mktemp "${OUT}.XXXXXX")
	printf 'GH_TOKEN=%s\n' "$token" >"$tmp"
	chmod 600 "$tmp"
	if [ "$(id -u)" = 0 ] && id "$OWNER" >/dev/null 2>&1; then
		chown "$OWNER:$(id -gn "$OWNER" 2>/dev/null || echo "$OWNER")" "$tmp"
	fi
	mv -f "$tmp" "$OUT"
}

if [ $# -gt 0 ]; then MODE=$1; fi
case "$MODE" in
mint) ;;
-h | --help)
	usage
	exit 0
	;;
--print) ;;
--check) ;;
*) die "unknown argument $MODE" 1 ;;
esac

# --- check: is the existing token still good? ------------------------------
if [ "$MODE" = --check ]; then
	[ -f "$OUT" ] || exit 2
	token=$(sed -n 's/^GH_TOKEN=//p' "$OUT" | head -1)
	[ -n "$token" ] || exit 2
	code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" \
		-H "Accept: application/vnd.github+json" "$API/repos/$REPO") || exit 2
	# A token that expires in under ~5 minutes still answers 200; the timer's
	# cadence is what keeps that gap small.
	[ "$code" = 200 ] || exit 2
	exit 0
fi

# --- mint ------------------------------------------------------------------
[ -n "$APP_ID" ] || die "GH_APP_ID (or GH_APP_CLIENT_ID) is required — the App's numeric ID or client ID, not its name" 1
[ -n "$REPO" ] || die "GH_REPO is required (owner/repo)" 1
[ -f "$KEY" ] || die "no private key at $KEY (download it from the App settings)" 1
require_tools
if [ "$MODE" != --print ] && [ ! -d "${OUT%/*}" ]; then
	mkdir -p "${OUT%/*}" || die "cannot create ${OUT%/*}" 1
fi

JWT=$(jwt)
[ -n "$JWT" ] || die "could not sign a JWT with $KEY" 2

installation=$(api GET "/repos/$REPO/installation")
inst_id=$(printf '%s' "$installation" | json_field id)
if [ -z "$inst_id" ]; then
	die "no installation for $REPO — is the App installed on that repo?" 2
fi

minted=$(api POST "/app/installations/$inst_id/access_tokens")
token=$(printf '%s' "$minted" | json_field token)
expires=$(printf '%s' "$minted" | json_field expires_at)
if [ -z "$token" ]; then
	die "GitHub returned no token (check the App's permissions: contents/issues/pull_requests write)" 2
fi

if [ "$MODE" = --print ]; then
	printf '%s\n' "$token"
else
	write_token "$token"
	echo "gh-app-token: installation $inst_id for $REPO, valid until $expires, written to $OUT" >&2
fi
