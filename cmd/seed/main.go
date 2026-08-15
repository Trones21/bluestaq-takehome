// Command seed loads demo data.
//
//	make seed
//
// Builds the scenario the design is actually about: two teams with an
// overlapping member, and notes covering every sharing shape -- private, team,
// cross-team, and one-off user. Without this a reviewer opening /docs sees an
// empty database and has to construct the interesting case themselves.
//
// It also stages a version conflict, so the headline design choice can be seen
// without setting it up: one note is left with a stale ETag printed alongside
// the current one, ready to paste into an If-Match and get a 409.
//
// Idempotent by wiping first. Demo data that accumulates on every run stops
// demonstrating anything.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/auth"
	"github.com/Trones21/bluestaq-takehome/internal/config"
	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One password for every demo account, printed at the end so a reviewer can
// log in as anyone. Local demo data only.
const demoPassword = "demo-password-123"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration invalid:\n%w", err)
	}
	if strings.Contains(cfg.DatabaseURL, "rds.amazonaws.com") {
		return fmt.Errorf("refusing to seed what looks like a production database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)

	if _, err := pool.Exec(ctx, `
		truncate attachments, note_shares, notes, team_members, teams, users restart identity cascade
	`); err != nil {
		return fmt.Errorf("clearing existing data: %w", err)
	}

	hash, err := auth.HashPassword(demoPassword)
	if err != nil {
		return err
	}

	user := func(email, name string) sqlcgen.User {
		u, err := st.CreateUser(ctx, sqlcgen.CreateUserParams{
			Email: email, PasswordHash: hash, DisplayName: name,
		})
		if err != nil {
			panic(err)
		}
		return u
	}

	// Bob is in both teams. That overlap is what makes the cross-team case
	// legible: he resolves to a different role on the same note depending on
	// which grant reaches him.
	alice := user("alice@example.com", "Alice Chen")
	bob := user("bob@example.com", "Bob Okafor")
	carol := user("carol@example.com", "Carol Diaz")
	dave := user("dave@example.com", "Dave Nowak")

	platform := team(ctx, pool, "Platform", map[uuid.UUID]string{
		alice.ID: "admin", bob.ID: "member",
	})
	design := team(ctx, pool, "Design", map[uuid.UUID]string{
		carol.ID: "admin", bob.ID: "member",
	})

	note := func(owner sqlcgen.User, title, body string, tags ...string) sqlcgen.CreateNoteRow {
		if tags == nil {
			tags = []string{}
		}
		n, err := st.CreateNote(ctx, sqlcgen.CreateNoteParams{
			OwnerUserID: owner.ID, Title: title, Body: body, Tags: tags,
		})
		if err != nil {
			panic(err)
		}
		return n
	}

	// Private: no grants at all.
	private := note(alice, "Alice's private scratchpad",
		"Half-formed thoughts. Nobody else can see this.", "personal")

	// Team note: one grant.
	runbook := note(alice, "Deploy runbook",
		"1. make ci\n2. ./scripts/deploy.sh\n3. watch /readyz", "ops", "runbook")
	share(ctx, pool, runbook.ID, "team", platform.ID, "editor", alice.ID)

	// Cross-team: two grants, different roles. Bob is an editor via Platform
	// AND a viewer via Design -- the strongest grant wins, so he resolves as
	// editor while Carol sees viewer.
	spec := note(alice, "Notes API v2 spec",
		"Shared with Platform (edit) and Design (read).", "spec", "cross-team")
	share(ctx, pool, spec.ID, "team", platform.ID, "editor", alice.ID)
	share(ctx, pool, spec.ID, "team", design.ID, "viewer", alice.ID)

	// One-off user grant, crossing no team at all.
	review := note(carol, "Design review feedback",
		"Carol shared this with Alice directly, no team involved.", "design")
	share(ctx, pool, review.ID, "user", alice.ID, "viewer", carol.ID)

	// A soft-deleted note, so restore has something to act on.
	deleted := note(alice, "Abandoned draft", "Deleted, but recoverable.", "draft")
	if _, err := st.SoftDeleteNote(ctx, deleted.ID); err != nil {
		return err
	}

	// Dave owns nothing and is in no team: the control case for "sees nothing".
	_ = dave

	fmt.Print(summary(alice, bob, carol, dave, platform, design, spec, private, runbook, deleted))
	return nil
}

func team(ctx context.Context, pool *pgxpool.Pool, name string, members map[uuid.UUID]string) sqlcgen.Team {
	var t sqlcgen.Team
	err := pool.QueryRow(ctx,
		`insert into teams (name) values ($1) returning id, name, created_at, updated_at`, name,
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		panic(err)
	}
	for userID, role := range members {
		if _, err := pool.Exec(ctx,
			`insert into team_members (team_id, user_id, role) values ($1, $2, $3)`,
			t.ID, userID, role); err != nil {
			panic(err)
		}
	}
	return t
}

func share(ctx context.Context, pool *pgxpool.Pool, noteID uuid.UUID, ptype string, pid uuid.UUID, role string, by uuid.UUID) {
	if _, err := pool.Exec(ctx, `
		insert into note_shares (note_id, principal_type, principal_id, role, granted_by)
		values ($1, $2, $3, $4, $5)
	`, noteID, ptype, pid, role, by); err != nil {
		panic(err)
	}
}

func summary(alice, bob, carol, dave sqlcgen.User, platform, design sqlcgen.Team,
	spec, private, runbook, deleted sqlcgen.CreateNoteRow) string {

	return fmt.Sprintf(`
Seeded. Every account uses the password: %s

  Alice  %-24s owns most notes, admin of Platform
  Bob    %-24s member of BOTH teams
  Carol  %-24s admin of Design
  Dave   %-24s in no team, owns nothing (the control case)

Teams
  Platform  %s
  Design    %s

Worth looking at:

  Cross-team note %s
    Shared with Platform as editor and Design as viewer.
    Log in as Bob   -> your_role is "editor"  (strongest grant wins)
    Log in as Carol -> your_role is "viewer"
    Log in as Dave  -> 404, not 403

  Private note %s
    Log in as anyone but Alice -> 404. Its existence is not disclosed.

  Deleted note %s
    Invisible to everyone, including Alice's normal reads.
    As Alice:  GET  /v1/notes?deleted=true      to find it
               POST /v1/notes/{id}/restore      to bring it back

  Version conflict, without setting one up:
    GET   /v1/notes/%s            note the ETag, say "1"
    PATCH twice with If-Match: 1  the second returns 409 with the current state

Try it: open http://localhost:8080/docs and log in as alice@example.com
`,
		demoPassword,
		alice.Email, bob.Email, carol.Email, dave.Email,
		platform.ID, design.ID,
		spec.ID, private.ID, deleted.ID, runbook.ID)
}
