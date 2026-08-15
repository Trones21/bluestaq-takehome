#!/usr/bin/env bash
# Shared configuration and helpers. Sourced by the other scripts, never run.

set -euo pipefail

# Repository root, regardless of where the caller invoked from.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export REPO_ROOT

# Deploy target. Override per invocation:  SERVER=notes@1.2.3.4 ./scripts/deploy.sh
SERVER="${SERVER:-}"
REMOTE_PATH="${REMOTE_PATH:-~/transferred/notes}"
IMAGE_NAME="${IMAGE_NAME:-notes-api:latest}"
IMAGE_TAR="${IMAGE_TAR:-notes-api.tar}"

# Local stack (docker-compose.yml)
DEV_DB_URL="${DEV_DB_URL:-postgres://notes:notes@localhost:5433/notes?sslmode=disable}"
TEST_DB_URL="${TEST_DB_URL:-postgres://notes:notes@localhost:5433/notes_test?sslmode=disable}"

# --- output -----------------------------------------------------------------

info()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
ok()    { printf '\033[32m✅ %s\033[0m\n' "$*"; }
warn()  { printf '\033[33m⚠️  %s\033[0m\n' "$*"; }
die()   { printf '\033[31m❌ %s\033[0m\n' "$*" >&2; exit 1; }

# --- guards -----------------------------------------------------------------

require_cmd() {
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required but not installed"
  done
}

require_var() {
  for name in "$@"; do
    [ -n "${!name:-}" ] || die "$name is required (set it in the environment or scripts/env/prod.sh)"
  done
}

# confirm prompts before something irreversible. Answering anything but "yes"
# aborts -- a bare newline must never mean "deploy to production".
confirm() {
  local prompt="${1:-Continue?}"
  if [ "${ASSUME_YES:-0}" = "1" ]; then
    warn "ASSUME_YES=1, skipping confirmation: $prompt"
    return 0
  fi
  printf '\n\033[33m%s\033[0m [type "yes" to proceed] ' "$prompt"
  read -r reply
  [ "$reply" = "yes" ] || die "aborted"
}

# wait_for_http polls a URL until it answers or the timeout expires.
wait_for_http() {
  local url="$1" timeout="${2:-30}" elapsed=0
  while ! curl -sf --max-time 2 "$url" >/dev/null 2>&1; do
    sleep 1
    elapsed=$((elapsed + 1))
    [ "$elapsed" -lt "$timeout" ] || return 1
  done
  return 0
}
