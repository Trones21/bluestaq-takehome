from pathlib import Path

# DO NOT ADD A TRAILING SLASH TO ANY OF THESE

# The Go backend. The runner lives inside the repo, so the app is one level up.
APP_DIR = Path(__file__).resolve().parents[1]

# No Django-style fixture files here: this backend has no fixture loader, and
# flows create everything they need through the API. Kept because db_client.py
# imports it.
FIXTURES_PATH = f"{APP_DIR}/nocrud/fixtures"

# --- Go adapter settings ----------------------------------------------------

# Built once before the flows fan out and reused by every flow. Running
# `go run` per flow would recompile the whole program for each one, which
# dominates the runtime and makes the provision step look artificially slow.
API_BINARY = APP_DIR / "bin" / "api"
MIGRATE_BINARY = APP_DIR / "bin" / "migrate"

# Admin listener offset. Each flow's app needs its own admin port too, or the
# second flow fails to bind. Derived from the API port rather than searched
# separately, so the pair always moves together.
ADMIN_PORT_OFFSET = 10000

# Postgres for the per-flow databases. Matches docker-compose.yml.
DB_HOST = "localhost"
DB_PORT = "5433"
DB_USER = "notes"
DB_PASS = "notes"
DB_ADMIN_DB = "notes"  # connected to in order to CREATE/DROP the per-flow ones

# At least 32 bytes or the service refuses to start. Test-only.
JWT_SECRET = "nocrud-runner-secret-not-for-production-use"
