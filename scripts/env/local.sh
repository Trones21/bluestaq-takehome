#!/usr/bin/env bash
# Local development environment. SOURCE this, do not execute it:
#
#   source scripts/env/local.sh
#
# These values are for the docker-compose stack on your machine. They are
# committed on purpose -- there is nothing secret about a throwaway local
# database. Production values never live in this repository; see prod.sh.

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  echo "This script sets variables in your shell, so it must be sourced:"
  echo "  source ${0}"
  exit 1
fi

export ENV=development

# Ports 8080 (API) and 9090 (admin: metrics, health) must differ -- the service
# refuses to start otherwise, so /metrics can never end up publicly routed.
export PORT=8080
export ADMIN_PORT=9090

# 5433 on the host, so the compose Postgres does not collide with a local one.
export DATABASE_URL="postgres://notes:notes@localhost:5433/notes?sslmode=disable"
export DB_MAX_CONNS=10

# Bounds how long a request may spend in handler code. DB_MAX_CONNS caps how
# many queries run at once; this caps how long the requests queued behind them
# are willing to wait. Must stay below the 60s write timeout.
export REQUEST_TIMEOUT=15s

# Enforced by Postgres on every API connection, not by the Go process --
# cancelling a context breaks our end of the socket, but the backend keeps
# working until it tries to write. STATEMENT_TIMEOUT bounds one running
# statement; IDLE_IN_TX_TIMEOUT bounds a transaction left open executing
# nothing, which no statement timeout can catch. cmd/sweep is unaffected: it
# builds its own pool straight from DATABASE_URL.
export STATEMENT_TIMEOUT=3s
export IDLE_IN_TX_TIMEOUT=10s

# Local only. Production reads this from pass; see generate_dot_env_prod.sh.
# Must be at least 32 bytes or startup fails.
export JWT_SECRET="local-development-only-secret-do-not-use-in-prod"
export JWT_TTL=24h

export LOG_LEVEL=debug

# Swagger UI runs on the API itself at /docs, so the only browser origin worth
# allowing locally is a future SPA dev server.
export CORS_ALLOWED_ORIGINS="http://localhost:5173"

# MinIO from docker-compose. Path-style addressing is required: MinIO does not
# do virtual-host buckets, real S3 does.
export S3_ENDPOINT="http://localhost:9000"
export S3_REGION=us-east-1
export S3_BUCKET=notes-attachments
export S3_ACCESS_KEY=minioadmin
export S3_SECRET_KEY=minioadmin
export S3_PATH_STYLE=true
export S3_UPLOAD_TTL=15m
export S3_DOWNLOAD_TTL=1h
export MAX_ATTACHMENT_MB=100

echo "✅ local env loaded — API :$PORT, admin :$ADMIN_PORT, db $DATABASE_URL"
