-- +goose Up

-- citext gives case-insensitive email uniqueness without every query
-- remembering to lower(). pgcrypto supplies gen_random_uuid().
create extension if not exists citext;
create extension if not exists pgcrypto;

create table users (
    id            uuid primary key default gen_random_uuid(),
    email         citext      not null,
    password_hash text        not null,
    display_name  text        not null check (length(display_name) between 1 and 128),
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),
    -- Soft delete. In a shared notes service the notes are not only yours, so
    -- hard-deleting a user would destroy other people's working context.
    deleted_at    timestamptz
);

-- Uniqueness applies only to live accounts: a deleted user must not block
-- the address forever, but two live accounts must never share one.
create unique index users_email_live_uniq on users (email) where deleted_at is null;

create table teams (
    id         uuid primary key default gen_random_uuid(),
    name       text        not null check (length(name) between 1 and 128),
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

create table team_members (
    team_id    uuid        not null references teams (id) on delete cascade,
    user_id    uuid        not null references users (id) on delete cascade,
    role       text        not null check (role in ('member', 'admin')),
    created_at timestamptz not null default now(),
    primary key (team_id, user_id)
);

-- "Which teams am I in" runs on every note read, so it gets its own index
-- rather than relying on the primary key's leading column.
create index team_members_user_idx on team_members (user_id);

create table notes (
    id            uuid primary key default gen_random_uuid(),
    -- No team_id: a note has no home team. It belongs to its owner; teams are
    -- only ever grantees. This is what makes cross-team sharing two rows in
    -- note_shares rather than a schema change.
    owner_user_id uuid        not null references users (id),
    title         text        not null check (length(title) between 1 and 512),
    body          text        not null default '',
    tags          text[]      not null default '{}',
    -- Incremented on every update; surfaced as an ETag and required back as
    -- If-Match. Without it, concurrent edits silently destroy each other.
    version       integer     not null default 1,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),
    deleted_at    timestamptz
);

create index notes_owner_idx on notes (owner_user_id) where deleted_at is null;
-- Keyset pagination sorts on (updated_at desc, id desc); the index matches
-- that ordering exactly so the scan never sorts.
create index notes_updated_idx on notes (updated_at desc, id desc) where deleted_at is null;
create index notes_tags_idx on notes using gin (tags);

-- Generated rather than trigger-maintained, so it can never drift from the
-- columns it summarises. Title outranks body: a word in the title is a
-- stronger signal than the same word buried in a paragraph.
alter table notes
    add column search_tsv tsvector generated always as (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(body, '')), 'B')
    ) stored;

create index notes_search_idx on notes using gin (search_tsv);

create table note_shares (
    note_id        uuid        not null references notes (id) on delete cascade,
    -- Polymorphic: principal_id points at users or teams depending on
    -- principal_type, which Postgres cannot express as a foreign key. The
    -- consequence is real -- deleting a user or team orphans its grants, and
    -- an orphan would resolve against a reused UUID -- so both deletions
    -- remove matching rows explicitly, in the same transaction. See SPEC 3.
    principal_type text        not null check (principal_type in ('user', 'team')),
    principal_id   uuid        not null,
    role           text        not null check (role in ('viewer', 'editor')),
    granted_by     uuid        not null references users (id),
    created_at     timestamptz not null default now(),
    primary key (note_id, principal_type, principal_id)
);

-- The primary key serves "who can see this note"; this index serves the
-- opposite direction, "what can I see", which is the list/search hot path.
create index note_shares_principal_idx on note_shares (principal_type, principal_id);

create table attachments (
    id          uuid primary key default gen_random_uuid(),
    note_id     uuid        not null references notes (id) on delete cascade,
    object_key  text        not null unique,
    filename    text        not null check (length(filename) between 1 and 255),
    mime_type   text        not null,
    size_bytes  bigint      not null check (size_bytes > 0),
    checksum    text,
    -- pending until the client has actually uploaded and we have verified the
    -- object. Rows stuck here are abandoned uploads, swept separately.
    status      text        not null check (status in ('pending', 'ready')),
    uploaded_by uuid        not null references users (id),
    created_at  timestamptz not null default now(),
    ready_at    timestamptz
);

create index attachments_note_ready_idx on attachments (note_id) where status = 'ready';
-- Supports the orphan sweep: find pending uploads older than the cutoff.
create index attachments_pending_idx on attachments (created_at) where status = 'pending';

-- +goose Down
drop table if exists attachments;
drop table if exists note_shares;
drop table if exists notes;
drop table if exists team_members;
drop table if exists teams;
drop table if exists users;
