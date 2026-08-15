#!/usr/bin/env bash
# Exercise a running deployment end to end.
#
#   ./scripts/smoke.sh                                  # against local
#   BASE_URL=https://api.example.com ./scripts/smoke.sh  # against a deployment
#
# Runs the multi-user request flows (see flows/), which drive several
# authenticated clients against each other. That is the only way to check the
# behaviour that matters here: a single client cannot express "Bob cannot read
# Alice's private note".
#
# Safe against a live system: every account and note it creates is namespaced
# with a random suffix and cleaned up at the end.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
cd "$REPO_ROOT"

require_cmd curl

PY="$REPO_ROOT/nocrud/.venv/bin/python"
[ -x "$PY" ] || die "runner venv missing — create it with: make flows-setup"

BASE_URL="${BASE_URL:-http://localhost:8080}"
ADMIN_URL="${ADMIN_URL:-http://localhost:9090}"

info "target $BASE_URL"

# Readiness first, so a service that is simply not up yet reports that rather
# than a wall of confusing flow failures.
if [[ "$ADMIN_URL" == http://localhost:* ]]; then
  if ! wait_for_http "$ADMIN_URL/readyz" 30; then
    die "not ready at $ADMIN_URL/readyz — is the stack running? (make dev-up && make run)"
  fi
  ok "service is ready"
fi

# --serial points the runner at an already-running app instead of provisioning
# one per flow. That is the only correct mode against a deployment: parallel
# mode would try to create databases on the production server.
info "running request flows (serial, against the deployed app)"
cd "$REPO_ROOT/nocrud"
if APP_PORT="${APP_PORT:-8080}" NOCRUD_BASE_URL="$BASE_URL" "$PY" noCRUD.py -req --serial; then
  ok "all flows passed"
else
  die "flows failed against $BASE_URL"
fi
