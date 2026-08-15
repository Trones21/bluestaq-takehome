// Package testdb provides a real Postgres for integration tests.
//
// No mocked database, deliberately. Every interesting bug in this service
// lives in a SQL predicate -- the access resolution, the version check, the
// keyset scan -- and a mock would assert that the Go code calls the query, not
// that the query is right.
package testdb

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/store"
	"github.com/Trones21/bluestaq-takehome/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const defaultURL = "postgres://notes:notes@localhost:5433/notes_test?sslmode=disable"

var (
	migrateOnce sync.Once
	migrateErr  error
)

// URL returns the test database connection string.
func URL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultURL
}

// New returns a store connected to a migrated, empty test database.
//
// Skips rather than fails when Postgres is unreachable, so `go test ./...` on
// a machine without Docker still runs the unit suite. CI sets
// REQUIRE_TEST_DATABASE=1 so a missing database there is a failure instead of
// a silent pass -- an integration suite that quietly skips is worse than none.
func New(t *testing.T) *store.Store {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, URL())
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		if pool != nil {
			pool.Close()
		}
		if os.Getenv("REQUIRE_TEST_DATABASE") != "" {
			t.Fatalf("test database required but unreachable at %s: %v", URL(), err)
		}
		t.Skipf("test database unreachable (%v); run `make test-deps` to start it", err)
	}

	migrateOnce.Do(func() { migrateErr = migrate() })
	if migrateErr != nil {
		pool.Close()
		t.Fatalf("migrating test database: %v", migrateErr)
	}

	s := store.New(pool)
	truncate(t, s)
	t.Cleanup(pool.Close)
	return s
}

func migrate() error {
	db, err := sql.Open("pgx", URL())
	if err != nil {
		return err
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

// truncate empties every domain table between tests.
//
// Truncate rather than a per-test transaction that rolls back: several
// behaviours under test are themselves transactional (share cleanup on user
// deletion, the version check), and wrapping them in an outer transaction
// would change what is being tested.
func truncate(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	_, err := s.Pool.Exec(ctx, `
		truncate attachments, note_shares, notes, team_members, teams, users
		restart identity cascade
	`)
	if err != nil {
		t.Fatalf("truncating: %v", err)
	}
}
