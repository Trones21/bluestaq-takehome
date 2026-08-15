// Command migrate applies database migrations.
//
// This is a separate binary, and that is the point: migrations run as a
// discrete deploy step, never on application startup.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate status
//	go run ./cmd/migrate down
//
// Two reasons it is not folded into the API's boot sequence, both of which
// only bite in production:
//
//  1. With more than one instance, N processes race to migrate the same
//     database at the same moment.
//  2. A self-migrating app cannot be rolled back. If the rollout fails after
//     the schema has already changed, the previous binary comes back up
//     against a schema it does not understand.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/Trones21/bluestaq-takehome/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: migrate <up|down|status|version> [args]")
	}
	command, args := os.Args[1], os.Args[2:]

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	// database/sql via the pgx stdlib shim: goose wants a *sql.DB, and this is
	// a short-lived one-shot process, so the pgxpool machinery buys nothing.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("database unreachable: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	if err := goose.RunContext(ctx, command, db, ".", args...); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}
