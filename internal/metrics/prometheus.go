package metrics

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// A Prometheus records metrics through github.com/prometheus/client_golang.
type Prometheus struct {
	requests           *prometheus.CounterVec
	requestDuration    *prometheus.HistogramVec
	confirms           *prometheus.CounterVec
	profilesInFlight   prometheus.Gauge
	discoverySynced    prometheus.Gauge
	collections        *prometheus.CounterVec
	collectionSamples  *prometheus.CounterVec
	collectionDuration prometheus.Histogram
	scheduleSlots      *prometheus.CounterVec
	sweeperDeletes     *prometheus.CounterVec
	collectionsActive  prometheus.Gauge
	natsConnected      prometheus.Gauge
	tlsReloads         *prometheus.CounterVec
	tlsCertificateTTL  prometheus.Gauge
	authFailures       *prometheus.CounterVec
	authSessions       prometheus.Counter
	jwksRefreshes      *prometheus.CounterVec
	jwksKeys           prometheus.Gauge
	jwksAge            prometheus.GaugeFunc
	authFileReloads    *prometheus.CounterVec
	cookieKeys         *prometheus.GaugeVec

	// jwksFetched is the Unix time of the last successful key fetch, or 0
	// before the first. jwksAge reads it on the scrape goroutine.
	jwksFetched atomic.Int64
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
		collections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_collections_total",
			Help: "Total number of Collections, by terminal result.",
		}, []string{"result"}),
		collectionSamples: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_collection_samples_total",
			Help: "Total number of worker samples, by result.",
		}, []string{"result"}),
		collectionDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "profgate_collection_duration_seconds",
			Help:    "Duration in seconds of completed Collections.",
			Buckets: []float64{10, 30, 60, 120, 300, 600, 1200},
		}),
		scheduleSlots: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_schedule_slots_total",
			Help: "Total number of scheduling attempts, by result.",
		}, []string{"result"}),
		sweeperDeletes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_sweeper_deletes_total",
			Help: "Total number of sweeper deletions, by kind.",
		}, []string{"kind"}),
		collectionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "profgate_collections_active",
			Help: "Number of Collections currently active.",
		}),
		natsConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "profgate_nats_connected",
			Help: "Whether the NATS connection is currently up: 1 if up, 0 otherwise.",
		}),
		tlsReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_tls_reloads_total",
			Help: "Total number of attempts to re-read the API listener certificate, by result.",
		}, []string{"result"}),
		tlsCertificateTTL: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "profgate_tls_certificate_expiry_seconds",
			Help: "When the certificate the API listener serves stops being valid, in seconds since the epoch.",
		}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_auth_failures_total",
			Help: "Total number of authentication failures answered 401, 429, or 503, by mode and reason.",
		}, []string{"mode", "reason"}),
		authSessions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "profgate_auth_sessions_issued_total",
			Help: "Total number of browser sessions minted.",
		}),
		jwksRefreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_oidc_jwks_refresh_total",
			Help: "Total number of signing key fetches, by result.",
		}, []string{"result"}),
		jwksKeys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "profgate_oidc_jwks_keys",
			Help: "Number of usable signing keys currently held.",
		}),
		authFileReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "profgate_auth_file_reload_total",
			Help: "Total number of re-reads of an authentication file, by file and result.",
		}, []string{"file", "result"}),
		cookieKeys: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "profgate_auth_cookie_key_info",
			Help: "One per loaded cookie key, by fingerprint and role, always 1.",
		}, []string{"fingerprint", "role"}),
	}

	// The age is computed when the scrape asks, not when the fetch happened,
	// so a gateway that stopped fetching keeps climbing towards jwksMaxStale
	// on its own. Before the first successful fetch the value is NaN: process
	// start is not a fetch, and NaN crosses no alert threshold the way 0 or a
	// growing age would.
	p.jwksAge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "profgate_oidc_jwks_age_seconds",
		Help: "Seconds since the last successful signing key fetch, or NaN before the first.",
	}, func() float64 {
		at := p.jwksFetched.Load()
		if at == 0 {
			return math.NaN()
		}

		return time.Since(time.Unix(at, 0)).Seconds()
	})

	reg.MustRegister(
		p.requests, p.requestDuration, p.confirms, p.profilesInFlight, p.discoverySynced,
		p.collections, p.collectionSamples, p.collectionDuration, p.scheduleSlots, p.sweeperDeletes,
		p.collectionsActive, p.natsConnected, p.tlsReloads, p.tlsCertificateTTL,
		p.authFailures, p.authSessions, p.jwksRefreshes, p.jwksKeys, p.jwksAge,
		p.authFileReloads, p.cookieKeys,
	)

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

// Collection implements Recorder.
func (p *Prometheus) Collection(result string) {
	p.collections.WithLabelValues(result).Inc()
}

// CollectionSample implements Recorder.
func (p *Prometheus) CollectionSample(result string) {
	p.collectionSamples.WithLabelValues(result).Inc()
}

// CollectionDuration implements Recorder.
func (p *Prometheus) CollectionDuration(d time.Duration) {
	p.collectionDuration.Observe(d.Seconds())
}

// ScheduleSlot implements Recorder.
func (p *Prometheus) ScheduleSlot(result string) {
	p.scheduleSlots.WithLabelValues(result).Inc()
}

// SweeperDelete implements Recorder.
func (p *Prometheus) SweeperDelete(kind string) {
	p.sweeperDeletes.WithLabelValues(kind).Inc()
}

// CollectionsActive implements Recorder.
func (p *Prometheus) CollectionsActive(delta int) {
	p.collectionsActive.Add(float64(delta))
}

// NATSConnected implements Recorder.
func (p *Prometheus) NATSConnected(up bool) {
	value := 0.0
	if up {
		value = 1.0
	}
	p.natsConnected.Set(value)
}

// TLSReload implements Recorder.
func (p *Prometheus) TLSReload(result string) {
	p.tlsReloads.WithLabelValues(result).Inc()
}

// TLSCertificateExpiry implements Recorder.
func (p *Prometheus) TLSCertificateExpiry(notAfter time.Time) {
	p.tlsCertificateTTL.Set(float64(notAfter.Unix()))
}

// AuthFailure implements Recorder.
func (p *Prometheus) AuthFailure(mode, reason string) {
	p.authFailures.WithLabelValues(mode, reason).Inc()
}

// AuthSessionIssued implements Recorder.
func (p *Prometheus) AuthSessionIssued() {
	p.authSessions.Inc()
}

// JWKSRefresh implements Recorder.
func (p *Prometheus) JWKSRefresh(result string) {
	p.jwksRefreshes.WithLabelValues(result).Inc()
}

// JWKSKeys implements Recorder.
func (p *Prometheus) JWKSKeys(n int) {
	p.jwksKeys.Set(float64(n))
}

// JWKSFetched implements Recorder.
func (p *Prometheus) JWKSFetched(at time.Time) {
	p.jwksFetched.Store(at.Unix())
}

// AuthFileReload implements Recorder.
func (p *Prometheus) AuthFileReload(file, result string) {
	p.authFileReloads.WithLabelValues(file, result).Inc()
}

// CookieKeys implements Recorder.
// The set is replaced rather than added to, so a key no longer in the file
// loses its series instead of reporting 1 forever.
func (p *Prometheus) CookieKeys(keys []CookieKey) {
	p.cookieKeys.Reset()
	for _, k := range keys {
		p.cookieKeys.WithLabelValues(k.Fingerprint, k.Role).Set(1)
	}
}
