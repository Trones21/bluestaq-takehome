-- name: CreateUser :one
insert into users (email, password_hash, display_name)
values ($1, $2, $3)
returning *;

-- Login and lookup paths filter deleted_at: a soft-deleted account must not
-- authenticate, and must not be findable.

-- name: GetUserByEmail :one
select * from users
where email = $1 and deleted_at is null;

-- name: GetUser :one
select * from users
where id = $1 and deleted_at is null;

-- GetUserForDisplay includes soft-deleted rows on purpose. A note owned by a
-- deleted user still has to render an owner, as a tombstone rather than a
-- blank.
-- name: GetUserForDisplay :one
select * from users
where id = $1;

-- name: UpdateUserProfile :one
update users
set display_name = coalesce(sqlc.narg('display_name'), display_name),
    email        = coalesce(sqlc.narg('email'), email),
    updated_at   = now()
where id = sqlc.arg('id') and deleted_at is null
returning *;

-- name: UpdateUserPassword :execrows
update users
set password_hash = $2, updated_at = now()
where id = $1 and deleted_at is null;

-- name: SoftDeleteUser :execrows
update users
set deleted_at = now(), updated_at = now()
where id = $1 and deleted_at is null;

-- DeleteUserShareGrants removes the grants that name this user as a
-- principal. This is the price of the polymorphic principal column: no foreign
-- key covers it, so no cascade fires, and an orphaned grant would resolve
-- against a reused UUID. Always runs in the same transaction as the delete.
-- name: DeleteUserShareGrants :execrows
delete from note_shares
where principal_type = 'user' and principal_id = $1;

-- FindUserByExactEmail backs share-target lookup. Exact match only, and
-- scoped to people the caller already shares a team with: a general user
-- directory is a data-harvesting surface, and enumeration costs nothing to
-- close at design time.
-- name: FindUserByExactEmail :one
select u.* from users u
where u.email = sqlc.arg('email')
  and u.deleted_at is null
  and (
    u.id = sqlc.arg('caller_id')
    or exists (
      select 1
      from team_members caller
      join team_members target on target.team_id = caller.team_id
      where caller.user_id = sqlc.arg('caller_id')
        and target.user_id = u.id
    )
  );
