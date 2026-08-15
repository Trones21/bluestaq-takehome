"""Two people editing one note.

The problem sharing creates. Without a precondition the second save silently
overwrites the first and nobody is told, which is the default most
implementations of this ship.
"""

from utils.api_client import new_api_client_with_new_random_user


def _shared_note(role="editor"):
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")
    note = alice.create_object("notes", {"title": "Shared doc", "body": "original"})
    alice.raw("PUT", f"notes/{note['id']}/shares",
              data={"principal_type": "user", "principal_id": bob.user_id,
                    "role": role}).raise_for_status()
    return alice, bob, note["id"]


def concurrent_edit_is_rejected_not_merged():
    """Rules 12-13: the second writer is refused and the winner's work survives."""
    alice, bob, note_id = _shared_note()

    # Both read the same version.
    alice_view = alice.get_object_by_id("notes", note_id, silent=True)
    bob_view = bob.get_object_by_id("notes", note_id, silent=True)
    assert alice_view["version"] == bob_view["version"]
    version = alice_view["version"]

    # Alice saves first and wins.
    alice.expect_status("PATCH", f"notes/{note_id}", 200,
                        data={"body": "alice's work"},
                        headers={"If-Match": str(version)})

    # Bob saves against the version he read, and is refused.
    res = bob.expect_status(
        "PATCH", f"notes/{note_id}", 409,
        data={"body": "bob's work"}, headers={"If-Match": str(version)},
        why="stale write must be refused, not applied",
    )

    # The 409 carries the current state, so a client can show a diff instead of
    # just an error.
    body = res.json()
    assert body.get("conflict", {}).get("body") == "alice's work", body
    assert body["conflict"]["version"] == version + 1, body

    # The whole point: Alice's work is still there.
    current = alice.get_object_by_id("notes", note_id, silent=True)
    assert current["body"] == "alice's work", (
        f"body is {current['body']!r} -- the losing write overwrote the winner"
    )
    return "stale writes are refused and the winning edit survives"


def precondition_is_mandatory():
    """Rules 10-11: no If-Match is 428, and a wildcard is refused."""
    alice, _, note_id = _shared_note()

    # 428, not 400: the request is well formed, it just refuses to proceed
    # unconditionally.
    alice.expect_status("PATCH", f"notes/{note_id}", 428,
                        data={"body": "x"},
                        why="unconditional writes are refused outright")

    # "*" means "any existing version", which is exactly the unconditional
    # overwrite this exists to prevent.
    alice.expect_status("PATCH", f"notes/{note_id}", 428,
                        data={"body": "x"}, headers={"If-Match": "*"},
                        why="wildcard is an unconditional overwrite")
    return "If-Match is required and wildcards are refused"


def quoted_and_bare_versions_both_work():
    """Rule 14: hand-written requests routinely send a bare number."""
    alice, _, note_id = _shared_note()

    alice.expect_status("PATCH", f"notes/{note_id}", 200,
                        data={"body": "one"}, headers={"If-Match": '"1"'})
    alice.expect_status("PATCH", f"notes/{note_id}", 200,
                        data={"body": "two"}, headers={"If-Match": "2"})
    return "both quoted entity-tag and bare version forms are accepted"


def soft_delete_hides_and_owner_restores():
    """Rules 15-19: a deleted note vanishes for everyone; only the owner recovers it."""
    alice, bob, note_id = _shared_note()

    bob.get_object_by_id("notes", note_id, silent=True)  # visible before

    # Editors may not delete or restore -- both are owner-only, and asymmetry
    # would let an editor undo an owner's deletion.
    bob.expect_status("DELETE", f"notes/{note_id}", 403, why="delete is owner-only")
    bob.expect_status("POST", f"notes/{note_id}/restore", 403, why="restore is owner-only")

    alice.expect_status("DELETE", f"notes/{note_id}", 204)

    bob.expect_status("GET", f"notes/{note_id}", 404, why="deleted notes vanish for shared users")
    alice.expect_status("GET", f"notes/{note_id}", 404, why="and for the owner's normal reads")
    alice.expect_status("DELETE", f"notes/{note_id}", 404, why="a second delete finds nothing")

    # Restore still finds it, despite the read path hiding it.
    alice.expect_status("POST", f"notes/{note_id}/restore", 200)
    bob.get_object_by_id("notes", note_id, silent=True)

    alice.expect_status("POST", f"notes/{note_id}/restore", 409,
                        why="restoring a live note is a conflict")
    return "soft delete hides from everyone; only the owner restores"
