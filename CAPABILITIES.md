# Capability Matrix

Every "can it do X" question, answered. **This doubles as the test manifest** — each row with a
permission answer becomes an assertion, so the behavior list and the test list cannot drift
apart.

**Test column:** `unit` = table-driven authorization test · `flow` = multi-user request flow ·
`int` = integration test against real Postgres/MinIO · `—` = not built

---

## Access & sharing

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Can a user read another user's private note? | **No** | No grant → `effective_role` = none → 404 | unit, flow |
| Can they even learn it exists? | **No** | 404 not 403 on no-read (DISCUSSION §8) | unit |
| Does a note belong to a team? | **No** | Notes have an owner; teams are only grantees | — |
| Can a note exist with no team at all? | **Yes** | Zero grants = private note | flow |
| Can a note be shared with two teams? | **Yes** | Two rows in `note_shares` | flow |
| Can it be shared with a user outside all my teams? | **Yes** | `principal_type = 'user'` grant | flow |
| Can a user belong to multiple teams? | **Yes** | `team_members` is many-to-many | int |
| Can a non-owner share a note? | **No** | Share/revoke is owner-only | unit, flow |
| Can an editor re-share? | **No** | Deliberate — keeps the access graph one hop deep | unit |
| Does team admin grant note access? | **No** | `team_members.role` governs membership only; access comes from the grant | unit |
| Can a viewer see who else a note is shared with? | **Yes** | `GET /shares` needs viewer — knowing who can see your note matters | int |
| Can a note be public to all authenticated users? | **No — one enum value away** | Add `principal_type = 'everyone'`; no migration of existing rows | — |
| Public to the unauthenticated internet? | **No — out of scope** | Needs a share token + unauth read path + different threat model | — |
| Can I enumerate users? | **No** | Lookup is exact-email only, scoped to co-members | int |
| Can a filter widen my access? | **No** | Every filter narrows the already-authorized set (§ below) | unit |

## Concurrent editing

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Can two people edit the same note at once? | **Yes** | Nobody is locked out | flow |
| Can one silently overwrite the other? | **No** | `If-Match` → 409 on stale version | int, flow |
| Is the note locked while someone edits? | **No** | Optimistic, not pessimistic | — |
| Do I get a conflict if we edit *different fields*? | **Yes — spurious 409** | Versioning is per-note, not per-field. Known limit; fix needs version history (DISCUSSION §4) | int |
| Does the client get enough to merge? | **Yes** | 409 body carries the current representation | int |
| Real-time sync / presence? | **No** | Tier 2, needs SSE + pub/sub fanout | — |
| Live collaborative cursors (Google-Docs style)? | **No** | Tier 3, CRDT — rewrites the body storage path | — |
| Is `If-Match` optional? | **No** | Missing → 428 Precondition Required | int |

## Notes lifecycle

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Are deletes recoverable? | **Yes** | Soft delete; `POST /notes/{id}/restore` | int |
| Who can delete / restore? | **Owner only** | Matches delete; an editor undoing an owner's delete is surprising | unit |
| Do deleted notes vanish for people they were shared with? | **Yes** | `deleted_at IS NULL` on all read paths | int |
| Can I see *other people's* deleted notes I had access to? | **No** | `?deleted=true` is owner-scoped | unit |
| Is there version history / can I see past revisions? | **No** | First follow-up; also the blocker for field-level merge | — |
| Can I transfer ownership? | **No** | Needed for offboarding — see below | — |
| Empty title allowed? | **No** | Non-empty, ≤512 chars | int |

## Deletion & offboarding

The interesting one, because "delete a user" is three different operations.

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Can I delete my own account? | **Yes** | Soft delete; tokens revoked | int |
| Do my notes die with me? | **No** | Notes survive; owner renders as a tombstone. In a *shared* service the notes aren't only yours | int |
| Can an admin offboard someone who left the company? | **No — out of scope** | Requires org-level admin + ownership transfer. **The model has no org**, only teams | — |
| Can a GDPR erasure be honored? | **Partially** | Soft delete hides the user; true erasure needs an anonymize-and-retain path (note content may be a business record) | — |
| Deleting a team — what happens to its notes? | **Nothing** | Notes are owner-owned; only the team's grants are removed | int |
| Do orphaned share rows linger? | **No** | No FK on `principal_id`, so user/team deletion explicitly deletes matching shares **in the same transaction** | int |
| Can a team lose its last admin? | **No** | Blocked on remove, demote, and leave | int |
| Can a note become unreachable (dead owner, no live grants)? | **Yes — known gap** | A real system needs an admin recovery path. Named, not built | — |

## Search, filtering, pagination

**Invariant:** every filter narrows the set the caller can already read. Filters never widen
access. `?owner=bob` returns Bob's notes *I can already see* — not all of Bob's notes.

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Full-text search over title and body? | **Yes** | `tsvector` + GIN, title weighted above body | int |
| Can search surface a note I can't read? | **No** | Search is a filter *on* the authorized set | unit, flow |
| Notes owned by a specific user? | **Yes** | `?owner={id}` (replaces `owned_by_me`) | int |
| Notes shared with a specific team? | **Yes** | `?team={id}` | int |
| Only notes shared *with* me, excluding mine? | **Yes** | `?shared_with_me=true` | int |
| Filter by tag? | **Yes** | `?tag=` repeatable, AND semantics, GIN on `text[]` | int |
| Combine filters? | **Yes** | All compose as AND against the authorized set | int |
| Stable pagination? | **Mostly — see below** | Keyset on `(updated_at, id)` | int |
| Sort options? | **Two** | `updated_at DESC` default; relevance when `q` is present | int |

**Honest note on pagination:** keyset eliminates the offset-drift problem (offset pagination
skips and duplicates rows on every concurrent insert). It does **not** make the scan a snapshot —
a note edited mid-pagination changes its sort key and can still be seen twice or missed. That is
acceptable for a recency-ordered feed. A perfectly stable page requires sorting on an immutable
key (`created_at, id`) or a snapshot cursor. Worth saying precisely rather than claiming
"stable."

## Attachments

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Images and video? | **Yes** | MIME allowlist: `image/*`, `video/mp4`, `application/pdf` | int |
| Do bytes go through the API? | **No** | Presigned PUT direct to object storage | int |
| Who can upload / download? | **Editor / viewer** | Same `effective_role` as the parent note | unit |
| Can I upload a 10GB file? | **No** | 100 MiB, verified at `/complete` — presigned PUT can't enforce size (DISCUSSION §6) | int |
| Can a leaked download URL be revoked? | **No — known gap** | Valid until TTL expires, independent of revoke. Bounded by short TTL | — |
| Abandoned uploads cleaned up? | **Yes** | `pending` rows + objects swept after 24h, as a command not a scheduler | int |
| Does an attachment survive its note's deletion? | **No** | `ON DELETE CASCADE` on `note_id` (real FK here) | int |
| Can an image render inline at a position in the note? | **Yes** | Markdown `![alt](attachment:{id})`; the body *is* the position, no position column | int |
| Are presigned URLs stored in the note body? | **No — never** | They expire; the body would rot. Body holds a stable ref, server resolves per read | int |
| Can a note reference a nonexistent attachment? | **No** | Body parsed on write; refs must exist, be `ready`, and belong to this note → 400 | int |
| Can I reference another note's attachment? | **No** | Same validation. Auto-copying across notes is out of scope | int |
| Can I delete an attachment the body still references? | **No** | 409 listing the referencing positions | int |
| Are unreferenced attachments deleted? | **No** | A non-inlined file attachment is legitimate; the sweep targets abandoned *uploads* only | int |
| Do images break while a note is open? | **No, for an hour** | GET presign TTL 1h vs 15m for uploads | — |
| Can we observe attachment downloads? | **No — inherent gap** | Bytes never touch the service; only presign issuance is visible (§10) | — |

## Operational

| Question | Answer | Mechanism | Test |
|---|---|---|---|
| Per-endpoint request timings? | **Yes** | Prometheus histogram, labelled by **route pattern** not raw path | int |
| Can I see error rates by endpoint? | **Yes** | `http_requests_total{route,status}` + provisioned Grafana dashboard | — |
| Is `/metrics` public? | **No** | Separate admin port; it leaks route structure and volumes | int |
| Are note contents ever logged? | **No** | Explicit-field logging only — bodies, titles, emails, tokens, presigned URLs never logged | unit |
| Can I correlate log lines for one request? | **Yes** | Request ID middleware, echoed in the response header | int |
| Distributed tracing? | **No** | One service, one DB — the request log says what a trace would | — |
| Does a deploy drop in-flight requests? | **No** | Graceful shutdown drains on SIGTERM | — |
| Does a DB blip restart the process? | **No** | `/healthz` never touches the DB; only `/readyz` does | int |
| Do migrations run on app startup? | **No** | Discrete step — avoids multi-instance races and un-rollbackable deploys | — |
| Is a double-submitted create idempotent? | **No — named gap** | Needs an `Idempotency-Key` header + key table | — |
| Rate limiting? | **No** | Edge/shared-state concern (DISCUSSION §5) | — |
| Request body cap, timeouts, bounded pool? | **Yes** | These are survival, not policy | int |
| Audit log of who read what? | **No — real gap** | Genuinely wanted for a shared-access system. Named in README | — |
| Can it run multi-instance? | **Yes** | Stateless, JWT auth, no in-process session state | — |
| Is login protected from brute force? | **No — real gap** | Needs failed-attempt throttling on account+source. Not an edge concern: a WAF can't distinguish a failed login from a successful one | — |
| Token revocation / refresh tokens? | **No** | Short-lived access tokens; production delegates to an IdP | — |
| Password reset flow? | **No** | Needs email infrastructure | — |
| Can a reviewer run it? | **Yes** | `docker compose up` + seed script + `/docs` | — |

---

## API versioning — the open question

There is no industry consensus, and the choice matters far less than *having* a rule.

| Approach | Who | Tradeoff |
|---|---|---|
| **URL path** (`/v1/notes`) | Most common | Visible in logs and curl, trivial to route and cache. Purists object that a URI should name the resource, not its representation. Tempts big-bang v2 rewrites. |
| **Header** (`Accept: …+v2`) | Cleaner in theory | Invisible in logs and browsers, harder to route at the proxy, easy to omit by accident. |
| **Date-based** (`API-Version: 2026-06-20`) | Stripe | Genuinely best for many third-party consumers pinned at different points — but needs a payload transform chain between versions. Heavy machinery. |
| **None, additive-only** | Many internal APIs | Never remove or repurpose a field; only add optional ones. Works better than it sounds. |

**Decision: keep `/v1` in the path, but the actual strategy is additive-only within v1.** The
prefix costs nothing and signals that evolution was considered; `v2` would exist only for a
genuinely breaking change to the resource model.

Worth saying out loud: for an internal service with one frontend you deploy alongside it,
versioning is close to ceremonial. It earns its keep when there are third-party consumers you
cannot force to upgrade.

---

## Using this as the test plan

Rows marked `flow` are the multi-user scenarios — the ones that need two or three authenticated
clients acting against each other, which is where the authorization rules actually get
interesting. Rows marked `unit` are the pure `effective_role` matrix. Rows marked `—` are the
scope boundary, stated so a reviewer can see what was decided rather than forgotten.

**This file is kept current as the build proceeds.** A behavior change that does not show up
here is a behavior change nobody can review.
