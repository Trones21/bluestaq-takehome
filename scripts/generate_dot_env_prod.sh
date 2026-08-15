#!/usr/bin/env bash
# Render the production environment into .env_prod for the container.
#
#   ./scripts/generate_dot_env_prod.sh
#
# Why a file rather than build args or a baked-in layer: build args land in the
# image history, where anyone who can pull the image can read them. An env file
# supplied at `docker compose up` exists only while the container runs.
#
# Reads ~/env_setup/notes/prod.sh (see scripts/env/prod.sh.example), which
# pulls secrets from pass. The rendered file is gitignored and deleted by
# deploy.sh once transferred.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

ENV_FILE="${PROD_ENV_FILE:-$HOME/env_setup/notes/prod.sh}"
OUT="$REPO_ROOT/.env_prod"

[ -f "$ENV_FILE" ] || die "no production env at $ENV_FILE
  Create it from the template:
    mkdir -p ~/env_setup/notes
    cp scripts/env/prod.sh.example ~/env_setup/notes/prod.sh
    chmod 600 ~/env_setup/notes/prod.sh"

info "sourcing $ENV_FILE"
# shellcheck disable=SC1090
source "$ENV_FILE"

require_var ENV PORT ADMIN_PORT DATABASE_URL JWT_SECRET S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY

# Explicit allowlist rather than dumping the whole environment: `env` would
# otherwise carry SSH_AUTH_SOCK, AWS session tokens, and whatever else is in
# the deploying shell into a file that ships to the server.
VARS=(
  ENV PORT ADMIN_PORT
  DATABASE_URL DB_MAX_CONNS REQUEST_TIMEOUT
  STATEMENT_TIMEOUT IDLE_IN_TX_TIMEOUT
  JWT_SECRET JWT_TTL
  LOG_LEVEL CORS_ALLOWED_ORIGINS
  S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY S3_PATH_STYLE
  S3_UPLOAD_TTL S3_DOWNLOAD_TTL MAX_ATTACHMENT_MB
)

# Written 600 before anything is in it, so the secrets are never briefly
# world-readable between creation and chmod.
: > "$OUT"
chmod 600 "$OUT"

for name in "${VARS[@]}"; do
  value="${!name:-}"
  [ -n "$value" ] || continue
  printf '%s=%s\n' "$name" "$value" >> "$OUT"
done

ok "wrote $OUT ($(wc -l < "$OUT") variables, mode 600)"
warn "contains live secrets — deploy.sh removes it after transfer"
