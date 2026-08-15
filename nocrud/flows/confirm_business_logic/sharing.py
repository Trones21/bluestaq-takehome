"""Who can see a note, and how that changes.

These are the rules that only exist with more than one user in play, which is
the reason this runner earns its place next to the Go test suite: it drives
several real authenticated clients against a real server over HTTP.
"""

from utils.api_client import new_api_client_with_new_random_user


def private_note_stays_private():
    """Rules 1-2: an invisible note is reported absent, not forbidden.

    404 rather than 403 on purpose. A 403 confirms the note exists, which turns
    ID enumeration into a way to learn about other teams' data.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    note = alice.create_object("notes", {"title": "Alice private", "body": "secret"})

    res = bob.expect_status(
        "GET", f"notes/{note['id']}", 404,
        why="a 403 would confirm the note exists",
    )
    assert "Alice private" not in res.text, "the 404 body leaked the note title"

    # A malformed id must be indistinguishable from a real one you cannot see.
    bob.expect_status("GET", "notes/not-a-uuid", 404, why="malformed ids look absent too")

    # Writes are refused the same way -- 404, not 403, since Bob may not even
    # learn the note is there.
    bob.expect_status(
        "PATCH", f"notes/{note['id']}", 404,
        data={"body": "hijack"}, headers={"If-Match": "1"},
    )
    return "private notes are invisible and indistinguishable from missing ones"


def granting_and_revoking_access():
    """Rule 3 and revocation: a user grant turns 404 into 200 and back."""
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    note = alice.create_object("notes", {"title": "Shared with Bob", "body": "hello"})
    note_id = note["id"]

    bob.expect_status("GET", f"notes/{note_id}", 404, why="before the grant")

    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "user", "principal_id": bob.user_id,
                    "role": "viewer"}).raise_for_status()

    seen = bob.get_object_by_id("notes", note_id, silent=True)
    assert seen["your_role"] == "viewer", f"role was {seen['your_role']}"

    # A viewer may read but not write. 403 here, not 404: Bob can already see
    # the note, so its existence is not a secret from him.
    bob.expect_status(
        "PATCH", f"notes/{note_id}", 403,
        data={"body": "nope"}, headers={"If-Match": str(seen["version"])},
        why="viewer may read but not write",
    )

    # Promotion to editor takes effect immediately.
    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "user", "principal_id": bob.user_id,
                    "role": "editor"}).raise_for_status()
    bob.expect_status(
        "PATCH", f"notes/{note_id}", 200,
        data={"body": "bob edited"}, headers={"If-Match": str(seen["version"])},
    )

    # Revoking takes it away just as immediately.
    alice.raw("DELETE", f"notes/{note_id}/shares/user/{bob.user_id}").raise_for_status()
    bob.expect_status("GET", f"notes/{note_id}", 404, why="after revocation")

    return "grants and revocations take effect immediately"


def editors_cannot_reshare():
    """An editor may change the note but not the guest list.

    Re-sharing rights are the usual source of accidental exposure, so the
    access graph is kept one hop deep: only the owner grants.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")
    carol = new_api_client_with_new_random_user("carol")

    note = alice.create_object("notes", {"title": "Editable", "body": "x"})
    note_id = note["id"]

    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "user", "principal_id": bob.user_id,
                    "role": "editor"}).raise_for_status()

    bob.expect_status(
        "PUT", f"notes/{note_id}/shares", 403,
        data={"principal_type": "user", "principal_id": carol.user_id, "role": "viewer"},
        why="editors must not be able to re-share",
    )
    carol.expect_status("GET", f"notes/{note_id}", 404, why="carol was never granted access")

    # But a viewer can see who else has access -- knowing who can read your
    # note matters more than hiding the list from people already on it.
    shares = bob.get(f"notes/{note_id}/shares", silent=True)
    assert len(shares["shares"]) == 1, shares
    return "editors may edit but not re-share; viewers can see the guest list"


def cross_team_sharing():
    """Rules 4, 5 and 9 -- the inference the whole schema rests on.

    "Shared amongst several small teams" taken to mean a note can cross a team
    boundary. One note, two disjoint teams, each member resolving to their own
    team's role, and an outsider seeing nothing.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")
    carol = new_api_client_with_new_random_user("carol")
    dave = new_api_client_with_new_random_user("dave")  # in neither team

    platform = alice.create_object("teams", {"name": "Platform"})
    design = alice.create_object("teams", {"name": "Design"})

    alice.post(f"teams/{platform['id']}/members",
               {"user_id": bob.user_id, "role": "member"}, silent=True)
    # Carol is made an ADMIN of design, to prove team admin is not note power.
    alice.post(f"teams/{design['id']}/members",
               {"user_id": carol.user_id, "role": "admin"}, silent=True)

    note = alice.create_object("notes", {"title": "Cross-team spec", "body": "shared"})
    note_id = note["id"]

    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "team", "principal_id": platform["id"],
                    "role": "editor"}).raise_for_status()
    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "team", "principal_id": design["id"],
                    "role": "viewer"}).raise_for_status()

    bob_view = bob.get_object_by_id("notes", note_id, silent=True)
    assert bob_view["your_role"] == "editor", bob_view["your_role"]

    carol_view = carol.get_object_by_id("notes", note_id, silent=True)
    assert carol_view["your_role"] == "viewer", (
        f"design admin resolved as {carol_view['your_role']} -- team admin is "
        "membership management, not note permission"
    )
    carol.expect_status(
        "PATCH", f"notes/{note_id}", 403,
        data={"body": "x"}, headers={"If-Match": str(carol_view["version"])},
        why="a team admin on a viewer grant still may not edit",
    )

    dave.expect_status("GET", f"notes/{note_id}", 404, why="in neither team")
    return "one note, two teams, per-team roles, no leakage to outsiders"


def strongest_grant_wins():
    """Rule 6: overlapping grants resolve to the strongest.

    The case that breaks if roles are ever compared as text -- "viewer" sorts
    above "editor", so a max() over names would quietly demote this to viewer.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    team = alice.create_object("teams", {"name": "Platform"})
    alice.post(f"teams/{team['id']}/members",
               {"user_id": bob.user_id, "role": "member"}, silent=True)

    note = alice.create_object("notes", {"title": "Overlapping grants", "body": "x"})
    note_id = note["id"]

    # Bob is a viewer personally and an editor through his team.
    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "user", "principal_id": bob.user_id,
                    "role": "viewer"}).raise_for_status()
    alice.raw("PUT", f"notes/{note_id}/shares",
              data={"principal_type": "team", "principal_id": team["id"],
                    "role": "editor"}).raise_for_status()

    view = bob.get_object_by_id("notes", note_id, silent=True)
    assert view["your_role"] == "editor", (
        f"overlapping grants resolved as {view['your_role']} -- the weaker grant won"
    )

    # And losing the team drops him back to his personal grant rather than to
    # nothing, because the two grants are independent.
    alice.raw("DELETE", f"teams/{team['id']}/members/{bob.user_id}").raise_for_status()
    view = bob.get_object_by_id("notes", note_id, silent=True)
    assert view["your_role"] == "viewer", view["your_role"]
    return "strongest matching grant wins; independent grants survive each other"


def sharing_with_a_foreign_team_is_refused():
    """You may only share with a team you belong to.

    Otherwise anyone could push a note into an arbitrary team's view, and team
    ids would be probeable through the difference between 404 and 204.
    """
    alice = new_api_client_with_new_random_user("alice")
    mallory = new_api_client_with_new_random_user("mallory")

    team = alice.create_object("teams", {"name": "Alice's team"})
    note = mallory.create_object("notes", {"title": "Unsolicited", "body": "spam"})

    mallory.expect_status(
        "PUT", f"notes/{note['id']}/shares", 404,
        data={"principal_type": "team", "principal_id": team["id"], "role": "viewer"},
        why="cannot share into a team you are not in, and its existence is not confirmed",
    )
    return "sharing into a foreign team is refused without confirming it exists"
