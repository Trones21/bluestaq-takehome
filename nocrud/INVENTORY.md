# Backend Inventory

Produced by the `nocrud-scaffold` skill, Phase 1. This is the checkpoint: check
it for a missed endpoint or a wrong rule before trusting the generated flows.

Sources merged: the route table in `internal/server/server.go` and each
handler's `Routes()`, plus the rules in `CAPABILITIES.md` and the handler
comments. There is no OpenAPI document yet, so this came from the source.

## Route inventory

Base URL `/v1`. Auth is `Authorization: Bearer <token>` on everything except
the two auth routes.

| Method | Path | Auth | Request | Success | Depends on |
|---|---|---|---|---|---|
| POST | `/auth/register` | no | `{email, password, display_name}` | 200 `{access_token, token_type, expires_at, user}` | — |
| POST | `/auth/login` | no | `{email, password}` | 200 same as register | a user |
| GET | `/users/me` | yes | — | 200 user | — |
| PATCH | `/users/me` | yes | `{display_name?, email?}` | 200 user | — |
| POST | `/users/me/password` | yes | `{current_password, new_password}` | 204 | — |
| DELETE | `/users/me` | yes | — | 204 | — |
| GET | `/users?email=` | yes | — | 200 user | target shares a team |
| GET | `/teams` | yes | — | 200 `{teams:[]}` | — |
| POST | `/teams` | yes | `{name}` | 201 team | — |
| GET | `/teams/{id}` | yes | — | 200 team | membership |
| PATCH | `/teams/{id}` | yes | `{name}` | 200 team | team admin |
| DELETE | `/teams/{id}` | yes | — | 204 | team admin |
| GET | `/teams/{id}/members` | yes | — | 200 `{members:[]}` | membership |
| POST | `/teams/{id}/members` | yes | `{user_id, role}` | 204 | team admin, target user |
| DELETE | `/teams/{id}/members/{userID}` | yes | — | 204 | team admin |
| POST | `/teams/{id}/leave` | yes | — | 204 | membership |
| POST | `/notes` | yes | `{title, body?, tags?}` | 201 note + `ETag` | — |
| GET | `/notes/{id}` | yes | — | 200 note + `ETag` | viewer |
| PATCH | `/notes/{id}` | yes | `{title?, body?, tags?}` + **`If-Match`** | 200 note + `ETag` | editor |
| DELETE | `/notes/{id}` | yes | — | 204 | owner |
| POST | `/notes/{id}/restore` | yes | — | 200 note | owner, note deleted |

**Not yet built** (flows deliberately omitted, not silently skipped): note
sharing (`/notes/{id}/shares`), list and search (`GET /notes`), attachments.

## Rules inventory

The part worth asserting. Each maps to a row in `CAPABILITIES.md`.

### Access

1. A stranger reading a private note gets **404, not 403** — a 403 confirms it exists.
2. A malformed note ID also gets 404, indistinguishable from "no such note".
3. A user grant makes the grantee a viewer or editor.
4. A team grant reaches every member and nobody else.
5. One note shared with **two disjoint teams** gives each member their own team's role.
6. When grants overlap, the **strongest wins** (user-viewer + team-editor → editor).
7. Ownership outranks any grant, including a weaker self-grant.
8. Losing team membership revokes note access immediately.
9. A team admin is **not** automatically a note editor.

### Concurrency

10. `PATCH` without `If-Match` → **428**, not 400.
11. `If-Match: *` → 428 (it means "any version", the unconditional overwrite this prevents).
12. A stale `If-Match` → **409** carrying the current representation.
13. The winning write survives; the loser's does not overwrite it.
14. Both quoted (`"1"`) and bare (`1`) versions are accepted.

### Lifecycle

15. Delete is soft: the note vanishes for shared users, including the owner's normal reads.
16. Restore is **owner-only**, and finds the note the read path hides.
17. Restoring a live note → 409.
18. A second delete → 404.
19. Editors may not delete or restore.

### Teams

20. The creator becomes admin.
21. A non-member gets 404 on a team; a member with the wrong role gets 403.
22. The last admin cannot leave, be removed, or be demoted → 409 on all three.
23. Deleting a team removes its share grants and revokes member access, leaving the owner's note intact.

### Accounts

24. Registration is case-insensitive on email → 409 on a case variant.
25. Login returns one message for wrong-password and unknown-account alike.
26. User lookup is exact-email and co-member scoped; anything else is 404.
27. Unknown JSON fields are rejected (400), so a client typo is not silently dropped.

## Framework adapter status

Backend is **Go**, not Django, so the three framework-specific pieces are
written fresh in `utils/provisioning.py` and `utils/api_client.py`:

| Concern | This project |
|---|---|
| DB provisioning | Postgres; `go run ./cmd/migrate up` against a fresh database per flow |
| Start the app | the compiled binary on `$APP_PORT` with `$DATABASE_URL` pointing at that database |
| Auth handshake | JWT bearer. `POST /v1/auth/register` returns `access_token`; sent as `Authorization: Bearer` |
