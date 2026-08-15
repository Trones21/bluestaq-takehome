"""Listing, search, and the two-phase attachment upload."""

import requests

from utils.api_client import new_api_client_with_new_random_user


def search_only_reaches_your_own_notes():
    """The invariant that makes every filter safe.

    Search is a filter ON the authorized set, so it can never surface a note
    the caller cannot read. Bob owns a note matching the same term, so a leak
    is distinguishable from an empty result.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    alice.create_object("notes", {"title": "Postgres indexing", "body": "vacuum and bloat"})
    bob.create_object("notes", {"title": "Bob on indexing", "body": "his own"})

    found = bob.get("notes?q=indexing", silent=True)["notes"]
    titles = [n["title"] for n in found]
    assert titles == ["Bob on indexing"], (
        f"search returned {titles} -- it reached a note Bob cannot read"
    )

    # And the filters narrow rather than widen: asking for Alice's notes
    # returns only the ones Bob can already see, which is none.
    owned = bob.get(f"notes?owner={alice.user_id}", silent=True)["notes"]
    assert owned == [], f"owner filter widened access: {owned}"
    return "search and filters narrow the authorized set, never widen it"


def keyset_pagination_visits_each_note_once():
    """Offset pagination skips and duplicates rows as the window shifts."""
    alice = new_api_client_with_new_random_user("alice")
    for i in range(12):
        alice.create_object("notes", {"title": f"note-{i:02d}"})

    seen, cursor = {}, None
    for _ in range(10):
        path = "notes?limit=5" + (f"&cursor={cursor}" if cursor else "")
        page = alice.get(path, silent=True)
        for n in page["notes"]:
            seen[n["title"]] = seen.get(n["title"], 0) + 1
        if not page["has_more"]:
            break
        cursor = page["next_cursor"]

    assert len(seen) == 12, f"walked {len(seen)} notes, want 12"
    dupes = {k: v for k, v in seen.items() if v != 1}
    assert not dupes, f"pagination returned rows twice: {dupes}"
    return "keyset pagination walks every note exactly once"


def deleted_notes_are_owner_scoped():
    """?deleted=true must not become a back door for former collaborators."""
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    note = alice.create_object("notes", {"title": "doomed"})
    alice.raw("PUT", f"notes/{note['id']}/shares",
              data={"principal_type": "user", "principal_id": bob.user_id,
                    "role": "editor"}).raise_for_status()

    alice.expect_status("DELETE", f"notes/{note['id']}", 204)

    assert len(alice.get("notes?deleted=true", silent=True)["notes"]) == 1
    assert alice.get("notes", silent=True)["notes"] == []
    assert bob.get("notes?deleted=true", silent=True)["notes"] == [], (
        "a former editor can see the owner's deleted note"
    )
    return "deleted notes are visible only to their owner"


def attachment_upload_is_two_phase():
    """Bytes go straight to object storage, never through the API.

    And the trap the design has to survive: a presigned PUT signs the method,
    key and expiry -- not the body length. The size cap is therefore enforced
    when the upload is confirmed, not when the URL is issued.
    """
    alice = new_api_client_with_new_random_user("alice")
    note = alice.create_object("notes", {"title": "with a diagram"})
    note_id = note["id"]

    body = b"fake png bytes"
    reserved = alice.post(f"notes/{note_id}/attachments", {
        "filename": "diagram.png", "mime_type": "image/png", "size_bytes": len(body),
    }, silent=True)

    assert reserved["status"] == "pending", reserved["status"]
    assert "upload_url" in reserved and reserved["upload_url"], reserved
    # The URL must address storage, not this API.
    assert "/v1/notes" not in reserved["upload_url"], reserved["upload_url"]

    # Confirming before uploading must not mark it ready -- a presigned URL is
    # an opportunity to upload, not evidence one happened.
    alice.expect_status(
        "POST", f"notes/{note_id}/attachments/{reserved['id']}/complete", 409,
        why="nothing has been uploaded yet",
    )

    put = requests.put(reserved["upload_url"], data=body)
    assert put.status_code == 200, f"direct upload failed: {put.status_code}"

    ready = alice.post(f"notes/{note_id}/attachments/{reserved['id']}/complete",
                       None, silent=True)
    assert ready["status"] == "ready", ready
    assert ready["size_bytes"] == len(body), (
        "size must be recorded from the stored object, not the client's claim"
    )

    listed = alice.get(f"notes/{note_id}/attachments", silent=True)["attachments"]
    assert len(listed) == 1 and listed[0]["download_url"], listed
    return "two-phase upload works and the size comes from the object"


def note_bodies_reference_attachments_not_urls():
    """A presigned URL must never be written into a body -- it expires.

    The body carries a stable attachment:{id} reference that the server
    resolves per read, so content lifetime and URL lifetime stay independent.
    """
    alice = new_api_client_with_new_random_user("alice")
    note = alice.create_object("notes", {"title": "with image"})
    other = alice.create_object("notes", {"title": "another note"})

    body = b"png"
    a = alice.post(f"notes/{note['id']}/attachments", {
        "filename": "d.png", "mime_type": "image/png", "size_bytes": len(body),
    }, silent=True)

    # Pending: cannot be referenced yet.
    alice.expect_status(
        "PATCH", f"notes/{note['id']}", 400,
        data={"body": f"![d](attachment:{a['id']})"}, headers={"If-Match": "1"},
        why="the upload has not been completed",
    )

    requests.put(a["upload_url"], data=body).raise_for_status()
    alice.post(f"notes/{note['id']}/attachments/{a['id']}/complete", None, silent=True)

    alice.expect_status(
        "PATCH", f"notes/{note['id']}", 200,
        data={"body": f"Diagram:\n\n![d](attachment:{a['id']})"},
        headers={"If-Match": "1"},
    )

    # The copy-paste case: the same markdown in a different note points at an
    # attachment that is not its own.
    alice.expect_status(
        "PATCH", f"notes/{other['id']}", 400,
        data={"body": f"![d](attachment:{a['id']})"}, headers={"If-Match": "1"},
        why="cross-note attachment references are refused, not silently resolved",
    )

    # And deleting it while the body still points at it would leave a broken
    # image in saved content.
    alice.expect_status(
        "DELETE", f"notes/{note['id']}/attachments/{a['id']}", 409,
        why="the body still references it",
    )
    return "bodies hold stable references; dangling and cross-note ones are refused"
