package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type Metrics struct {
	registry *prometheus.Registry

	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	transitions *prometheus.CounterVec
	rejections  prometheus.Counter
	reloads     *prometheus.CounterVec
	connLimit   prometheus.Counter
}

func New(src StateSource) *Metrics {
	reg := prometheus.NewRegistry()

	// Go runtime metrics
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(
		collectors.NewProcessCollector(
			collectors.ProcessCollectorOpts{},
		),
	)

	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "loadgate_requests_total",
				Help: "Requests by backend and status class",
			},
			[]string{"backend", "status_class"},
		),
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "loadgate_request_duration_seconds",
				Help: "Request duration by backend",
			},
			[]string{"backend"},
		),
		transitions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "loadgate_health_transitions_total",
				Help: "Health phase transitions",
			},
			[]string{"backend", "to"},
		),
		rejections: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "loadgate_ratelimit_rejections_total",
				Help: "Requests rejected by rate limiting",
			},
		),
		reloads: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "loadgate_reload_total",
				Help: "Config reload outcomes",
			},
			[]string{"results"},
		),
		connLimit: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "loadgate_connection_limit_rejections_total",
				Help: "L4 connections refused because max_connections were open",
			},
		),
	}

	reg.MustRegister(m.requests, m.duration, m.transitions, m.rejections, m.reloads, m.connLimit)

	reg.MustRegister(NewStateCollector(src))

	return m
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

func (m *Metrics) RequestObserved(backend string, statusCode int, seconds float64) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(backend, statusClass(statusCode)).Inc()
	m.duration.WithLabelValues(backend).Observe(seconds)
}

func (m *Metrics) HealthTransition(backend, toPhase string) {
	if m == nil {
		return
	}
	m.transitions.WithLabelValues(backend, toPhase).Inc()
}

func (m *Metrics) RateLimitRejected() {
	if m == nil {
		return
	}
	m.rejections.Inc()
}

func (m *Metrics) ReloadResult(ok bool) {
	if m == nil {
		return
	}
	result := "success"
	if !ok {
		result = "error"
	}
	m.reloads.WithLabelValues(result).Inc()
}

func (m *Metrics) ConnLimitRejected() {
	if m == nil {
		return
	}
	m.connLimit.Inc()
}

func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}
