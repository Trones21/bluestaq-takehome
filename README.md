# Notes API

A note-taking service shared amongst several small teams. Go, Postgres,
S3-compatible object storage.

```bash
make dev-up          # Postgres + MinIO
make migrate         # apply the schema
make seed            # demo users, teams, notes
make run             # http://localhost:8080  ·  docs at /docs
```

Then open <http://localhost:8080/docs> and log in as `alice@example.com` /
`demo-password-123`.

---

## Contents

- [Running it](#running-it)
- [Three design choices](#three-design-choices)
- [What I would do with more time](#what-i-would-do-with-more-time)
- [The model in one paragraph](#the-model-in-one-paragraph)
- [Testing](#testing)
- [Deploying](#deploying)
- [Project layout](#project-layout)
- [Scope](#scope)

---

## Running it

Needs Go 1.25+, Docker, and `make`. Everything runs from the repository root.

### Local development

```bash
make dev-up          # Postgres on :5433, MinIO on :9000 (console :9001)
make migrate         # apply migrations to the dev database
make seed            # load demo data, prints a tour of what to look at
make run             # start the API
```

`make run` sources `scripts/env/local.sh`, which is committed on purpose —
there is nothing secret about a throwaway local database. Production values
never live in this repository; see [Deploying](#deploying).

The API is on `:8080`. Metrics, `/healthz` and `/readyz` are on `:9090`, a
separate listener that should never be publicly routed — the service refuses to
start if the two ports match.

To load the environment into your own shell (for `curl`, or to run a binary
directly):

```bash
source scripts/env/local.sh
```

### Everything else

```bash
make help            # every target, with a description
make test            # full suite against a real Postgres and MinIO
make flows-setup     # one-time: Python venv for the flow runner
make flows           # 19 multi-user API flows
make ci              # everything that must pass before a deploy
make migrate-status  # which migrations have been applied
make sweep-dry-run   # what the orphan sweep would collect
make dev-reset       # stop the stack and delete all data
```

### Trying the interesting parts

After `make seed`, the seed output points at specific note IDs. The two worth
looking at:

**Cross-team sharing.** One note shared with two teams at different roles. Log
in as `bob@example.com` (in both teams) and you are an `editor`; as
`carol@example.com` you are a `viewer`; as `dave@example.com` you get `404` —
not `403`.

**A version conflict.** Read any note and note the `ETag`. Send two `PATCH`
requests with the same `If-Match`. The second returns `409` with the current
state of the note in the body.

---

## Three design choices

### 1. One ACL table, and no partitioning

> "a note-taking service used and shared amongst several small teams"

The load-bearing word is *teams*, plural. If notes were only ever shared inside
one team, the natural model is a hard tenant boundary — `notes.team_id`, one
partition per team, simple queries. But "shared amongst several small teams"
says a note can cross a team boundary, and that single reading rules the tenant
model out: there is no partition key that keeps a note and all of its ACL rows
together, so exactly the queries the product exists for would fan out across
partitions. Partitioning would add cost and *remove* the benefit.

So: a note has an owner, and access beyond the owner is rows in one table whose
grantee — the *principal* — is either a user or a team.

```sql
note_shares (note_id, principal_type, principal_id, role, granted_by)
--                   'user' | 'team'
```

Every sharing case the prompt implies falls out of that one table. Private is
zero rows. A team note is one row. Cross-team is two rows. A one-off share to a
colleague is one row with `principal_type = 'user'`. None of these needed a
schema change, and neither would "visible to everyone in the org" — that is one
more enum value and one clause in the authorization predicate.

**What it costs, stated plainly.** `principal_id` cannot be a foreign key,
because it points at two different tables. So `ON DELETE CASCADE` does not fire:
deleting a user or a team leaves orphaned grants that would later resolve
against a reused UUID. Both deletions therefore remove their grants explicitly,
in the same transaction, and both have tests. I verified the orphan against a
live database rather than assuming it. The alternative — two nullable FK columns
with a check constraint that exactly one is set — buys real referential
integrity for a clumsier schema, and I would defend either.

**Why the role is an integer, not a string.** The database resolves the
strongest matching grant with `max()`. Ordering role *names* in SQL sorts
alphabetically, and `'viewer' > 'editor'` — so a `max()` over names silently
promotes every viewer on a note that carries both grants. There is a test that
fails if the ranks ever become alphabetically ordered, because the bug is
invisible until a note happens to have two grants.

Row-level security was considered and rejected: it moves authorization out of
testable application code and into policies that are harder to unit test and
harder to explain in a review. It is a good defence-in-depth layer *on top of*
the application rules, not instead of them.

### 2. Optimistic concurrency, because sharing creates the problem

The moment two people can edit one note, last-write-wins destroys work silently
— no error, anywhere, and the loser's five minutes are simply gone. That is the
default, and it is what most implementations of this exercise ship.

Every note read returns its version as an `ETag`. Writes require it back as
`If-Match`:

```
GET   /v1/notes/{id}     → 200, ETag: "7"
PATCH /v1/notes/{id}     If-Match: "7"  → 200, ETag: "8"
                         If-Match: "6"  → 409 + the current note in the body
                         (no If-Match)  → 428 Precondition Required
```

The check is a single conditional statement — `UPDATE ... WHERE id = $1 AND
version = $2` — so check and write are atomic with no explicit lock or
transaction. Zero rows affected means someone else committed first. A test
fires eight concurrent writers at one version and requires exactly one success
and seven conflicts.

`If-Match` is required rather than optional, because optional means the
careless client — the one most likely to clobber someone — is exactly the one
that skips it. `If-Match: *` is refused for the same reason: it means "any
version", which is the unconditional overwrite this exists to prevent. And the
409 carries the current representation, so a client can show a diff rather than
just an error.

**Known limitation, worth raising before you find it:** versioning is per-note,
not per-field. Editing tags while someone else edits the body produces a
spurious conflict. The fix is a server-side three-way merge when the changed
field sets are disjoint — but that needs the base version's values, which means
version history, which I cut. So this limitation and "add version history" are
the same item, not two.

### 3. Attachments never pass through the API

Notes hold images and video, and the obvious implementation — multipart upload
through the API to a column or a disk — is the one that does not survive
production. It puts large uploads on the app server's request path, holds a
connection and a database transaction for the duration, and rules out a CDN.

So the service is a metadata and policy layer over object storage:

```
POST .../attachments            → row (pending) + a presigned PUT URL
PUT  <presigned url>            → client uploads bytes directly to storage
POST .../attachments/{id}/complete → verify, record size, mark ready
GET  .../attachments            → fresh, short-lived download URLs
```

**The trap, and the reason this is two-phase.** A presigned PUT signs the
method, key and expiry — **not the body length**. A client holding that URL can
upload any number of bytes, so a size check that lives only in the presign
handler is decorative. The cap is enforced at `/complete` by stat-ing the
object; the test proves it by declaring 100 bytes, uploading over a megabyte
(storage accepts it, which is the point), and requiring a 413. Presigned POST
with a `content-length-range` policy would let S3 reject the upload itself, and
is the upgrade.

**Bodies reference attachments, never URLs.** A presigned URL must never be
written into a note body — it expires, so the note would rot, rendering today
and 404ing in fifteen minutes. The body carries `![alt](attachment:{uuid})` and
the server resolves it per read. No position column is needed: Markdown already
says where each image renders, so the body *is* the ordering. References are
validated on write and must exist, be ready, and belong to *this* note — that
last clause because copy-pasting body text between notes is a real user action
that would otherwise create a cross-note reference.

**Why not `bytea` in Postgres?** A fairer question than it first appears, and
the usual objection is wrong: values over ~2KB are TOASTed out of line, so a
scan of `notes` does not drag blobs through memory. The real reasons are WAL
amplification (a 50MB video is 50MB+ of write-ahead log, which becomes
replication lag and larger backups), backups scaling with media volume, and no
range requests for video seeking. And for small images `bytea` genuinely wins
on simplicity, because it buys atomicity — no pending state, no orphan sweep,
no two-phase upload. The honest rule is a threshold, not a dogma: images under
a megabyte in Postgres is defensible; video never is. This service takes video.

**The soft spot I am not hiding:** a leaked download URL stays valid until it
expires, independent of a later revoke. Short TTLs bound it. Proxying every
download through the service so the ACL is checked per request is the answer
when content is sensitive enough to pay for it.

---

## What I would do with more time

Roughly in the order I would pick them up.

1. **Login throttling.** The sharpest gap. Nothing limits failed login attempts,
   so the endpoint is open to password brute force. It looks like rate limiting
   but is not delegable to an edge WAF, which cannot tell a failed login from a
   successful one — only this service knows. It needs failed-attempt throttling
   keyed on account plus source.

2. **Version history.** Also the prerequisite for fixing the spurious-conflict
   limitation above, since a field-level merge needs the base version's values.
   Two features, one change.

3. **An audit log.** A real gap for a shared-access system: today there is no
   record of who read or was granted what.

4. **Ownership transfer and offboarding.** Currently only self-service account
   deletion exists. Offboarding someone who left the company wants transfer, not
   deletion — and it is missing for a structural reason worth naming: the model
   has team admins but no *org* admin, and offboarding is inherently
   org-level. Related: a note whose owner is deleted and whose grants are all
   gone becomes unreachable, with no admin recovery path.

5. **Real-time collaboration.** Optimistic concurrency is the honest version at
   this budget. The next step is presence and live invalidation over SSE, which
   needs cross-instance fanout — Postgres `LISTEN`/`NOTIFY` (no new
   infrastructure, but an 8KB payload cap and a dedicated connection per
   listener, which fights PgBouncer in transaction pooling) or Redis pub/sub.
   True concurrent editing means a CRDT, and that is not a bolt-on: the note
   body stops being a queryable text column and becomes an opaque binary doc,
   so full-text search needs a plaintext projection materialized on write.

6. **Secrets out of the deploy path.** Today `pass` renders a `.env_prod` that
   is scp'd. It is created `600`, removed by an exit trap, and shredded on the
   server — but it still puts production secrets on a laptop. SSM Parameter
   Store or Secrets Manager with an IAM-scoped instance role fixes that
   properly.

7. **Rate limiting proper**, at the edge or in shared state — never in-process,
   where N instances give N× the intended limit.

**What I would stop doing:** the `docker save`/`scp`/`docker load` deploy. It is
right for one host and fewer moving parts than running a registry, but it stops
being right the moment there is a second box, at which point it becomes a push
to ECR and a pull on each. Also the CDN-from-unpkg Swagger UI — fine as a local
review tool, wrong for a real deployment, where the assets should be vendored.

---

## The model in one paragraph

**A note has no home team.** It belongs to its owner; teams are only ever
grantees. "Team note" is shorthand for a note with one team grant, not a note
that lives inside a team. Everything else follows: cross-team sharing is two
rows rather than a schema change, and a note with no grants is simply private.

| Action | Minimum role |
|---|---|
| read, list shares, download attachments | viewer |
| edit title/body/tags, add/remove attachments | editor |
| share, revoke, delete, restore | owner |

Three deliberate choices in that table. **Editors cannot re-share** — re-sharing
rights are the usual source of "how did this leak", and owner-only keeps the
access graph one hop deep. **Team role is not note permission** — a team admin
is not automatically an editor of everything shared with the team; membership
role governs the team only. **Restore is owner-only, matching delete** —
otherwise an editor could undo an owner's deletion.

**404, not 403.** If you cannot read a note at all, the API reports it absent. A
403 confirms it exists, turning ID enumeration into a way to learn about other
teams' data. 403 appears only when you can already see the resource but lack the
role for that specific action — its existence is not a secret from you.

Every "can it do X" question, including everything deliberately left out, is
answered in [CAPABILITIES.md](CAPABILITIES.md), which doubles as the test
manifest. [SPEC.md](SPEC.md) is the design written before the code;
[DISCUSSION.md](DISCUSSION.md) is the defence of each decision and the
alternatives rejected.

---

## Testing

Two suites, because there are two kinds of failure.

```bash
make test          # Go: unit + integration, against real Postgres and MinIO
make flows         # noCRUD: 19 multi-user API flows
```

**Go tests.** The authorization rules get an exhaustive table-driven matrix —
that is the code where a bug is a data leak rather than a broken feature, so
the expected values are written out by hand rather than derived from the
implementation, which would only prove the code agrees with itself. Integration
tests run against a real database and real object storage; nothing is mocked,
because every interesting bug here lives in a SQL predicate or in what S3
actually does.

Each test *package* gets its own database. `go test ./...` runs packages
concurrently, so a shared one meant a truncate in one package wiped another's
fixtures mid-test — passing per-package and failing only on the full suite,
which is the worst way for a harness bug to present.

**Flows.** [noCRUD](https://github.com/Trones21/noCRUD) drives several
authenticated clients against each other over HTTP, each flow against its own
provisioned database and app instance. This is the layer that can express "Bob
cannot read Alice's private note", which no single-client test can. noCRUD ships
a Django adapter; the Go one is in `nocrud/utils/provisioning.py`, and
`nocrud/INVENTORY.md` maps every flow to the rule it asserts.

---

## Deploying

```bash
cp scripts/env/prod.sh.example ~/env_setup/notes/prod.sh   # then edit
chmod 600 ~/env_setup/notes/prod.sh
./scripts/deploy.sh
```

`deploy.sh` runs the CI checks, shows you the output, and **asks before
shipping** — it accepts only a literal `yes`, because a bare newline must never
mean deploy to production. Then: build, transfer, migrate, start, smoke.

**Migrations run before the new container starts, as their own step.** Not on
application startup, where N instances race the same database and a failed
rollout cannot be rolled back — the previous binary would come back up against a
schema it does not understand. Anything zero-downtime uses expand/contract,
since both versions are briefly live.

The image is distroless: ~40MB, non-root, no shell and no package manager, so a
future RCE finds no toolchain. That costs `docker exec` diagnostics, which is
why the container healthcheck runs the API's own `-healthcheck` probe rather
than shelling out to curl.

Traefik terminates TLS and routes only the public port; the admin port carrying
`/metrics` has no router and binds to loopback, reachable over
`ssh -L 9090:localhost:9090`. Port 80 redirects to 443 so a client that forgets
the scheme does not send a bearer token in cleartext first.

`ci.sh` is a script rather than a CI workflow file, so identical checks run on a
laptop and in a pipeline — YAML that only exists in CI cannot be run before
pushing. It also regenerates sqlc and fails on a diff, catching a query file
edited without rerunning codegen.

---

## Project layout

```
api/openapi.yaml       hand-written contract, embedded in the binary
cmd/api                the service
cmd/migrate            migrations — a separate binary, run as its own step
cmd/seed               demo data
cmd/sweep              collects abandoned uploads
internal/authz         the decision table. one rule, in one place
internal/store         sqlc-generated queries + one hand-written list query
internal/apitest       real router, real database, real HTTP, a client per user
migrations/            goose SQL, embedded
nocrud/                the flow runner and its Go adapter
scripts/               env, ci, deploy, smoke
deploy/                production compose + Traefik + Prometheus
```

**Why sqlc, and where it stops.** The argument is not scan boilerplate — an LLM
writes that fine. It is compile-time verification against the real schema:
rename a column, regenerate, and the *build* breaks at every query that touched
it, where hand-written scan code fails at runtime in whichever endpoint was
missed. This is a language-independent argument, not a Go one; sqlc has a Java
plugin, so the real position is *sqlc over an ORM*.

It stops fitting at `GET /v1/notes`, which has optional filters. sqlc's answer
to dynamic predicates (`sqlc.narg` plus `WHERE ($1 IS NULL OR col = $1)`)
compiles but cannot use an index through the OR-NULL, so every filter
combination degrades to a sequential scan. That one query is hand-written pgx,
in its own file, commented as the deliberate exception.

---

## Scope

Built: users and auth, teams and membership, notes with soft delete and
restore, sharing to users and teams, full-text search with filters and keyset
pagination, attachments, observability, migrations, and a deploy pipeline.

Deliberately not built, with reasons in [SPEC.md §2](SPEC.md) and
[CAPABILITIES.md](CAPABILITIES.md): real-time collaboration, version history,
comments, refresh tokens, rate limiting, an audit log, public share links, and
org-level administration.

**Why Go, given this is a Java shop.** Two honest reasons. It is what I write
backends in continuously, so in a time-boxed exercise you see my best work
rather than my rustiest; and the deploy pipeline here is image-size sensitive,
where a 40MB static binary against a 200–400MB JVM image is wall-clock on every
deploy over SSH. What I will *not* claim: not concurrency, since Loom's virtual
threads went GA in Java 21 and this workload is IO-bound, which is exactly the
case that closes; not ecosystem, since Spring Security would have given me JWT
plumbing and method-level authorization free where I hand-rolled both. And the
concession worth making unprompted — in a Java org a Go service is an
operational outlier: another toolchain, another base image, another scanning
path, fewer people who can be paged for it. That is a legitimate reason to say
no, and it has nothing to do with the language. I would write this in Java on
your team without complaint, and I am happy to walk through what the Spring
Boot version of this schema and authorization layer looks like.
