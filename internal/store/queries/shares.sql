-- name: UpsertNoteShare :one
insert into note_shares (note_id, principal_type, principal_id, role, granted_by)
values ($1, $2, $3, $4, $5)
on conflict (note_id, principal_type, principal_id)
    do update set role = excluded.role, granted_by = excluded.granted_by
returning note_id, principal_type, principal_id, role, granted_by, created_at;

-- ListNoteShares resolves each principal's display name in the same query.
--
-- A left join per principal type rather than two round trips: a share list is
-- useless without names, and "who can see this note" is exactly the question a
-- viewer asks.
-- name: ListNoteShares :many
select
    s.principal_type,
    s.principal_id,
    s.role,
    s.created_at,
    coalesce(u.display_name, t.name, '(deleted)') as principal_name,
    u.email as principal_email
from note_shares s
left join users u on s.principal_type = 'user' and u.id = s.principal_id
left join teams t on s.principal_type = 'team' and t.id = s.principal_id
where s.note_id = $1
order by s.principal_type, principal_name;

-- name: DeleteNoteShare :execrows
delete from note_shares
where note_id = $1 and principal_type = $2 and principal_id = $3;

-- TeamExists and UserExists validate a grant target before it is written.
--
-- Needed because principal_id has no foreign key, so the database will happily
-- accept a grant to a UUID that names nothing. Without these, a typo becomes a
-- permanently dead grant row rather than a 404.
-- name: TeamExists :one
select exists(select 1 from teams where id = $1);

-- name: UserExists :one
select exists(select 1 from users where id = $1 and deleted_at is null);

-- IsTeamMember gates sharing with a team the granter does not belong to.
-- name: IsTeamMember :one
select exists(
    select 1 from team_members where team_id = $1 and user_id = $2
);
