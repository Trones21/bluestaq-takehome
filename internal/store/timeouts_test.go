package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Trones21/bluestaq-takehome/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres accepts unknown runtime parameters at connect time by failing the
// connection, but a *misspelled* one is the quiet case: set the wrong key and
// nothing enforces anything, with no error anywhere. So this asserts the
// settings are live on a pooled connection rather than trusting the wiring,
// and asserts statement_timeout actually cancels rather than merely being set.
func TestPooledConnectionsCarryTheDatabaseTimeouts(t *testing.T) {
	base := testdb.New(t) // skips when no database is reachable

	poolCfg, err := pgxpool.ParseConfig(base.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parsing test database URL: %v", err)
	}
	const statement = 800 * time.Millisecond
	const idleInTx = 5 * time.Second
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] =
		strconv.FormatInt(statement.Milliseconds(), 10)
	poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] =
		strconv.FormatInt(idleInTx.Milliseconds(), 10)

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	defer pool.Close()

	for _, tc := range []struct{ setting, want string }{
		{"statement_timeout", "800ms"},
		{"idle_in_transaction_session_timeout", "5s"},
	} {
		var got string
		if err := pool.QueryRow(ctx, "show "+tc.setting).Scan(&got); err != nil {
			t.Fatalf("show %s: %v", tc.setting, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.setting, got, tc.want)
		}
	}

	// The setting is enforced by the server, so this must fail on its own --
	// the context here is deliberately generous enough that a client-side
	// deadline cannot be what stops it.
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err = pool.Exec(queryCtx, "select pg_sleep(10)")
	if err == nil {
		t.Fatal("statement_timeout did not cancel a query that ran past it")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("query ran %s; statement_timeout should have stopped it near %s", elapsed, statement)
	}
	t.Logf("server cancelled the query after %s: %v", time.Since(start).Round(time.Millisecond), err)
}
