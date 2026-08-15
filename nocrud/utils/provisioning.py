"""Framework adapter for the Go backend.

The noCRUD skill's Framework Adapter table lists three per-framework concerns;
this file implements two of them (DB provisioning, starting the app) and
utils/api_client.py implements the third (the auth handshake).

Compared with the Django adapter this is simpler in one way and fiddlier in
another:

  simpler  - no makemigrations to hang waiting for input, and no settings.py
             to cross-check, because the app reads DATABASE_URL directly. The
             db_match_check dance the Django adapter needs has no analogue: we
             hand the process the exact connection string.

  fiddlier - the app is compiled. Running `go run` per flow would rebuild the
             whole program for every flow and dominate the wall clock, so the
             binaries are built once up front and reused. And this service
             binds *two* ports (public + admin), so both have to be allocated
             per flow or the second flow fails to bind.
"""

import os
import socket
import subprocess
import sys
import threading
import time
from typing import Dict

import psycopg
from psycopg import sql

import config


def provision_env_for_flow(flow_name) -> Dict:
    """Create an isolated database, migrate it, and start the app against it."""
    return provision_go_env_using_migrate(flow_name)


# =================================================
#  Go provisioning
# =================================================


def provision_go_env_using_migrate(flow_name):
    ensure_binaries_built()

    # Two independently allocated ports, not one derived from the other.
    #
    # Deriving admin = public + 10000 looks tidy and breaks in parallel mode:
    # Linux hands out ephemeral ports up to ~60999, so anything above 55535
    # produces an admin port past 65535 and the process dies on bind. It also
    # risked one flow's admin port colliding with another flow's public port.
    port, admin_port = reserve_ports()
    # Postgres lowercases unquoted identifiers and caps names at 63 bytes;
    # flow names here are short, but lowercase keeps the psql output matching
    # what the runner prints.
    db_name = f"nocrud_p{port}_{flow_name}".lower()[:63]

    db_client = AdminDBClient()
    db_client.createDB(db_name)

    database_url = (
        f"postgres://{config.DB_USER}:{config.DB_PASS}"
        f"@{config.DB_HOST}:{config.DB_PORT}/{db_name}?sslmode=disable"
    )

    # The whole environment for this flow's app. Built explicitly rather than
    # mutating os.environ, because in parallel mode several flows provision
    # concurrently in the same process and would clobber each other's values.
    app_env = {
        **os.environ,
        "DATABASE_URL": database_url,
        "PORT": str(port),
        "ADMIN_PORT": str(admin_port),
        "JWT_SECRET": config.JWT_SECRET,
        "JWT_TTL": "1h",
        "LOG_LEVEL": "error",  # flow output is the signal; request logs are noise
        "S3_BUCKET": "notes-attachments",
        "S3_ENDPOINT": "http://localhost:9000",
        "S3_ACCESS_KEY": "minioadmin",
        "S3_SECRET_KEY": "minioadmin",
        "S3_PATH_STYLE": "true",
    }

    # APP_PORT is what APIClient reads to find the app, matching the runner's
    # existing convention.
    os.environ["APP_PORT"] = str(port)

    run_migrations(database_url)
    print(f"✅  Migration succeeded for DB: {db_name}")

    backend_proc, port, admin_port = start_backend_with_retry(port, admin_port, app_env)
    os.environ["APP_PORT"] = str(port)

    return {
        "DB_NAME": db_name,
        "APP_PORT": port,
        "ADMIN_PORT": admin_port,
        "client": db_client,
        "proc": backend_proc,
    }


def ensure_binaries_built():
    """Build the API and migrator once.

    The Django adapter has nothing equivalent -- Python needs no build step.
    Here, skipping this would mean a full `go build` inside every flow's
    provision, so the first flow pays for it and the rest reuse the binaries.
    """
    if config.API_BINARY.exists() and config.MIGRATE_BINARY.exists():
        return

    print("🔨 building api and migrate binaries (once)")
    result = subprocess.run(
        ["make", "build"],
        cwd=str(config.APP_DIR),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"go build failed:\n{result.stdout}")
    print("✅ binaries built")


def run_migrations(database_url):
    """Apply migrations with the project's own migrator.

    Note this is the *separate* migrate binary, not the API: this backend runs
    migrations as a discrete step and never on application startup, so the
    runner exercises the same path a real deploy does.
    """
    result = subprocess.run(
        [str(config.MIGRATE_BINARY), "up"],
        env={**os.environ, "DATABASE_URL": database_url},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"migrations failed:\n{result.stdout}")


def start_backend_with_retry(port, admin_port, app_env, attempts=6):
    """Start the app, retrying with fresh ports if the bind loses a race.

    Asking the OS for a free port and then closing the socket leaves a window
    in which another process can take it. With one flow that window is
    theoretical; with a pool of workers all provisioning at once it is hit
    routinely, and the failure looks like an unrelated flow error.

    Retrying is the honest fix: the race cannot be closed while the port is
    chosen by one process and bound by another, so detect and go again.
    """
    last_err = None
    for attempt in range(attempts):
        try:
            proc = start_backend_subprocess(port, admin_port, app_env)
            return proc, port, admin_port
        except RuntimeError as err:
            if "address already in use" not in str(err):
                raise
            last_err = err
            port, admin_port = reserve_ports()
            app_env = {**app_env, "PORT": str(port), "ADMIN_PORT": str(admin_port)}
            print(f"↻ port race on attempt {attempt + 1}, retrying on {port}/{admin_port}")
    raise RuntimeError(f"could not bind a free port after {attempts} attempts: {last_err}")


def start_backend_subprocess(port, admin_port, app_env):
    proc = subprocess.Popen(
        [str(config.API_BINARY)],
        cwd=str(config.APP_DIR),
        env=app_env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
    )

    # Retained as well as streamed. In parallel mode each flow runs in a Pool
    # worker whose stdout is captured, so a process that dies at startup
    # otherwise reports "exited with code 1" and nothing else -- the actual
    # reason (bad port, missing config) is the one line you need.
    output_lines = []

    def stream_output():
        assert proc.stdout is not None
        for line in proc.stdout:
            output_lines.append(line)
            print(f"[api:{port}] {line}", end="", file=sys.__stdout__)

    threading.Thread(target=stream_output, daemon=True).start()

    # Readiness, not just a listening socket. The port opens before the pool
    # has connected, so a plain connect check races the first request -- which
    # would surface as a mystifying flow failure rather than a setup problem.
    wait_for_backend_ready(admin_port, proc, output_lines)
    return proc


def wait_for_backend_ready(admin_port, proc, output_lines, timeout=20):
    """Poll /readyz until the app can actually serve."""
    import urllib.error
    import urllib.request

    deadline = time.time() + timeout
    while time.time() < deadline:
        if proc.poll() is not None:
            # Give the reader thread a moment to drain the pipe, so the error
            # includes what the process actually said.
            time.sleep(0.2)
            detail = "".join(output_lines).strip() or "(no output)"
            raise RuntimeError(
                f"backend exited during startup with code {proc.returncode}:\n{detail}"
            )
        try:
            with urllib.request.urlopen(
                f"http://localhost:{admin_port}/readyz", timeout=0.5
            ) as res:
                if res.status == 200:
                    return True
        except (urllib.error.URLError, OSError):
            time.sleep(0.1)

    detail = "".join(output_lines).strip() or "(no output)"
    raise TimeoutError(
        f"backend not ready on admin port {admin_port} after {timeout}s:\n{detail}"
    )


def find_open_port():
    """Let the OS assign a free port."""
    return find_open_ports(1)[0]


def reserve_ports():
    """A public and an admin port for one flow."""
    return find_open_ports(2)


def find_open_ports(count):
    """Allocate `count` distinct free ports.

    All sockets are held open until every port is chosen, then released
    together. Allocating one at a time lets the OS hand out the same port
    twice, because each socket is closed before the next is opened.

    A gap remains between releasing these and the app binding them, which is
    inherent to asking the OS for a free port rather than being handed a
    socket. It is small, and the readiness check below turns a lost race into
    a clear startup error rather than a confusing flow failure.
    """
    sockets = []
    try:
        for _ in range(count):
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.bind(("", 0))
            sockets.append(s)
        return [s.getsockname()[1] for s in sockets]
    finally:
        for s in sockets:
            s.close()


def cleanup_env(env):
    """Stop the app and drop its database.

    The runner sets persist_db=True when a flow fails, so the database survives
    for inspection. SIGTERM rather than kill, so the graceful-shutdown path
    actually runs -- the same path a deploy uses.
    """
    try:
        proc = env.get("proc")
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()

        if env.get("persist_db"):
            print(f"🔎 Persisting db {env['DB_NAME']} for inspection")
        else:
            AdminDBClient().dropDB(env["DB_NAME"])
    except Exception as e:
        print(f"⚠️ Cleanup warning: {e}")


# =================================================
#  Admin DB client
# =================================================


class AdminDBClient:
    """Creates and drops the per-flow databases.

    Separate from utils/db_client.py, which is Django-shaped: it shells out to
    manage.py and filters django_*/auth_* tables. Nothing here needs either.
    """

    def __init__(self):
        self.conn = psycopg.connect(
            dbname=config.DB_ADMIN_DB,
            user=config.DB_USER,
            password=config.DB_PASS,
            host=config.DB_HOST,
            port=config.DB_PORT,
        )
        self.conn.autocommit = True  # CREATE/DROP DATABASE cannot run in a transaction
        self.cur = self.conn.cursor()

    def createDB(self, db_name):
        try:
            self.cur.execute(
                sql.SQL("CREATE DATABASE {}").format(sql.Identifier(db_name))
            )
            print(f"✅ Database '{db_name}' created.")
        except psycopg.errors.DuplicateDatabase:
            print(f"⚠️ Database '{db_name}' already exists.")
        except Exception as e:
            print(f"❌ Failed to create database '{db_name}': {e}")
            raise

    def dropDB(self, db_name):
        try:
            # The app may not have fully released its connections yet, and
            # Postgres refuses to drop a database that still has any.
            self.cur.execute(
                "select pg_terminate_backend(pid) from pg_stat_activity "
                "where datname = %s and pid <> pg_backend_pid()",
                (db_name,),
            )
            self.cur.execute(
                sql.SQL("DROP DATABASE IF EXISTS {}").format(sql.Identifier(db_name))
            )
            print(f"🗑️  Database '{db_name}' dropped.")
        except Exception as e:
            print(f"❌ Failed to drop database '{db_name}': {e}")
            raise

    def close(self):
        self.conn.close()
