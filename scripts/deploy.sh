#!/usr/bin/env bash
# Deploy the API to the host.
#
#   ./scripts/deploy.sh                       # full: checks, build, ship, migrate, smoke
#   SKIP_CI=1 ./scripts/deploy.sh             # skip checks (you already ran them)
#   ASSUME_YES=1 ./scripts/deploy.sh          # non-interactive, for a pipeline
#   SERVER=notes@1.2.3.4 ./scripts/deploy.sh  # override the target
#
# Ships the image as a tar over SSH rather than through a registry. For one
# host that is fewer moving parts than running a registry and authenticating to
# it; it stops being the right call the moment there is a second host, at which
# point this becomes a push to ECR and a pull on each box.
#
# Order matters: checks, then build, then transfer, then MIGRATE, then start.
# Migrating before the new container starts is what makes the deploy
# rollback-able -- see the comment at that step.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
cd "$REPO_ROOT"

require_cmd docker ssh scp

# ---------------------------------------------------------------- 1. checks

if [ "${SKIP_CI:-0}" != "1" ]; then
  info "running CI checks"
  ./scripts/ci.sh
  echo
  confirm "Checks passed. Review the output above. Deploy to ${SERVER:-<unset>}?"
else
  warn "SKIP_CI=1 — deploying without running checks"
  confirm "Deploy to ${SERVER:-<unset>} WITHOUT running checks?"
fi

# ------------------------------------------------------------ 2. environment

info "rendering production environment"
./scripts/generate_dot_env_prod.sh

# generate_dot_env_prod.sh sources the prod env, but it does so in a subshell,
# so SERVER and APP_HOST have to be read here too.
ENV_FILE="${PROD_ENV_FILE:-$HOME/env_setup/notes/prod.sh}"
# shellcheck disable=SC1090
source "$ENV_FILE"
require_var SERVER APP_HOST ACME_EMAIL DATABASE_URL

# .env_prod holds live secrets. Remove it on any exit, including a failure
# partway through -- otherwise a botched deploy leaves plaintext credentials in
# the working tree.
cleanup() {
  rm -f "$REPO_ROOT/.env_prod" "$REPO_ROOT/$IMAGE_TAR"
}
trap cleanup EXIT

# ----------------------------------------------------------------- 3. build

info "building $IMAGE_NAME"
docker build -t "$IMAGE_NAME" .

info "saving image to $IMAGE_TAR"
docker save -o "$IMAGE_TAR" "$IMAGE_NAME"
ok "image is $(du -h "$IMAGE_TAR" | cut -f1)"

# -------------------------------------------------------------- 4. transfer

info "preparing $SERVER:$REMOTE_PATH"
ssh "$SERVER" "mkdir -p $REMOTE_PATH/letsencrypt"

info "transferring"
scp \
  "$IMAGE_TAR" \
  .env_prod \
  deploy/compose.yaml \
  deploy/prometheus.yml \
  "$SERVER:$REMOTE_PATH/"

# Substitute the host and ACME email into the compose file on the server, so
# the committed file carries no environment-specific values.
ssh "$SERVER" "cd $REMOTE_PATH && \
  sed -i 's|__APP_HOST__|$APP_HOST|g; s|__ACME_EMAIL__|$ACME_EMAIL|g' compose.yaml"

info "loading image on server"
ssh "$SERVER" "docker load -i $REMOTE_PATH/$IMAGE_TAR && rm -f $REMOTE_PATH/$IMAGE_TAR"

# --------------------------------------------------------------- 5. migrate

# BEFORE the new container starts, and as its own step.
#
# Not on application startup, because with more than one instance N processes
# race for the same database. And not after the container starts, because the
# new code would briefly run against the old schema.
#
# Migrations must be backwards-compatible with the version still serving
# traffic (expand/contract), since both are live during the swap: add a column,
# deploy code that writes both, backfill, deploy code that reads the new one,
# and only then drop the old.
info "applying migrations"
ssh "$SERVER" "docker run --rm --network host --env-file $REMOTE_PATH/.env_prod \
  --entrypoint /migrate $IMAGE_NAME up"
ok "schema up to date"

# ----------------------------------------------------------------- 6. start

info "starting services"
ssh "$SERVER" "cd $REMOTE_PATH && docker compose up -d --remove-orphans"

# The env file lives on the server only as long as the deploy needs it; compose
# has already read it into the container environment.
ssh "$SERVER" "shred -u $REMOTE_PATH/.env_prod 2>/dev/null || rm -f $REMOTE_PATH/.env_prod"

# ----------------------------------------------------------------- 7. verify

info "waiting for readiness"
if ssh "$SERVER" "timeout 60 bash -c 'until curl -sf localhost:9090/readyz >/dev/null; do sleep 2; done'"; then
  ok "service is ready"
else
  die "service did not become ready — check: ssh $SERVER 'cd $REMOTE_PATH && docker compose logs api'"
fi

info "running smoke flows against https://$APP_HOST"
if BASE_URL="https://$APP_HOST" ./scripts/smoke.sh; then
  ok "smoke flows passed"
else
  warn "SMOKE FLOWS FAILED — the deploy is live but misbehaving"
  warn "roll back: ssh $SERVER 'cd $REMOTE_PATH && docker compose down' and redeploy the previous image"
  exit 1
fi

echo
ok "deployed to https://$APP_HOST"
echo "   docs:    https://$APP_HOST/docs"
echo "   logs:    ssh $SERVER 'cd $REMOTE_PATH && docker compose logs -f api'"
echo "   metrics: ssh -L 9090:localhost:9090 $SERVER   then open http://localhost:9090/metrics"
