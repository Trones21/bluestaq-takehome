#!/usr/bin/env bash
# Everything that must pass before a deploy.
#
#   ./scripts/ci.sh
#
# Runs standalone, and deploy.sh runs it first. Kept as a script rather than a
# CI workflow file so the same checks run identically on a laptop and in a
# pipeline -- a workflow that only exists in YAML cannot be run before pushing.
#
# Needs the local stack for the integration tests. REQUIRE_TEST_DATABASE=1
# means an unreachable database fails rather than silently skipping, which is
# the whole point of running this before a deploy.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
cd "$REPO_ROOT"

require_cmd go docker

failures=0
step() {
  local name="$1"; shift
  info "$name"
  if "$@"; then
    ok "$name"
  else
    warn "FAILED: $name"
    failures=$((failures + 1))
  fi
}

info "starting test dependencies"
docker compose up -d postgres minio createbucket >/dev/null 2>&1
until docker compose exec -T postgres pg_isready -U notes -d notes >/dev/null 2>&1; do sleep 1; done
docker compose exec -T postgres psql -U notes -d postgres -tAc \
  "select 1 from pg_database where datname='notes_test'" | grep -q 1 || \
  docker compose exec -T postgres psql -U notes -d postgres -c "create database notes_test" >/dev/null
ok "dependencies ready"

step "gofmt" bash -c '
  unformatted=$(gofmt -l . | grep -v "^internal/store/sqlcgen/" || true)
  if [ -n "$unformatted" ]; then echo "unformatted files:"; echo "$unformatted"; exit 1; fi'

step "go vet" go vet ./...

# Regenerating and diffing catches the case where someone edits a query file
# and forgets to run sqlc, leaving generated code that no longer matches.
if command -v sqlc >/dev/null 2>&1; then
  step "sqlc generate is current" bash -c '
    sqlc generate
    if ! git diff --quiet -- internal/store/sqlcgen; then
      echo "generated code is stale — run: sqlc generate"
      git diff --stat -- internal/store/sqlcgen
      exit 1
    fi'
else
  warn "sqlc not installed, skipping generated-code check"
fi

step "tests" env REQUIRE_TEST_DATABASE=1 TEST_DATABASE_URL="$TEST_DB_URL" go test ./... -count=1

# Build for the deploy target, not the host: a build that only works on macOS
# is not evidence that the Linux image will build.
step "release build" env CGO_ENABLED=0 GOOS=linux go build -trimpath -o /dev/null ./cmd/api
step "migrator build" env CGO_ENABLED=0 GOOS=linux go build -trimpath -o /dev/null ./cmd/migrate

echo
if [ "$failures" -gt 0 ]; then
  die "$failures check(s) failed"
fi
ok "all checks passed"
