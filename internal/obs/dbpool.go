package obs

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolStats exports pgxpool's internal counters to Prometheus.
//
// This is the only view of the thing most likely to bound throughput. MaxConns
// caps how many queries run at once; when demand exceeds it, goroutines queue.
// Nothing else in the RED metrics distinguishes "the query was slow" from "the
// request waited its turn", and the fix for each is the opposite of the other:
// tune the query, or raise the ceiling.
//
// The signal to alert on is acquire_wait_seconds_total: divided by
// acquires_total it gives the average time a request spent queueing rather
// than working, and it is meaningful without further interpretation.
//
// acquire_empty_total needs care. It counts acquires that found no *idle*
// connection, which includes the pool growing toward its ceiling -- with
// MinConns at 0 a cold pool records one per connection it opens, so a non-zero
// value is normal and alerting on it directly produces false positives. It
// indicates saturation only alongside connections{state="acquired"} sitting at
// connections_max.
//
// Implemented as a Collector that reads Stat() at scrape time rather than as
// gauges updated on a ticker: pgxpool already maintains these counters, so
// polling them on a timer would only add staleness.
type PoolStats struct {
	stat func() *pgxpool.Stat

	conns       *prometheus.Desc
	maxConns    *prometheus.Desc
	acquires    *prometheus.Desc
	empty       *prometheus.Desc
	canceled    *prometheus.Desc
	waitSeconds *prometheus.Desc
}

// NewPoolStats builds a collector over pool. Registered by RegisterPool.
func NewPoolStats(pool *pgxpool.Pool) *PoolStats {
	return &PoolStats{
		stat: pool.Stat,
		conns: prometheus.NewDesc(
			"pgxpool_connections",
			"Connections held by the pool, by state.",
			[]string{"state"}, nil),
		maxConns: prometheus.NewDesc(
			"pgxpool_connections_max",
			"Configured ceiling on pool connections (DB_MAX_CONNS).",
			nil, nil),
		acquires: prometheus.NewDesc(
			"pgxpool_acquires_total",
			"Connections acquired from the pool.",
			nil, nil),
		empty: prometheus.NewDesc(
			"pgxpool_acquires_empty_total",
			"Acquires that found no idle connection, whether the pool was at its ceiling or still growing toward it.",
			nil, nil),
		canceled: prometheus.NewDesc(
			"pgxpool_acquires_canceled_total",
			"Acquires abandoned because the request context ended while waiting.",
			nil, nil),
		waitSeconds: prometheus.NewDesc(
			"pgxpool_acquire_wait_seconds_total",
			"Cumulative time spent waiting for a connection.",
			nil, nil),
	}
}

func (p *PoolStats) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.conns
	ch <- p.maxConns
	ch <- p.acquires
	ch <- p.empty
	ch <- p.canceled
	ch <- p.waitSeconds
}

func (p *PoolStats) Collect(ch chan<- prometheus.Metric) {
	s := p.stat()

	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}

	gauge(p.conns, float64(s.AcquiredConns()), "acquired")
	gauge(p.conns, float64(s.IdleConns()), "idle")
	gauge(p.conns, float64(s.ConstructingConns()), "constructing")
	gauge(p.maxConns, float64(s.MaxConns()))

	counter(p.acquires, float64(s.AcquireCount()))
	counter(p.empty, float64(s.EmptyAcquireCount()))
	counter(p.canceled, float64(s.CanceledAcquireCount()))
	counter(p.waitSeconds, s.AcquireDuration().Seconds())
}

// RegisterPool adds the pool collector to the metrics registry. Separate from
// NewMetrics because the pool is built after metrics: the registry has to exist
// before the database connection does.
func (m *Metrics) RegisterPool(pool *pgxpool.Pool) error {
	return m.registry.Register(NewPoolStats(pool))
}
