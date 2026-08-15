# Notes Service — Spec

Working design doc. Written before implementation so the tradeoffs are on the record and
reviewable independently of the code.

Rejected alternatives and the defense of each decision live in [DISCUSSION.md](DISCUSSION.md).
Every "can it do X" question, including everything deliberately left out, is answered in
[CAPABILITIES.md](CAPABILITIES.md), which doubles as the test manifest. This file states what is
being built.

**Stack:** Go, chi router, pgx/v5 + sqlc, Postgres 16, MinIO (S3-compatible) for objects.
The "why Go, not Java" and "why sqlc" arguments are DISCUSSION §1 and §2.

## 1. Reading of the prompt

> "the backend for a note-taking service used and shared amongst several small teams"

Two words in that sentence drive the entire design:

- **shared** — notes are not private-by-construction; access is a first-class concept, not a
  `WHERE user_id = ?` clause.
- **teams**, plural — a note can be visible to more than one team. This is the load-bearing
  inference. It rules out modelling a team as a hard tenant boundary, because a note that
  crosses teams has no single home tenant.

Everything else in the prompt is deliberately open ("You choose the language, framework,
schema, endpoints, storage, and what's in scope"), so the spec states its assumptions
explicitly rather than pretending they were given.

### Assumptions being made

| # | Assumption | Basis |
|---|---|---|
| A1 | Notes may be shared across team boundaries, not just within one team | "several small teams" |
| A2 | Notes carry attachments (images/video), not just text | "capture and work with their notes" — the common real-world shape |
| A3 | No frontend is in scope; a reviewer needs a way to exercise the API instead | prompt says "backend"; OpenAPI + seed data substitutes for a UI |
| A4 | "Small teams" means tens of users per team, not thousands | sizing decisions below assume this |
| A5 | Notes are plain text / Markdown; rendering is a client concern | keeps the server out of the content-format business |

## 2. Scope

### In

- Users: register, profile update, password change, soft-delete account
- Authentication (JWT), team CRUD and membership management
- Notes: create, read, update, soft-delete, **restore**, list
- Sharing: grant/revoke access to a **user or a team**, with a role
- Search: full-text over title + body, scoped to what the caller may read
- Tags and filtering
- Cursor pagination
- Optimistic concurrency on updates
- Attachments: metadata + presigned upload/download against object storage
- Inline attachment references in the note body, validated on write (§7)
- Observability: request-timing middleware, Prometheus metrics per endpoint, structured logs,
  Grafana dashboard in compose (§10)
- Graceful shutdown, config validation at boot, request-ID correlation (§11)
- Migrations as a discrete deploy step + bash release pipeline (§12)
- OpenAPI document, seed script, `docker compose up` to run everything
- Tests: unit tests on the authorization rules, integration tests against real Postgres

### Out (and why)

| Not doing | Why |
|---|---|
| Realtime collaborative editing (CRDT/OT) | Days of work, not hours. Optimistic concurrency (§5) is the honest version at this budget. Evolution path in DISCUSSION §4 — tier 3 is a rewrite of the note-body storage path, not a bolt-on. |
| Note **version history** | Different from restore, which *is* in scope. History is the prerequisite for field-level merge (DISCUSSION §4) and is the first follow-up. |
| Comments, mentions, notifications | Adjacent product surface, not the core ask. |
| Rich text, Markdown rendering, sanitization | Server stores bytes; clients render. |
| Refresh tokens / session revocation | Short-lived access tokens only; production would delegate to an IdP. |
| Rate limiting and quotas | Belongs at the edge or in shared state, not in-process. Layering in DISCUSSION §5. The in-process pieces that *are* correctness concerns — body size cap, server timeouts, bounded pool — do ship (§7). |
| WAF / DDoS protection | A separate system, correctly. Not something an application server should be pretending to do. |
| Login brute-force throttling | **A real security gap**, and the one consequence of deferring rate limiting that is not purely operational — a WAF cannot distinguish a failed login from a successful one, so the edge does not cover it. First thing to add. DISCUSSION §5. |
| Audit log | Real gap for a shared-access system. Named in the README. |
| Public notes ("everyone in the org") | Not built — but it is `principal_type = 'everyone'`, one enum value and one predicate clause. §3. |
| Public share links (no login) | Different feature: share token, unauthenticated read path, separate threat model. §3. |
| Copying attachments between notes | Cross-note references are rejected rather than silently resolved (§7). Auto-copy is a real feature, not a bug fix. |
| Org/billing tier above teams | Not implied by the prompt. |

## 3. Domain model

### Decision: owner + a single generalized ACL table

A note has exactly one **owner** (the creating user). Access beyond the owner is expressed
by rows in `note_shares`, where the grantee — the *principal* — is either a user or a team.

**A note has no home team.** It belongs to its owner; teams are only ever grantees. "Team note"
is shorthand for a note with one team grant, not a note that lives inside a team. This is the
mental model the whole design rests on — it is why cross-team sharing is two rows rather than a
schema change, and why a note with zero grants is simply private.

This one table expresses every sharing case the prompt implies:

- **private note** — no share rows
- **team note** — one share row with `principal_type = 'team'`
- **shared across teams** — two team share rows
- **one-off share to a colleague** — one row with `principal_type = 'user'`

Not built, but worth noting for what it says about the model: **"visible to everyone in the
org"** is `principal_type = 'everyone'` — one enum value and one clause in the authorization
predicate, no migration of existing rows. **"Anyone with the link, no login"** is a different
feature entirely (unguessable share token, unauthenticated read path, different threat model)
and is firmly out of scope. These two get conflated as "public"; they are not the same work.

The alternatives and why they lost:

- **Team-owned notes** (`notes.team_id NOT NULL`, membership *is* access) is simpler and has
  cleaner queries, but makes cross-team sharing impossible without copying the note — which
  contradicts A1. Copying means divergent edits, which is worse than the complexity saved.
- **`owner_id` + `team_id` + `visibility` enum** avoids the join table but freezes the sharing
  axis at build time. Adding "share with one other team" later means a schema migration and a
  rewrite of every read path. The join table costs one extra table now and nothing later.

The tradeoff being accepted: **every read path now goes through an authorization join.** That
is the cost, it is real, and §4 addresses how it stays fast.

### Decision: no physical partitioning

Partition-by-team was considered and rejected:

- A1 means a note can belong to two teams. There is no single partition key that keeps a note
  and all of its ACL rows co-located, so cross-team list queries would fan out across
  partitions and lose the benefit.
- A4 means the data volume does not justify it. Partitioning solves a problem this service
  does not have yet.

Indexed foreign keys instead. The README will name the trigger to revisit: sustained
single-table volume where index maintenance or vacuum becomes the bottleneck, at which point
the partition key is time (`created_at`), not team — because time-partitioning is orthogonal
to the sharing model and team-partitioning is not.

Postgres **row-level security** was also considered. Rejected for this build: it moves the
authorization rules out of testable application code and into policies that are harder to unit
test and to explain in review. Worth a README paragraph as defense-in-depth for a
higher-assurance deployment.

### Schema sketch

```sql
users (
  id            uuid primary key,
  email         citext unique not null,
  password_hash text not null,
  display_name  text not null,
  created_at    timestamptz not null default now(),
  deleted_at    timestamptz          -- soft delete; notes survive, owner renders as a tombstone
);

teams (
  id         uuid primary key,
  name       text not null,
  created_at timestamptz not null default now()
);

team_members (
  team_id uuid references teams on delete cascade,
  user_id uuid references users on delete cascade,
  role    text not null check (role in ('member','admin')),
  primary key (team_id, user_id)
);
create index on team_members (user_id);   -- "which teams am I in" is on every read path

notes (
  id            uuid primary key,
  owner_user_id uuid not null references users,
  title         text not null,
  body          text not null default '',
  tags          text[] not null default '{}',
  version       integer not null default 1,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  deleted_at    timestamptz                      -- soft delete
);
create index on notes (owner_user_id) where deleted_at is null;
create index on notes (updated_at desc, id desc) where deleted_at is null;  -- keyset pagination
create index on notes using gin (tags);

-- generated tsvector; title weighted above body
alter table notes add column search_tsv tsvector
  generated always as (
    setweight(to_tsvector('english', coalesce(title,'')), 'A') ||
    setweight(to_tsvector('english', coalesce(body ,'')), 'B')
  ) stored;
create index on notes using gin (search_tsv);

note_shares (
  note_id        uuid not null references notes on delete cascade,
  principal_type text not null check (principal_type in ('user','team')),
  principal_id   uuid not null,
  role           text not null check (role in ('viewer','editor')),
  granted_by     uuid not null references users,
  created_at     timestamptz not null default now(),
  primary key (note_id, principal_type, principal_id)
);
create index on note_shares (principal_type, principal_id);  -- "what can I see" direction

attachments (
  id          uuid primary key,
  note_id     uuid not null references notes on delete cascade,
  object_key  text not null unique,
  filename    text not null,
  mime_type   text not null,
  size_bytes  bigint not null,
  checksum    text,
  status      text not null check (status in ('pending','ready')),
  uploaded_by uuid not null references users,
  created_at  timestamptz not null default now()
);
create index on attachments (note_id) where status = 'ready';
```

`principal_id` is intentionally not a foreign key — it is polymorphic across two tables. The
alternative (two nullable FK columns with a check constraint that exactly one is set) buys real
referential integrity for a clumsier schema; it is close to a coin flip.

**The integrity cost is concrete, not theoretical.** Because there is no FK, `ON DELETE CASCADE`
does not clean up shares when a principal disappears:

- deleting a **team** orphans its `('team', id)` share rows
- deleting a **user** orphans its `('user', id)` share rows

Orphaned grants would resolve against a reused UUID. So both deletions must remove matching
`note_shares` rows **in the same transaction**, and both get an integration test. This is the
price of the one-table ACL and it is paid in application code. See DISCUSSION §7.

Tags are a `text[]` with a GIN index rather than a join table. At A4 volumes the array is
simpler and fast enough; a join table is the answer only once tags need their own metadata
(colors, per-team tag vocabularies, renames).

## 4. Authorization

One rule, in one place, exercised by every endpoint.

```
effective_role(user u, note n) =
    'owner'   if n.owner_user_id = u
    else max over grants g in note_shares(n) where
             (g.principal_type = 'user' and g.principal_id = u)
          or (g.principal_type = 'team' and g.principal_id in teams_of(u))
    else none

owner > editor > viewer
```

| Action | Minimum role |
|---|---|
| read note, list attachments, download attachment | viewer |
| update title/body/tags, add/remove attachments | editor |
| share, revoke, delete note | owner |

Deliberate simplifications, each defensible:

- **Editors cannot re-share.** Re-sharing rights are the usual source of "how did this leak"
  incidents; owner-only keeps the access graph one hop deep and auditable.
- **Team role does not affect note access.** `team_members.role` governs membership
  management only. A team admin is not automatically an editor of every note shared with the
  team — that conflation is a common source of surprise.
- **No ownership transfer.** Out of scope; the note dies with a deleted owner (or is reassigned
  by an operator, in a real system).

**Performance note.** The list/search path resolves `teams_of(u)` once per request and passes
it as an array parameter, so the note query is a single scan with an `EXISTS` against
`note_shares` on the `(principal_type, principal_id)` index — no N+1, no per-row lookup. This
is the concrete answer to the cost accepted in §3.

Every access-denied case returns **404, not 403**, when the caller cannot read the note at all.
Returning 403 confirms the note exists, which leaks the existence of other teams' notes. 403 is
used only when the caller *can* read the note but lacks the role for the specific action.

## 5. Concurrency

`notes.version` increments on every successful update. Reads return it as an `ETag`; updates
require `If-Match`.

```
GET  /v1/notes/{id}          -> 200, ETag: "7"
PATCH /v1/notes/{id}
  If-Match: "7"              -> 200, ETag: "8"
  If-Match: "6"              -> 409 Conflict (+ current representation in the body)
  (no If-Match)              -> 428 Precondition Required
```

The update is a single conditional statement — `UPDATE ... WHERE id = $1 AND version = $2` —
so the check and the write are atomic without an explicit transaction or row lock. Zero rows
affected means a conflict.

This is the design choice most directly demanded by "shared amongst several teams": the moment
two people can edit one note, last-write-wins silently destroys work. It is cheap to implement
and it is the difference between a service that shares notes and one that loses them.

## 6. API surface

Versioned under `/v1`. Errors are `application/problem+json` (RFC 9457).

```
POST   /v1/auth/register
POST   /v1/auth/login                          -> access token

GET    /v1/users/me
PATCH  /v1/users/me                            display_name, email
POST   /v1/users/me/password                   requires current password
DELETE /v1/users/me                            soft delete; notes survive, tokens revoked
GET    /v1/users?email=                        EXACT match only, scoped to co-members

GET    /v1/teams                               teams I belong to
POST   /v1/teams                               creator becomes admin
GET    /v1/teams/{id}
PATCH  /v1/teams/{id}                          admin only
DELETE /v1/teams/{id}                          admin only; deletes its share grants (§3)
GET    /v1/teams/{id}/members
POST   /v1/teams/{id}/members                  admin only
DELETE /v1/teams/{id}/members/{userId}         admin only; cannot remove last admin
POST   /v1/teams/{id}/leave                    cannot leave as last admin

POST   /v1/notes
GET    /v1/notes                               filters below
GET    /v1/notes/{id}
PATCH  /v1/notes/{id}                          If-Match required
DELETE /v1/notes/{id}                          soft delete, owner only
POST   /v1/notes/{id}/restore                  owner only; clears deleted_at

GET    /v1/notes/{id}/shares
PUT    /v1/notes/{id}/shares                   upsert grant {principal_type, principal_id, role}
DELETE /v1/notes/{id}/shares/{type}/{id}

POST   /v1/notes/{id}/attachments              -> {attachment_id, upload_url}
POST   /v1/notes/{id}/attachments/{aid}/complete
GET    /v1/notes/{id}/attachments              -> download_url per ready attachment
DELETE /v1/notes/{id}/attachments/{aid}

GET    /healthz  /readyz
GET    /openapi.json   /docs
```

### Listing and filters

**The invariant: every filter narrows the set the caller can already read.** Filters never
widen access. `?owner=bob` returns Bob's notes *I can already see*, not all of Bob's notes.
This is what makes the endpoint safe to extend — a new filter is a new `AND`, never a new way
in.

| Param | Effect |
|---|---|
| `q` | Full-text over title + body; switches ordering to `ts_rank` |
| `tag` | Repeatable, AND semantics |
| `owner` | Notes owned by this user (replaces a separate `owned_by_me` flag — it is `?owner=me`) |
| `team` | Notes shared with this team |
| `shared_with_me` | Excludes notes I own |
| `deleted` | Owner-scoped; a deleted note stays invisible to everyone else |
| `limit`, `cursor` | Keyset pagination |

Pagination is keyset on `(updated_at, id)`, cursor an opaque base64 of the last row's sort key.
Offset pagination is wrong here because it skips and duplicates rows on every concurrent insert.

**Precisely:** keyset removes offset drift; it does not make the scan a snapshot. A note edited
mid-pagination changes its sort key and can still be seen twice or missed. That is acceptable
for a recency-ordered feed, and the fix — sorting on an immutable `(created_at, id)` — trades
away the ordering users actually want.

### Deletion semantics

"Delete a user" is three different operations, and only the first is built:

1. **Self-service account deletion** — soft delete, tokens revoked, **notes survive** with the
   owner rendered as a tombstone. In a shared service the notes are not only yours; hard-deleting
   them destroys other people's working context.
2. **Offboarding** (person left the company) — wants ownership *transfer*, not deletion. Out of
   scope because the model has no org-level admin, only team admins. That is the honest reason,
   and it is a modelling boundary rather than an oversight.
3. **GDPR erasure** — needs an anonymize-and-retain path, since note content may be a business
   record while the identity is not.

Deleting a **team** does nothing to notes; it removes that team's grants only.

## 7. Attachments

Bytes never transit the JSON API. The service is a metadata and policy layer over an
S3-compatible object store (MinIO in `docker compose`, behind a storage interface so real S3
drops in unchanged).

```
1. POST .../attachments {filename, mime_type, size_bytes}
     - caller must be editor
     - mime allowlist + max size enforced HERE, before any URL is issued
     - row inserted status='pending', object_key = notes/{note_id}/{uuid}
     - returns a presigned PUT URL, ~15 min TTL

2. client PUTs bytes directly to object storage

3. POST .../attachments/{aid}/complete
     - service HEADs the object, verifies it exists and size matches the declared value
     - status='ready'

4. GET .../attachments -> presigned GET URLs, short TTL, viewer role sufficient
```

Why not multipart through the API: it puts large uploads on the request path of the app
server, blows up memory and timeouts, and prevents the CDN/edge story entirely. This is the
pattern that would not survive a production video upload, so it is not the pattern to
demonstrate. (Why not `bytea` in Postgres — a better question than it first appears — is
DISCUSSION §3.)

**Size enforcement has a trap.** A presigned **PUT** cannot enforce object size: the signature
covers the key and headers, not the body length, so a client holding the URL can upload any
number of bytes. A size check that lives only in the presign handler is decorative. So the cap
is enforced at step 3 — `HEAD` the object, compare against the declared `size_bytes`, delete and
reject on mismatch. Presigned **POST** with a `content-length-range` policy is the stronger
answer (S3 rejects the upload itself) and is named as the upgrade. See DISCUSSION §6.

### Limits that do ship

Not rate limiting (out of scope, DISCUSSION §5) — these are correctness and survival concerns,
roughly twenty lines total:

| Limit | Value | Enforced |
|---|---|---|
| Request body | 1 MiB | `http.MaxBytesReader` |
| Attachment size | 100 MiB | declared at presign, verified at complete |
| Attachment MIME | image/* , video/mp4, application/pdf | allowlist at presign |
| Server timeouts | read/write/idle | `http.Server` |
| Request duration | 15s | context deadline, `obs.Timeout` |
| Statement duration | 3s | `statement_timeout`, enforced by Postgres |
| Open transaction, idle | 10s | `idle_in_transaction_session_timeout` |
| DB connections | bounded | `pgxpool` max conns |

Rows stuck in `pending` are orphans (client abandoned the upload). A sweep deletes `pending`
rows and their objects after 24h. For the deliverable this is a documented command rather than
a scheduler, so it stays testable and does not add a background-job dependency.

### Referencing attachments inside the note body

An image in a note is not just an attached file — it renders at a specific point in the text.
That position is the Markdown itself, so **no position column is needed**: the body *is* the
ordering. What does need designing is what the Markdown points at.

**A presigned URL must never be written into the body.** It expires, so the note would rot —
saved content that renders today and 404s in fifteen minutes. This is the whole trap, and it is
easy to walk into because the presigned URL is the thing you have in hand at upload time.

So the body carries a **stable reference** and the server resolves it at read time:

```markdown
![architecture diagram](attachment:01H8XG5K...)
```

`GET /v1/notes/{id}` returns the body verbatim plus an `attachments` map of id → freshly
presigned URL, and the client substitutes. The body stays immutable, portable, and diffable;
URL lifetime is decoupled from content lifetime.

**Validation on create and update.** The body is parsed for `attachment:` references, and each
must exist, be `ready`, and belong to **this note**. Anything else is a 400. Without this you
accumulate dangling references — and the cross-note case is real: copy body text between notes
and the reference points at another note's attachment. Rejected rather than silently copied;
auto-copying attachments across notes is named as out of scope.

**Deleting a referenced attachment returns 409**, with the referencing positions listed. Remove
the reference first. The alternative — allowing it and leaving broken images — makes the service
complicit in corrupting its own content.

**Unreferenced attachments are not orphans.** A file attached to a note without being inlined is
a legitimate case, so the `pending` sweep is about abandoned *uploads*, not about attachments
missing from the body.

**Presigned GET TTL is 1 hour**, not the 15 minutes used for uploads. A reader with a note open
for an hour should not watch its images break. Longer than that and the revoke gap gets worse.

**The alternative considered:** point the Markdown at `/v1/notes/{id}/attachments/{aid}/content`
and 302 to a presigned URL. That makes a plain `<img src>` work with no client-side
substitution — but an `<img>` tag cannot send an `Authorization` header, so it forces cookie
auth or a token in the query string. That is a meaningfully worse security posture to adopt for
rendering convenience, so it is rejected, and it is the reason CDN signed *cookies* exist.

**Note lists do not include attachment URLs.** Presigning is a per-object operation, so
embedding download URLs in a 50-note list page means 50 presign calls to serve a screen that
displays none of them. Attachment URLs are returned only by `GET /notes/{id}` and
`GET /notes/{id}/attachments`. The list response carries an attachment *count* instead.

Presigned URLs are the deliberate soft spot: a leaked download URL is valid until it expires,
independent of a later revoke. Short TTLs bound it. The alternative — proxying every download
through the service so the ACL is checked per request — is noted in the README as the choice
to make when the content is sensitive enough to justify the cost.

**One production detail worth knowing now:** an S3 presigned URL addresses S3 directly and
therefore **bypasses a CDN**. Putting CloudFront in front of attachments is not transparent —
it means switching to CloudFront signed URLs or signed cookies, issued with a different key and
a different policy format. The storage interface hides the swap from callers, but it is a real
piece of work rather than a configuration change, and assuming otherwise is how "just add a CDN"
turns into a sprint.

## 8. Testing

- **Unit** — the `effective_role` resolution and the action table in §4, exhaustively. This is
  the part where a bug means a data leak, so it gets table-driven coverage of every
  role × action × principal-type combination.
- **Integration** — real Postgres (containerized), no mocked database. Covers the sharing
  flows end to end, the 409 conflict path, keyset pagination across concurrent inserts, the
  404-not-403 behavior, and share cleanup on user/team deletion.
- **Flows** — several authenticated clients driving the API against each other, for the
  scenarios that only exist with more than one user in play. The rows marked `flow` in
  CAPABILITIES.md are the list.
- **Attachments** — MinIO in the test compose stack; the presign → upload → complete cycle.

## 9. Delivery

`docker compose up` brings up Postgres, MinIO, and the service, runs migrations, and seeds:
three users, two teams with an overlapping member, and notes covering private / team-shared /
cross-team-shared / user-shared. A reviewer can then read `/docs` and exercise every path.

README covers: how to run, the three design choices above (ACL over partitioning, optimistic
concurrency, presigned attachments), what was cut and why, and the commit history.

## 10. Observability

**In scope.** You cannot operate what you cannot see, and "is the API slow" is not answerable
after the fact without this.

One small terminology note so the README says the right thing: **Prometheus is metrics, not
logs.** Its logging counterpart in the Grafana stack is Loki. This build does metrics +
structured logs to stdout; shipping logs to Loki is named, not built.

### Middleware

Every request passes through, in order: request ID → structured logger → metrics → recovery
→ deadline.

- **Request ID** — generated if absent, echoed in the response header, attached to the
  request-scoped logger so every line from one request correlates.
- **Structured logs** — `log/slog`, JSON, one line per request with method, route pattern,
  status, duration, user ID, request ID.
- **Metrics** — RED per endpoint:
  - `http_requests_total{method, route, status}` — counter
  - `http_request_duration_seconds{method, route}` — histogram
  - `http_request_timeouts_total{method, route}` — counter
  - `pgxpool_*` — pool saturation, via a small `prometheus.Collector` over `pool.Stat()`
    (`client_golang` ships the Go runtime and process collectors, but not one for pgx)

### Cardinality: raw path in logs, route pattern in metrics

Metrics are labelled with the **chi route pattern** (`/v1/notes/{id}`), never the raw path. The
log line carries the raw path. Both, in the place each is cheap.

The reason is specific to how pull-based metrics work, and it is *not* a hot-path latency
concern — nothing here is written to an external system per request. A `HistogramVec` holds an
in-process map from label-tuple to child metric, and because Prometheus scrapes, every child
ever observed must stay resident to be reported at the next scrape. Nothing is evicted for the
life of the process.

Label by raw path and every distinct note UUID permanently adds an entry — for a histogram,
~12 bucket counters plus sum and count plus retained label strings. The cost is proportional to
distinct IDs ever seen, so it is invisible in development and only bites at scale: memory that
never returns, and a `/metrics` body that eventually takes longer to serialize than the scrape
interval allows.

Logs have the opposite shape — write-and-forget, indexed elsewhere, cardinality is free. So the
raw path belongs there, and per-request debuggability is not lost.

`chi.RouteContext(r).RoutePattern()` gives the pattern; `r.URL.Path` in a label is the bug.

### Exposure

`/metrics` binds to a **separate admin port**, not the public listener. It leaks internal route
structure, request volumes, and error rates, and it has no business being reachable from the
internet. Same port serves `/healthz` and `/readyz`.

Grafana and Prometheus are provisioned in `docker compose` with a dashboard checked into the
repo, so `docker compose up` gives a reviewer working graphs rather than a scrape endpoint and
homework.

### What is not logged

A notes service is full of things that must never reach a log line: **note titles and bodies,
email addresses, JWTs, password hashes, and presigned URLs** (a logged presigned URL is a
credential sitting in your log aggregator until its TTL expires). Logging is by explicit field,
never by struct dump, so this is enforced by construction rather than by discipline.

### The blind spot the attachment design creates

Attachment bytes never touch this service — the client fetches them directly from object
storage. So downloads are **invisible to our metrics entirely**: bytes served, object 404s,
bandwidth, and per-file popularity exist only in S3 or CloudFront access logs.

This is the correct tradeoff (it is the entire point of not proxying bytes) but it is a real
gap, not a non-issue. What we *can* see is presign issuance: `attachment_presign_total{op}`
tells us intent to download even when the download itself is unobservable. Actual delivery
metrics come from the storage layer's own logs, which is a separate pipeline and out of scope.

### Out of scope

Distributed tracing (OpenTelemetry), log shipping, alerting rules, object-storage access logs.
All named in the README. With one service and one database, traces would show what the request
log already shows.

## 11. Cross-cutting concerns

Small things that are cheap to build and expensive to retrofit.

| Concern | Decision |
|---|---|
| **Graceful shutdown** | On SIGTERM, stop accepting, drain in-flight requests with a timeout, close the pool. Without it every deploy 502s whatever was in flight. |
| **Config** | Env vars, parsed and **validated at startup**, process exits on anything missing or malformed. Fail at boot, not on the first request that needs the value. |
| **`/healthz` vs `/readyz`** | Liveness is "the process is alive" — never touches the DB, or a database blip triggers a restart loop. Readiness is "can serve traffic" and does ping the DB. Conflating them is a classic outage amplifier. |
| **CORS** | Configurable allowed origins, default deny. No frontend today, but it is three lines and a browser client is the obvious next consumer. |
| **OpenAPI** | Hand-written `openapi.yaml` as the source of truth, served with Swagger UI. It is the contract; generating it from handlers makes the implementation the contract instead. |
| **Idempotency keys** | **Out of scope**, named. A double-submitted `POST /notes` creates two notes today. An `Idempotency-Key` header plus a short-lived key table is the fix. |
| **Timestamps** | `timestamptz` everywhere, UTC, RFC 3339 on the wire. |

## 12. Deployment and release

Documented target rather than built (DISCUSSION §11): Terraform-provisioned single host,
Traefik for TLS, `docker save`/`scp`/`docker load` deploy, RDS Postgres, real S3. Locally,
`docker compose up` with Postgres + MinIO — the only production deltas are a connection string
and an S3 endpoint, which is the whole reason storage sits behind an interface.

Secrets currently come from `pass` into a generated `.env_prod`. Out of scope to change; the
ceiling on that approach (production secrets at rest on a laptop) is named in the README with
SSM/Secrets Manager as the upgrade.

The service is stateless and JWT-authenticated, so nothing here prevents running several
instances behind a load balancer. The only coordination point would be SSE fanout if tier 2 of
DISCUSSION §4 were built, which is why that section specifies pub/sub rather than in-process hub
state.

### Migrations

**goose**, plain SQL, embedded in the binary via `embed.FS` so the migration set and the code
that expects it ship as one artifact and cannot drift.

**Migrations run as a discrete deploy step, never on application startup.** Two reasons, both
of which bite in production and not in development:

1. With more than one instance, N processes race to migrate the same database on boot.
2. If the app migrates itself, a failed rollout has already changed the schema — you cannot
   roll the app back, because the old version now faces a schema it does not know.

**Zero-downtime changes use expand/contract**, because during any rollout both versions are
briefly live: add the new column → deploy code that writes both → backfill → deploy code that
reads the new one → drop the old. A migration that renames a column in one step is a migration
that breaks every request in flight.

### Release pipeline

Bash scripts, deliberately, not GitHub Actions:

```
test.sh      compose up deps → go test ./... (unit + integration)
migrate.sh   goose up against the target DB
deploy.sh    build → save → scp → load → migrate → compose up -d
smoke.sh     run the multi-user flows against the deployed base URL
```

**Why bash over a CI config for this deliverable:** a reviewer can read a forty-line shell
script and know exactly what happens. A GitHub Actions workflow they cannot run is decoration —
it demonstrates familiarity with YAML, not with deployment.

**The tradeoff, stated rather than hidden:** scripts run from a laptop have no audit trail and
depend on whatever is installed locally. The upgrade path is the same scripts invoked by CI —
which is exactly why the logic lives in the scripts and not in the workflow file, and is the
argument for writing them this way even when you do have CI.

Order matters: **migrate before the new version comes up**, and only run smoke flows against a
deployment that already passed them locally.

## 13. Still open

Nothing blocking. Remaining judgment calls, all reversible:

- **API versioning strategy.** `/v1` in the path is kept, but the real rule is additive-only
  changes within v1. Comparison of the four approaches is in CAPABILITIES.md. Worth a second
  look before the first breaking change, not before the first line of code.
- Whether the seed data should include a deliberately conflicting concurrent edit so a reviewer
  can see the 409 without setting it up themselves. Leaning yes — it demonstrates the design
  choice the README leads with.
