-- name: CreateAttachment :one
insert into attachments (note_id, object_key, filename, mime_type, size_bytes, status, uploaded_by)
values ($1, $2, $3, $4, $5, 'pending', $6)
returning id, note_id, object_key, filename, mime_type, size_bytes, checksum, status, uploaded_by, created_at, ready_at;

-- MarkAttachmentReady closes the two-phase upload.
--
-- Conditional on status = 'pending', so completing twice is not an error the
-- second time round and cannot rewrite a verified size.
-- name: MarkAttachmentReady :one
update attachments
set status = 'ready', ready_at = now(), size_bytes = $2, checksum = $3
where id = $1 and status = 'pending'
returning id, note_id, object_key, filename, mime_type, size_bytes, checksum, status, uploaded_by, created_at, ready_at;

-- name: GetAttachment :one
select id, note_id, object_key, filename, mime_type, size_bytes, checksum, status, uploaded_by, created_at, ready_at
from attachments
where id = $1 and note_id = $2;

-- name: ListNoteAttachments :many
select id, note_id, object_key, filename, mime_type, size_bytes, checksum, status, uploaded_by, created_at, ready_at
from attachments
where note_id = $1 and status = 'ready'
order by created_at;

-- name: DeleteAttachment :execrows
delete from attachments where id = $1 and note_id = $2;

-- ListStaleUploads finds abandoned uploads: rows still pending long after the
-- presigned URL expired, because the client never finished.
--
-- Note this deliberately does NOT treat an unreferenced attachment as an
-- orphan. A file attached to a note without being inlined in the body is a
-- legitimate case; only an unfinished upload is garbage.
-- name: ListStaleUploads :many
select id, object_key
from attachments
where status = 'pending' and created_at < $1
order by created_at
limit $2;

-- name: DeleteAttachmentByID :execrows
delete from attachments where id = $1;
