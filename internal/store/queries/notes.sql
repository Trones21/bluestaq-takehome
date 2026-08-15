-- Note queries.
--
-- Columns are listed explicitly rather than using `select *`: notes carries a
-- generated search_tsv, and selecting it would drag the full-text vector of
-- every note across the wire for no reason.

-- name: CreateNote :one
insert into notes (owner_user_id, title, body, tags)
values ($1, $2, $3, $4)
returning id, owner_user_id, title, body, tags, version, created_at, updated_at, deleted_at;

-- GetNoteWithAccess fetches a note together with the caller's effective role,
-- in one statement.
--
-- One statement rather than fetch-then-check on purpose: a separate
-- authorization query is a time-of-check-to-time-of-use gap, and it doubles
-- the round trips on the hottest path in the service.
--
-- The role is returned as an integer rank, not a name. Ordering role *names*
-- in SQL sorts alphabetically, and "viewer" > "editor" -- a max() over names
-- would silently promote every viewer on a note that carries both grants.
--
-- deleted_at is returned rather than filtered. Callers decide: a read must
-- treat a deleted note as absent, but restore has to find it.
--
-- name: GetNoteWithAccess :one
select
    n.id, n.owner_user_id, n.title, n.body, n.tags, n.version,
    n.created_at, n.updated_at, n.deleted_at,
    (case
        when n.owner_user_id = sqlc.arg('user_id') then 3
        else coalesce((
            select max(case s.role
                        when 'editor' then 2
                        when 'viewer' then 1
                        else 0
                       end)
            from note_shares s
            where s.note_id = n.id
              and (
                    (s.principal_type = 'user' and s.principal_id = sqlc.arg('user_id'))
                 or (s.principal_type = 'team' and s.principal_id in (
                        select tm.team_id
                        from team_members tm
                        where tm.user_id = sqlc.arg('user_id')
                    ))
              )
        ), 0)
    end)::int as effective_role
from notes n
where n.id = sqlc.arg('note_id');

-- UpdateNote applies a partial update under optimistic concurrency.
--
-- The version predicate is what makes this safe: check and write are one
-- atomic statement, so no explicit lock or transaction is needed. Zero rows
-- affected means someone else committed first (or the note was deleted), and
-- the caller re-reads to tell those apart.
--
-- name: UpdateNote :one
update notes
set title      = coalesce(sqlc.narg('title'), title),
    body       = coalesce(sqlc.narg('body'), body),
    tags       = coalesce(sqlc.narg('tags'), tags),
    version    = version + 1,
    updated_at = now()
where id = sqlc.arg('id')
  and version = sqlc.arg('expected_version')
  and deleted_at is null
returning id, owner_user_id, title, body, tags, version, created_at, updated_at, deleted_at;

-- Soft delete. The note stays queryable for its owner and disappears for
-- everyone it was shared with.
-- name: SoftDeleteNote :execrows
update notes
set deleted_at = now(), updated_at = now(), version = version + 1
where id = $1 and deleted_at is null;

-- name: RestoreNote :one
update notes
set deleted_at = null, updated_at = now(), version = version + 1
where id = $1 and deleted_at is not null
returning id, owner_user_id, title, body, tags, version, created_at, updated_at, deleted_at;

-- name: CountNoteAttachments :one
select count(*) from attachments
where note_id = $1 and status = 'ready';
