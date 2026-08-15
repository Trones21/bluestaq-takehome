"""Team management and account rules."""

from utils.api_client import (
    FIXTURE_PASSWORD,
    APIClient,
    new_api_client_with_new_random_user,
)


def last_admin_cannot_strand_the_team():
    """Rule 22: a team with no admins can never be managed again.

    Three routes to the same hazard -- leaving, being removed, and being
    demoted -- so all three are checked.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    team = alice.create_object("teams", {"name": "Platform"})
    tid = team["id"]
    alice.post(f"teams/{tid}/members", {"user_id": bob.user_id, "role": "member"}, silent=True)

    alice.expect_status("POST", f"teams/{tid}/leave", 409, why="last admin leaving")
    alice.expect_status("DELETE", f"teams/{tid}/members/{alice.user_id}", 409,
                        why="last admin removing themselves")
    alice.expect_status("POST", f"teams/{tid}/members", 409,
                        data={"user_id": alice.user_id, "role": "member"},
                        why="last admin demoting themselves")

    # Once a second admin exists, the first may go -- and the survivor inherits
    # the same protection.
    alice.post(f"teams/{tid}/members", {"user_id": bob.user_id, "role": "admin"}, silent=True)
    alice.expect_status("POST", f"teams/{tid}/leave", 204)
    bob.expect_status("POST", f"teams/{tid}/leave", 409, why="bob is now the last admin")
    return "a team can never be left without an admin"


def team_membership_is_concealed_from_outsiders():
    """Rule 21: non-members get 404, members with the wrong role get 403.

    Team names leak organisational structure, so existence is hidden the same
    way note existence is. A member already knows the team exists, so an
    insufficient role is a plain 403.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")
    outsider = new_api_client_with_new_random_user("outsider")

    team = alice.create_object("teams", {"name": "Secret Project"})
    tid = team["id"]

    res = outsider.expect_status("GET", f"teams/{tid}", 404, why="non-member")
    assert "Secret Project" not in res.text, "the 404 leaked the team name"

    alice.post(f"teams/{tid}/members", {"user_id": bob.user_id, "role": "member"}, silent=True)
    bob.get(f"teams/{tid}", silent=True)  # now visible
    bob.expect_status("PATCH", f"teams/{tid}", 403, data={"name": "Renamed"},
                      why="member knows it exists, so 403 rather than 404")
    return "team existence is hidden from outsiders, not from members"


def deleting_a_team_revokes_its_grants():
    """Rule 23: the concrete cost of the polymorphic ACL.

    note_shares.principal_id has no foreign key -- it points at users or teams
    depending on principal_type -- so deleting a team cascades nothing. The
    grants must be removed explicitly, or an orphan would resolve against
    whoever next receives that UUID.
    """
    alice = new_api_client_with_new_random_user("alice")
    bob = new_api_client_with_new_random_user("bob")

    team = alice.create_object("teams", {"name": "Platform"})
    alice.post(f"teams/{team['id']}/members",
               {"user_id": bob.user_id, "role": "member"}, silent=True)

    note = alice.create_object("notes", {"title": "Team note", "body": "x"})
    alice.raw("PUT", f"notes/{note['id']}/shares",
              data={"principal_type": "team", "principal_id": team["id"],
                    "role": "editor"}).raise_for_status()
    bob.get_object_by_id("notes", note["id"], silent=True)

    alice.expect_status("DELETE", f"teams/{team['id']}", 204)

    bob.expect_status("GET", f"notes/{note['id']}", 404,
                      why="team grant died with the team")
    # The owner keeps the note: notes belong to owners, teams are only grantees.
    alice.get_object_by_id("notes", note["id"], silent=True)

    shares = alice.get(f"notes/{note['id']}/shares", silent=True)
    assert shares["shares"] == [], f"orphaned grants survived: {shares}"
    return "deleting a team removes its grants and leaves the owner's note intact"


def account_rules():
    """Rules 24-27: registration, login parity, lookup scoping, strict decoding."""
    alice = new_api_client_with_new_random_user("alice")

    # Case-insensitive uniqueness: the same address in different case is a
    # conflict, not a second account.
    dup = APIClient()
    dup.raw("POST", "auth/register")  # warm the session
    res = dup.raw("POST", "auth/register", data={
        "email": alice.email.upper(),
        "password": FIXTURE_PASSWORD,
        "display_name": "impostor",
    })
    assert res.status_code == 409, f"case-variant duplicate accepted: {res.status_code}"

    # Wrong password and unknown account return the same message. Saying which
    # would confirm whether an address is registered.
    anon = APIClient()
    wrong_pw = anon.raw("POST", "auth/login",
                        data={"email": alice.email, "password": "definitely-wrong-pw"})
    unknown = anon.raw("POST", "auth/login",
                       data={"email": "nobody-here@example.test", "password": "definitely-wrong-pw"})
    assert wrong_pw.status_code == unknown.status_code == 401
    assert wrong_pw.json()["detail"] == unknown.json()["detail"], (
        "login distinguishes a wrong password from an unknown account"
    )

    # Lookup is exact-email and scoped to people you share a team with.
    stranger = new_api_client_with_new_random_user("stranger")
    alice.expect_status("GET", f"users?email={stranger.email}", 404,
                        why="not a co-member, so not discoverable")
    alice.get(f"users?email={alice.email}", silent=True)  # yourself is fine

    # Unknown fields are rejected rather than silently dropped, so a client
    # typo surfaces instead of quietly doing nothing.
    alice.expect_status("PATCH", "users/me", 400, data={"is_admin": True},
                        why="unknown fields must not be silently ignored")
    return "registration, login parity, lookup scoping and strict decoding hold"
