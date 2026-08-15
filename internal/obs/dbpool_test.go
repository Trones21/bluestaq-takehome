package obs_test

import (
	"context"
	"testing"

	"github.com/Trones21/bluestaq-takehome/internal/obs"
	"github.com/Trones21/bluestaq-takehome/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Against a real pool rather than a fake: pgxpool.Stat has no exported
// constructor, so a fake would only prove the collector can read a struct we
// wrote ourselves. The value here is confirming the metric names and the
// counter/gauge split survive a real Stat().
func TestPoolStatsExposeSaturationSignals(t *testing.T) {
	pool := testdb.New(t).Pool

	m := obs.NewMetrics()
	if err := m.RegisterPool(pool); err != nil {
		t.Fatalf("registering pool collector: %v", err)
	}

	// Force an acquire so the counters are not all zero.
	var one int
	if err := pool.QueryRow(context.Background(), "select 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}

	for _, name := range []string{
		"pgxpool_connections",
		"pgxpool_connections_max",
		"pgxpool_acquires_total",
		"pgxpool_acquires_empty_total",
		"pgxpool_acquires_canceled_total",
		"pgxpool_acquire_wait_seconds_total",
	} {
		if n := testutil.CollectAndCount(obs.NewPoolStats(pool), name); n == 0 {
			t.Errorf("%s is not exposed", name)
		}
	}

	// The query above had to take a connection, so acquires is the one counter
	// with a value worth asserting rather than mere presence.
	if got := gather(t, pool, "pgxpool_acquires_total"); got < 1 {
		t.Fatalf("want at least one acquire recorded, got %v", got)
	}
	// Deliberately not asserted as zero: pgxpool counts an empty acquire when
	// no *idle* connection exists, so opening the pool's first connection
	// records one. That is why the doc comment steers alerting toward the wait
	// duration instead -- this counter is non-zero on a healthy idle pool.
	if got := gather(t, pool, "pgxpool_connections_max"); got != float64(pool.Config().MaxConns) {
		t.Fatalf("connections_max should mirror the configured ceiling, got %v", got)
	}
}

// gather reads the value of a single unlabelled series from the collector.
func gather(t *testing.T, pool *pgxpool.Pool, name string) float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	reg.MustRegister(obs.NewPoolStats(pool))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		metrics := fam.GetMetric()
		if len(metrics) != 1 {
			t.Fatalf("%s: want one series, got %d", name, len(metrics))
		}
		if c := metrics[0].GetCounter(); c != nil {
			return c.GetValue()
		}
		return metrics[0].GetGauge().GetValue()
	}
	t.Fatalf("%s was not collected", name)
	return 0
}
