package teams_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/apitest"
	"github.com/google/uuid"
)

type team struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	YourRole string    `json:"your_role"`
}

func createTeam(t *testing.T, c *apitest.Client, name string) team {
	t.Helper()
	var tm team
	c.Post("/v1/teams", map[string]string{"name": name}).
		Require(http.StatusCreated).JSON(&tm)
	return tm
}

func TestCreatorBecomesAdmin(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")

	tm := createTeam(t, alice, "Platform")
	if tm.YourRole != "admin" {
		t.Fatalf("creator role = %q, want admin", tm.YourRole)
	}

	var listed struct {
		Teams []team `json:"teams"`
	}
	alice.Get("/v1/teams").Require(http.StatusOK).JSON(&listed)
	if len(listed.Teams) != 1 || listed.Teams[0].ID != tm.ID {
		t.Fatalf("creator does not see their own team: %+v", listed.Teams)
	}
}

// A non-member gets 404, not 403. Team names leak organisational structure,
// so existence is concealed the same way note existence is.
func TestNonMemberSees404NotForbidden(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Secret Project")

	res := bob.Get("/v1/teams/" + tm.ID.String())
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.StatusCode)
	}
	if strings.Contains(string(res.Body), "Secret Project") {
		t.Error("404 leaked the team name")
	}
}

// A member knows the team exists, so an insufficient role is 403 here.
func TestMemberCannotAdministerButGets403NotFound(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Platform")
	addMember(t, alice, tm.ID, bob, "member")

	bob.Get("/v1/teams/" + tm.ID.String()).Require(http.StatusOK)

	for _, res := range []*apitest.Response{
		bob.Patch("/v1/teams/"+tm.ID.String(), map[string]string{"name": "Renamed"}),
		bob.Delete("/v1/teams/" + tm.ID.String()),
		bob.Post("/v1/teams/"+tm.ID.String()+"/members", map[string]any{"user_id": uuid.New()}),
	} {
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("member admin action got %d, want 403", res.StatusCode)
		}
	}
}

// A team with no admins can never be renamed, added to, or deleted again, so
// the last admin cannot leave or be removed.
func TestLastAdminCannotBeRemovedOrLeave(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Platform")
	addMember(t, alice, tm.ID, bob, "member")

	if code := alice.Post("/v1/teams/"+tm.ID.String()+"/leave", nil).StatusCode; code != http.StatusConflict {
		t.Errorf("last admin left the team: %d", code)
	}
	if code := alice.Delete("/v1/teams/" + tm.ID.String() + "/members/" + alice.ID.String()).StatusCode; code != http.StatusConflict {
		t.Errorf("last admin removed themselves: %d", code)
	}
	// Demotion is the same hazard by another route.
	res := alice.Post("/v1/teams/"+tm.ID.String()+"/members",
		map[string]any{"user_id": alice.ID, "role": "member"})
	if res.StatusCode != http.StatusConflict {
		t.Errorf("last admin demoted themselves: %d", res.StatusCode)
	}
}

func TestAdminCanLeaveOnceAnotherAdminExists(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Platform")
	addMember(t, alice, tm.ID, bob, "admin")

	alice.Post("/v1/teams/"+tm.ID.String()+"/leave", nil).Require(http.StatusNoContent)

	// Bob is now the sole admin, and inherits the same protection.
	if code := bob.Post("/v1/teams/"+tm.ID.String()+"/leave", nil).StatusCode; code != http.StatusConflict {
		t.Fatalf("the new last admin was allowed to leave: %d", code)
	}
}

// The concrete cost of the polymorphic ACL: principal_id has no foreign key,
// so deleting a team cascades nothing. The grant must be removed explicitly,
// or it would resolve against whoever next receives that UUID.
func TestDeletingATeamRemovesItsShareGrants(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Platform")
	addMember(t, alice, tm.ID, bob, "member")

	var note struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "team note"}).
		Require(http.StatusCreated).JSON(&note)

	shareWithTeam(t, env, note.ID, tm.ID, alice.ID, "editor")
	bob.Get("/v1/notes/" + note.ID.String()).Require(http.StatusOK)

	alice.Delete("/v1/teams/" + tm.ID.String()).Require(http.StatusNoContent)

	var orphans int
	if err := env.Store.Pool.QueryRow(context.Background(),
		`select count(*) from note_shares where principal_type = 'team' and principal_id = $1`,
		tm.ID,
	).Scan(&orphans); err != nil {
		t.Fatalf("counting grants: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d orphaned grant(s) survived team deletion -- a reused UUID would inherit access", orphans)
	}

	// Bob's access came from the team, so it is gone.
	if code := bob.Get("/v1/notes/" + note.ID.String()).StatusCode; code != http.StatusNotFound {
		t.Errorf("member still reads the note after team deletion: %d", code)
	}
	// Alice owns it, so hers is untouched -- notes belong to owners, and a
	// team is only ever a grantee.
	alice.Get("/v1/notes/" + note.ID.String()).Require(http.StatusOK)
}

// Removing someone from a team must revoke their note access at once, with
// nothing to clean up: the grant is to the team, so they simply stop matching.
func TestRemovingAMemberRevokesTheirNoteAccessImmediately(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Platform")
	addMember(t, alice, tm.ID, bob, "member")

	var note struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "team note"}).
		Require(http.StatusCreated).JSON(&note)
	shareWithTeam(t, env, note.ID, tm.ID, alice.ID, "editor")

	bob.Get("/v1/notes/" + note.ID.String()).Require(http.StatusOK)

	alice.Delete("/v1/teams/" + tm.ID.String() + "/members/" + bob.ID.String()).
		Require(http.StatusNoContent)

	if code := bob.Get("/v1/notes/" + note.ID.String()).StatusCode; code != http.StatusNotFound {
		t.Fatalf("removed member still reads the note: %d", code)
	}
	// The grant survives for whoever remains -- it was never Bob's personally.
	var grants int
	if err := env.Store.Pool.QueryRow(context.Background(),
		`select count(*) from note_shares where note_id = $1`, note.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Errorf("team grant count = %d, want 1 -- removing a member must not drop the team's grant", grants)
	}
}

// Team admin governs membership, not note content. Conflating the two is a
// common source of surprise, so it gets an explicit test.
func TestTeamAdminIsNotAutomaticallyANoteEditor(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	bob := env.Register("bob")

	tm := createTeam(t, alice, "Platform")
	addMember(t, alice, tm.ID, bob, "admin")

	var note struct {
		ID uuid.UUID `json:"id"`
	}
	alice.Post("/v1/notes", map[string]any{"title": "view only"}).
		Require(http.StatusCreated).JSON(&note)
	shareWithTeam(t, env, note.ID, tm.ID, alice.ID, "viewer")

	var got struct {
		YourRole string `json:"your_role"`
	}
	bob.Get("/v1/notes/" + note.ID.String()).Require(http.StatusOK).JSON(&got)
	if got.YourRole != "viewer" {
		t.Fatalf("team admin resolved as %q on a viewer grant, want viewer", got.YourRole)
	}
	if code := bob.Patch("/v1/notes/"+note.ID.String(),
		map[string]any{"body": "x"}, "If-Match", "1").StatusCode; code != http.StatusForbidden {
		t.Fatalf("team admin edited a note shared read-only: %d", code)
	}
}

func TestAddingUnknownUserIsNotFound(t *testing.T) {
	env := apitest.New(t)
	alice := env.Register("alice")
	tm := createTeam(t, alice, "Platform")

	res := alice.Post("/v1/teams/"+tm.ID.String()+"/members", map[string]any{"user_id": uuid.New()})
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.StatusCode)
	}
}

func addMember(t *testing.T, admin *apitest.Client, teamID uuid.UUID, target *apitest.Client, role string) {
	t.Helper()
	admin.Post("/v1/teams/"+teamID.String()+"/members",
		map[string]any{"user_id": target.ID, "role": role}).
		Require(http.StatusNoContent)
}

// shareWithTeam writes the grant directly; the sharing endpoints land next.
func shareWithTeam(t *testing.T, env *apitest.Env, noteID, teamID, grantedBy uuid.UUID, role string) {
	t.Helper()
	_, err := env.Store.Pool.Exec(context.Background(), `
		insert into note_shares (note_id, principal_type, principal_id, role, granted_by)
		values ($1, 'team', $2, $3, $4)
	`, noteID, teamID, role, grantedBy)
	if err != nil {
		t.Fatalf("sharing with team: %v", err)
	}
}
