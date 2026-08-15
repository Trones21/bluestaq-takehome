// Package obs holds metrics and the observability middleware.
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics is the RED set (Rate, Errors, Duration) plus the few domain counters
// that answer questions the request log cannot.
type Metrics struct {
	Requests *prometheus.CounterVec
	Duration *prometheus.HistogramVec
	InFlight prometheus.Gauge

	// Presigns is the only visibility we have into attachment traffic: the
	// bytes never touch this process, so downloads are invisible to us.
	// Issuance is a proxy for intent. See SPEC 10.
	Presigns *prometheus.CounterVec
	// Conflicts counts 409s from optimistic concurrency. A rising rate means
	// users are colliding, which is the signal that real-time collaboration
	// has become worth building.
	Conflicts prometheus.Counter
	// Timeouts counts requests killed by the deadline. Distinct from the 503s
	// in Requests, which also cover readiness failures: this one means the
	// service was up and still could not finish in time.
	Timeouts *prometheus.CounterVec

	registry *prometheus.Registry
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests by route pattern, method and status.",
		}, []string{"method", "route", "status"}),

		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "HTTP request latency by route pattern.",
			// Tuned for a JSON API backed by one database: sub-millisecond
			// resolution is noise, and anything past ~5s has already timed
			// out from the caller's perspective.
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"method", "route"}),

		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Requests currently being served.",
		}),

		Presigns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "attachment_presign_total",
			Help: "Presigned URLs issued, by operation (put/get).",
		}, []string{"op"}),

		Conflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "note_update_conflicts_total",
			Help: "Note updates rejected with 409 due to a stale If-Match version.",
		}),

		Timeouts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_request_timeouts_total",
			Help: "Requests whose context deadline expired before the handler finished.",
		}, []string{"method", "route"}),
	}

	reg.MustRegister(
		m.Requests, m.Duration, m.InFlight, m.Presigns, m.Conflicts, m.Timeouts,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
