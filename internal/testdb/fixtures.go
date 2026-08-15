package testdb

import (
	"context"
	"fmt"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/google/uuid"
)

// Fixture builders. Named arguments over struct literals at every call site,
// so a test reads as the scenario it describes rather than as setup.

func User(t *testing.T, s *store.Store, name string) sqlcgen.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), sqlcgen.CreateUserParams{
		Email:        fmt.Sprintf("%s-%s@example.test", name, uuid.NewString()[:8]),
		PasswordHash: "not-a-real-hash",
		DisplayName:  name,
	})
	if err != nil {
		t.Fatalf("creating user %s: %v", name, err)
	}
	return u
}

func Team(t *testing.T, s *store.Store, name string, members ...sqlcgen.User) sqlcgen.Team {
	t.Helper()
	ctx := context.Background()

	var team sqlcgen.Team
	err := s.Pool.QueryRow(ctx,
		`insert into teams (name) values ($1) returning id, name, created_at, updated_at`, name,
	).Scan(&team.ID, &team.Name, &team.CreatedAt, &team.UpdatedAt)
	if err != nil {
		t.Fatalf("creating team %s: %v", name, err)
	}

	for i, m := range members {
		// First member is the admin, matching how team creation works in the API.
		role := "member"
		if i == 0 {
			role = "admin"
		}
		if _, err := s.Pool.Exec(ctx,
			`insert into team_members (team_id, user_id, role) values ($1, $2, $3)`,
			team.ID, m.ID, role,
		); err != nil {
			t.Fatalf("adding %s to %s: %v", m.DisplayName, name, err)
		}
	}
	return team
}

func Note(t *testing.T, s *store.Store, owner sqlcgen.User, title string) sqlcgen.CreateNoteRow {
	t.Helper()
	n, err := s.CreateNote(context.Background(), sqlcgen.CreateNoteParams{
		OwnerUserID: owner.ID,
		Title:       title,
		Body:        "body of " + title,
		Tags:        []string{},
	})
	if err != nil {
		t.Fatalf("creating note %q: %v", title, err)
	}
	return n
}

// ShareWithUser grants a single user access to a note.
func ShareWithUser(t *testing.T, s *store.Store, noteID uuid.UUID, u sqlcgen.User, role string, grantedBy sqlcgen.User) {
	t.Helper()
	share(t, s, noteID, "user", u.ID, role, grantedBy.ID)
}

// ShareWithTeam grants every member of a team access to a note.
func ShareWithTeam(t *testing.T, s *store.Store, noteID uuid.UUID, team sqlcgen.Team, role string, grantedBy sqlcgen.User) {
	t.Helper()
	share(t, s, noteID, "team", team.ID, role, grantedBy.ID)
}

func share(t *testing.T, s *store.Store, noteID uuid.UUID, principalType string, principalID uuid.UUID, role string, grantedBy uuid.UUID) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), `
		insert into note_shares (note_id, principal_type, principal_id, role, granted_by)
		values ($1, $2, $3, $4, $5)
		on conflict (note_id, principal_type, principal_id) do update set role = excluded.role
	`, noteID, principalType, principalID, role, grantedBy)
	if err != nil {
		t.Fatalf("sharing note with %s %s: %v", principalType, principalID, err)
	}
}
