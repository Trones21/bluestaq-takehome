-- name: CreateTeam :one
insert into teams (name) values ($1)
returning id, name, created_at, updated_at;

-- name: GetTeam :one
select id, name, created_at, updated_at from teams where id = $1;

-- name: UpdateTeam :one
update teams set name = $2, updated_at = now()
where id = $1
returning id, name, created_at, updated_at;

-- name: DeleteTeam :execrows
delete from teams where id = $1;

-- DeleteTeamShareGrants removes grants naming this team as a principal.
--
-- Required because note_shares.principal_id has no foreign key -- it points at
-- users or teams depending on principal_type, which Postgres cannot express --
-- so dropping the team cascades nothing. A grant left behind would resolve
-- against a reused UUID. Always runs in the same transaction as the delete.
-- name: DeleteTeamShareGrants :execrows
delete from note_shares
where principal_type = 'team' and principal_id = $1;

-- name: ListMyTeams :many
select t.id, t.name, t.created_at, t.updated_at, tm.role
from teams t
join team_members tm on tm.team_id = t.id
where tm.user_id = $1
order by t.name;

-- GetMembership is the authorization check for every team operation.
-- name: GetMembership :one
select role from team_members
where team_id = $1 and user_id = $2;

-- name: ListTeamMembers :many
select u.id, u.email, u.display_name, tm.role, tm.created_at
from team_members tm
join users u on u.id = tm.user_id
where tm.team_id = $1
order by tm.role, u.display_name;

-- name: AddTeamMember :one
insert into team_members (team_id, user_id, role)
values ($1, $2, $3)
on conflict (team_id, user_id) do update set role = excluded.role
returning team_id, user_id, role, created_at;

-- name: RemoveTeamMember :execrows
delete from team_members where team_id = $1 and user_id = $2;

-- name: SetMemberRole :execrows
update team_members set role = $3 where team_id = $1 and user_id = $2;

-- CountTeamAdmins backs the last-admin guard. Read inside the same
-- transaction as the removal or demotion it protects, so two concurrent
-- requests cannot each observe two admins and both proceed, leaving zero.
-- name: CountTeamAdmins :one
select count(*) from team_members where team_id = $1 and role = 'admin';
