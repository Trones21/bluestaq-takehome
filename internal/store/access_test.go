package store_test

import (
	"context"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/authz"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/Trones21/bluestaq-takehome/internal/testdb"
	"github.com/google/uuid"
)

// roleOf runs the real access query and returns the resolved role.
//
// The unit matrix in internal/authz proves the *rules*; this proves the SQL
// implements them. Both are needed -- a correct table resolved by a wrong
// query is still a data leak.
func roleOf(t *testing.T, s *store.Store, noteID, userID uuid.UUID) authz.Role {
	t.Helper()
	row, err := s.GetNoteWithAccess(context.Background(), sqlcgen.GetNoteWithAccessParams{
		UserID: userID,
		NoteID: noteID,
	})
	if err != nil {
		if store.IsNotFound(err) {
			return authz.RoleNone
		}
		t.Fatalf("resolving access: %v", err)
	}
	return authz.Role(row.EffectiveRole)
}

func TestOwnerHasOwnerRole(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	note := testdb.Note(t, s, alice, "Alice's note")

	if got := roleOf(t, s, note.ID, alice.ID); got != authz.RoleOwner {
		t.Fatalf("owner resolved as %s", got)
	}
}

// A private note is one with no share rows. This is the case the whole design
// has to get right: nobody else sees it, at all.
func TestStrangerHasNoAccessToPrivateNote(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	note := testdb.Note(t, s, alice, "private")

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleNone {
		t.Fatalf("stranger resolved as %s on a private note", got)
	}
}

func TestDirectUserGrant(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	note := testdb.Note(t, s, alice, "shared directly")

	for _, tc := range []struct {
		grant string
		want  authz.Role
	}{
		{"viewer", authz.RoleViewer},
		{"editor", authz.RoleEditor},
	} {
		testdb.ShareWithUser(t, s, note.ID, bob, tc.grant, alice)
		if got := roleOf(t, s, note.ID, bob.ID); got != tc.want {
			t.Errorf("grant %q resolved as %s, want %s", tc.grant, got, tc.want)
		}
	}
}

func TestTeamGrantReachesMembers(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	carol := testdb.User(t, s, "carol") // deliberately not a member

	platform := testdb.Team(t, s, "platform", alice, bob)
	note := testdb.Note(t, s, alice, "team note")
	testdb.ShareWithTeam(t, s, note.ID, platform, "editor", alice)

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleEditor {
		t.Errorf("team member resolved as %s, want editor", got)
	}
	if got := roleOf(t, s, note.ID, carol.ID); got != authz.RoleNone {
		t.Errorf("non-member resolved as %s -- a team grant leaked outside the team", got)
	}
}

// The inference the whole schema rests on: "shared amongst several small
// teams" means a note can cross a team boundary. Two team grants, two
// disjoint teams, one note.
func TestNoteSharedAcrossTwoTeams(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	carol := testdb.User(t, s, "carol")
	dave := testdb.User(t, s, "dave") // in neither team

	platform := testdb.Team(t, s, "platform", alice, bob)
	design := testdb.Team(t, s, "design", carol)

	note := testdb.Note(t, s, alice, "cross-team spec")
	testdb.ShareWithTeam(t, s, note.ID, platform, "editor", alice)
	testdb.ShareWithTeam(t, s, note.ID, design, "viewer", alice)

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleEditor {
		t.Errorf("platform member resolved as %s, want editor", got)
	}
	if got := roleOf(t, s, note.ID, carol.ID); got != authz.RoleViewer {
		t.Errorf("design member resolved as %s, want viewer", got)
	}
	if got := roleOf(t, s, note.ID, dave.ID); got != authz.RoleNone {
		t.Errorf("outsider resolved as %s", got)
	}
}

// When several grants match, the strongest wins. This is the case that breaks
// if role names are ever compared as text, since "viewer" > "editor"
// alphabetically.
func TestHighestGrantWins(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	platform := testdb.Team(t, s, "platform", alice, bob)

	note := testdb.Note(t, s, alice, "overlapping grants")
	// Bob is a viewer personally, but an editor through his team.
	testdb.ShareWithUser(t, s, note.ID, bob, "viewer", alice)
	testdb.ShareWithTeam(t, s, note.ID, platform, "editor", alice)

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleEditor {
		t.Fatalf("overlapping grants resolved as %s, want editor -- max() is picking the weaker grant", got)
	}
}

// The reverse ordering, to catch a max() that happens to work only one way.
func TestHighestGrantWinsRegardlessOfWhichSideIsStronger(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	platform := testdb.Team(t, s, "platform", alice, bob)

	note := testdb.Note(t, s, alice, "overlapping grants")
	testdb.ShareWithUser(t, s, note.ID, bob, "editor", alice)
	testdb.ShareWithTeam(t, s, note.ID, platform, "viewer", alice)

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleEditor {
		t.Fatalf("resolved as %s, want editor", got)
	}
}

// Owning a note outranks any grant on it, including a weaker one someone
// managed to create against the owner.
func TestOwnershipOutranksAWeakerGrant(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	note := testdb.Note(t, s, alice, "self-shared")
	testdb.ShareWithUser(t, s, note.ID, alice, "viewer", alice)

	if got := roleOf(t, s, note.ID, alice.ID); got != authz.RoleOwner {
		t.Fatalf("owner with a viewer grant resolved as %s", got)
	}
}

// Membership is what carries a team grant, so losing it must revoke access
// immediately rather than at some later sync.
func TestLeavingATeamRevokesAccess(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	platform := testdb.Team(t, s, "platform", alice, bob)

	note := testdb.Note(t, s, alice, "team note")
	testdb.ShareWithTeam(t, s, note.ID, platform, "editor", alice)

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleEditor {
		t.Fatalf("precondition failed: bob is %s", got)
	}

	if _, err := s.Pool.Exec(context.Background(),
		`delete from team_members where team_id = $1 and user_id = $2`, platform.ID, bob.ID); err != nil {
		t.Fatalf("removing member: %v", err)
	}

	if got := roleOf(t, s, note.ID, bob.ID); got != authz.RoleNone {
		t.Fatalf("removed member still resolves as %s", got)
	}
}

// A note that does not exist and a note the caller cannot see must be
// indistinguishable, which is what lets the handler return 404 for both.
func TestMissingNoteAndInvisibleNoteLookTheSame(t *testing.T) {
	s := testdb.New(t)
	alice := testdb.User(t, s, "alice")
	bob := testdb.User(t, s, "bob")
	hidden := testdb.Note(t, s, alice, "private")

	if got := roleOf(t, s, hidden.ID, bob.ID); got != authz.RoleNone {
		t.Fatalf("invisible note resolved as %s", got)
	}
	if got := roleOf(t, s, uuid.New(), bob.ID); got != authz.RoleNone {
		t.Fatalf("nonexistent note resolved as %s", got)
	}
}
