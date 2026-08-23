package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// A Prometheus records metrics through github.com/prometheus/client_golang.
type Prometheus struct {
	requests         *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	confirms         *prometheus.CounterVec
	profilesInFlight prometheus.Gauge
	discoverySynced  prometheus.Gauge
}

// NewPrometheus builds the gateway's metrics and registers them with reg.
func NewPrometheus(reg prometheus.Registerer) *Prometheus {
	p := &Prometheus{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_requests_total",
			Help: "Total number of completed /v1 requests, by endpoint, profile, and response code.",
		}, []string{"endpoint", "profile", "code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "profgate_request_duration_seconds",
			Help:    "Duration in seconds of completed /v1 requests, by profile.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"profile"}),
		confirms: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_confirm_total",
			Help: "Total number of Pod confirmation attempts, by result.",
		}, []string{"result"}),
		profilesInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "profgate_profiles_in_flight",
			Help: "Number of profile fetches currently in progress.",
		}),
		discoverySynced: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "profgate_discovery_synced",
			Help: "Whether the discovery cache is currently synced: 1 if synced, 0 otherwise.",
		}),
	}

	reg.MustRegister(p.requests, p.requestDuration, p.confirms, p.profilesInFlight, p.discoverySynced)

	return p
}

// Request implements Recorder.
func (p *Prometheus) Request(endpoint Endpoint, profile, code string, d time.Duration) {
	p.requests.WithLabelValues(string(endpoint), profile, code).Inc()
	p.requestDuration.WithLabelValues(profile).Observe(d.Seconds())
}

// Confirm implements Recorder.
func (p *Prometheus) Confirm(result string) {
	p.confirms.WithLabelValues(result).Inc()
}

// ProfilesInFlight implements Recorder.
func (p *Prometheus) ProfilesInFlight(delta int) {
	p.profilesInFlight.Add(float64(delta))
}

// DiscoverySynced implements Recorder.
func (p *Prometheus) DiscoverySynced(synced bool) {
	value := 0.0
	if synced {
		value = 1.0
	}
	p.discoverySynced.Set(value)
}
