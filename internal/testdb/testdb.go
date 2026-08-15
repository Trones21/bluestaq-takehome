// Package testdb provides a real Postgres for integration tests.
//
// No mocked database, deliberately. Every interesting bug in this service
// lives in a SQL predicate -- the access resolution, the version check, the
// keyset scan -- and a mock would assert that the Go code calls the query, not
// that the query is right.
package testdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// BaseURL is the configured test database, which supplies the credentials and
// host. The database name in it is replaced per package -- see URL.
func BaseURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultURL
}

// URL returns this test package's own database.
//
// One database per package, because `go test ./...` runs separate packages
// concurrently while tests *within* a package run sequentially. Sharing one
// database across packages means one package's truncate wipes another's
// fixtures mid-test -- which passes when each package is run alone and fails
// only on the full suite, the worst way for it to fail.
//
// Per-package rather than per-test because sequential execution within a
// package already provides the isolation, and creating a database per test
// would dominate the runtime.
func URL() string {
	base := BaseURL()
	name := packageDBName()
	if name == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = "/" + name
	return u.String()
}

// packageDBName derives a database name from the test binary, which go test
// names after the package under test (e.g. ".../notes.test").
func packageDBName() string {
	bin := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
	if bin == "" {
		return ""
	}
	// Guard against two packages sharing a base name in different directories.
	sum := sha256.Sum256([]byte(os.Args[0]))
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, strings.ToLower(bin))

	return fmt.Sprintf("notes_test_%s_%s", safe, hex.EncodeToString(sum[:3]))
}

// ensureDatabase creates this package's database if it does not exist,
// connecting to the configured one to issue the CREATE.
func ensureDatabase(ctx context.Context) error {
	target := URL()
	if target == BaseURL() {
		return nil
	}

	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(u.Path, "/")

	admin, err := pgxpool.New(ctx, BaseURL())
	if err != nil {
		return err
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRow(ctx,
		`select exists(select 1 from pg_database where datname = $1)`, name,
	).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	// CREATE DATABASE cannot be parameterised, and the name is derived from
	// the test binary path rather than any external input.
	_, err = admin.Exec(ctx, fmt.Sprintf("create database %q", name))
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
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

	var pool *pgxpool.Pool
	err := ensureDatabase(ctx)
	if err == nil {
		pool, err = pgxpool.New(ctx, URL())
	}
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
