// Package store wraps the generated sqlc queries with the connection pool and
// the transaction helper.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/Trones21/bluestaq-takehome/internal/store/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns the pool and exposes the generated queries.
type Store struct {
	Pool *pgxpool.Pool
	*sqlcgen.Queries
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool, Queries: sqlcgen.New(pool)}
}

// Tx runs fn inside a transaction, rolling back on error or panic.
//
// Several operations here are only correct as a unit -- deleting a user or
// team must remove their share grants in the same transaction, because
// note_shares.principal_id has no foreign key and therefore no cascade. A
// half-applied delete leaves grants that would resolve against a reused UUID.
func (s *Store) Tx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe as an
		// unconditional cleanup and survives a panic mid-transaction.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IsNotFound reports whether err is pgx's "query returned no rows". Callers
// translate this into a 404 (or, for authorization, into "no access"), so it
// is worth a named helper rather than errors.Is at every call site.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// IsUniqueViolation reports whether err is a unique constraint failure,
// optionally for a specific constraint name.
//
// Checking the constraint rather than pre-querying avoids a check-then-insert
// race: two concurrent registrations for the same address would both pass a
// prior existence check and one would still fail here.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// IsForeignKeyViolation reports whether err is a foreign key failure, which
// generally means a referenced row was deleted concurrently.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
