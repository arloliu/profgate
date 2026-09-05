package metrics

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A Noop satisfies Recorder.
var _ Recorder = Noop{}

// A Prometheus satisfies Recorder.
var _ Recorder = (*Prometheus)(nil)

func TestPrometheus_Request(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.Request(EndpointProfile, "cpu", "ok", 250*time.Millisecond)

	wantCounter := `
# HELP profgate_requests_total Total number of completed /v1 requests, by endpoint, profile, and response code.
# TYPE profgate_requests_total counter
profgate_requests_total{code="ok",endpoint="profile",profile="cpu"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantCounter), "profgate_requests_total"); err != nil {
		t.Errorf("profgate_requests_total: %v", err)
	}

	wantHistogram := `
# HELP profgate_request_duration_seconds Duration in seconds of completed /v1 requests, by profile.
# TYPE profgate_request_duration_seconds histogram
profgate_request_duration_seconds_bucket{profile="cpu",le="0.1"} 0
profgate_request_duration_seconds_bucket{profile="cpu",le="0.5"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="1"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="2"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="5"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="10"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="30"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="60"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="120"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="300"} 1
profgate_request_duration_seconds_bucket{profile="cpu",le="+Inf"} 1
profgate_request_duration_seconds_sum{profile="cpu"} 0.25
profgate_request_duration_seconds_count{profile="cpu"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantHistogram), "profgate_request_duration_seconds"); err != nil {
		t.Errorf("profgate_request_duration_seconds: %v", err)
	}
}

func TestPrometheus_Confirm(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.Confirm("changed")

	want := `
# HELP profgate_confirm_total Total number of Pod confirmation attempts, by result.
# TYPE profgate_confirm_total counter
profgate_confirm_total{result="changed"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_confirm_total"); err != nil {
		t.Errorf("profgate_confirm_total: %v", err)
	}
}

func TestPrometheus_ProfilesInFlight(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.ProfilesInFlight(1)
	rec.ProfilesInFlight(-1)

	want := `
# HELP profgate_profiles_in_flight Number of profile fetches currently in progress.
# TYPE profgate_profiles_in_flight gauge
profgate_profiles_in_flight 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_profiles_in_flight"); err != nil {
		t.Errorf("profgate_profiles_in_flight after +1/-1 does not net to 0: %v", err)
	}
}

func TestPrometheus_DiscoverySynced(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.DiscoverySynced(true)

	want := `
# HELP profgate_discovery_synced Whether the initial discovery sync has completed: 1 once every informer has finished its first list, 0 before that.
# TYPE profgate_discovery_synced gauge
profgate_discovery_synced 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_discovery_synced"); err != nil {
		t.Errorf("profgate_discovery_synced: %v", err)
	}
}

func TestPrometheus_PGOSyncedFrom(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	count, err := testutil.GatherAndCount(reg, "profgate_pgo_synced")
	if err != nil {
		t.Fatalf("gather before PGOSyncedFrom: %v", err)
	}
	if count != 0 {
		t.Errorf("profgate_pgo_synced series before PGOSyncedFrom = %d, want 0: "+
			"the series exists only when pgo.enabled, and nothing but this call declares that", count)
	}

	synced := true
	rec.PGOSyncedFrom(func() bool { return synced })

	const help = `
# HELP profgate_pgo_synced Whether every PGO watch has replayed under the current store generation and every cache has applied that replay: 1 if both, 0 otherwise.
# TYPE profgate_pgo_synced gauge
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(help+"profgate_pgo_synced 1\n"),
		"profgate_pgo_synced"); err != nil {
		t.Errorf("profgate_pgo_synced with both halves of the barrier held: %v", err)
	}

	// No recorder call stands between the two gathers.
	// The scrape asks the function rather than reading a value pushed to it,
	// so a generation that moved since the last scrape shows in the next one.
	synced = false

	if err := testutil.GatherAndCompare(reg, strings.NewReader(help+"profgate_pgo_synced 0\n"),
		"profgate_pgo_synced"); err != nil {
		t.Errorf("profgate_pgo_synced after the barrier shut: %v", err)
	}
}

func TestPrometheus_Collection(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.Collection("completed")

	want := `
# HELP profgate_collections_total Total number of Collections, by terminal result.
# TYPE profgate_collections_total counter
profgate_collections_total{result="completed"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_collections_total"); err != nil {
		t.Errorf("profgate_collections_total: %v", err)
	}
}

func TestPrometheus_CollectionSample(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.CollectionSample("failed")

	want := `
# HELP profgate_collection_samples_total Total number of worker samples, by result.
# TYPE profgate_collection_samples_total counter
profgate_collection_samples_total{result="failed"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_collection_samples_total"); err != nil {
		t.Errorf("profgate_collection_samples_total: %v", err)
	}
}

func TestPrometheus_CollectionDuration(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.CollectionDuration(45 * time.Second)

	want := `
# HELP profgate_collection_duration_seconds Duration in seconds of completed Collections.
# TYPE profgate_collection_duration_seconds histogram
profgate_collection_duration_seconds_bucket{le="10"} 0
profgate_collection_duration_seconds_bucket{le="30"} 0
profgate_collection_duration_seconds_bucket{le="60"} 1
profgate_collection_duration_seconds_bucket{le="120"} 1
profgate_collection_duration_seconds_bucket{le="300"} 1
profgate_collection_duration_seconds_bucket{le="600"} 1
profgate_collection_duration_seconds_bucket{le="1200"} 1
profgate_collection_duration_seconds_bucket{le="+Inf"} 1
profgate_collection_duration_seconds_sum 45
profgate_collection_duration_seconds_count 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_collection_duration_seconds"); err != nil {
		t.Errorf("profgate_collection_duration_seconds: %v", err)
	}
}

func TestPrometheus_ScheduleSlot(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.ScheduleSlot("won")

	want := `
# HELP profgate_schedule_slots_total Total number of scheduling attempts, by result.
# TYPE profgate_schedule_slots_total counter
profgate_schedule_slots_total{result="won"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_schedule_slots_total"); err != nil {
		t.Errorf("profgate_schedule_slots_total: %v", err)
	}
}

func TestPrometheus_SweeperDelete(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.SweeperDelete("orphan")

	want := `
# HELP profgate_sweeper_deletes_total Total number of sweeper deletions, by kind.
# TYPE profgate_sweeper_deletes_total counter
profgate_sweeper_deletes_total{kind="orphan"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_sweeper_deletes_total"); err != nil {
		t.Errorf("profgate_sweeper_deletes_total: %v", err)
	}
}

func TestPrometheus_CollectionsActive(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.CollectionsActive(1)
	rec.CollectionsActive(1)
	rec.CollectionsActive(-1)

	want := `
# HELP profgate_collections_active Number of Collections currently active.
# TYPE profgate_collections_active gauge
profgate_collections_active 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_collections_active"); err != nil {
		t.Errorf("profgate_collections_active: %v", err)
	}
}

func TestPrometheus_NATSConnected(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.NATSConnected(true)

	want := `
# HELP profgate_nats_connected Whether the NATS connection is currently up: 1 if up, 0 if down, or NaN on a process that makes none.
# TYPE profgate_nats_connected gauge
profgate_nats_connected 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_nats_connected"); err != nil {
		t.Errorf("profgate_nats_connected: %v", err)
	}
}

// TestPrometheus_NATSBeforeAnyConnection covers the gauge's construction-time state,
// before any connection has been attempted.
// A process that makes no NATS connection at all must not report a transport it never configured as down:
// NaN crosses no comparison the way 0 would,
// so a rule reading the gauge is inert on an install that runs no collection.
func TestPrometheus_NATSBeforeAnyConnection(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	if got := testutil.ToFloat64(rec.natsConnected); !math.IsNaN(got) {
		t.Errorf("profgate_nats_connected before any connection = %v, want NaN", got)
	}

	want := `
# HELP profgate_nats_connected Whether the NATS connection is currently up: 1 if up, 0 if down, or NaN on a process that makes none.
# TYPE profgate_nats_connected gauge
profgate_nats_connected NaN
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_nats_connected"); err != nil {
		t.Errorf("profgate_nats_connected before any connection: %v", err)
	}
}

// TestPrometheus_TLS covers the pair that makes a stalled rotation visible:
// the counter says a reload was attempted and how it ended, and the gauge says
// how long the certificate now being served is still good for.
func TestPrometheus_TLS(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.TLSReload("applied")
	rec.TLSReload("failed")
	rec.TLSCertificateExpiry(time.Unix(1800000000, 0))

	wantCounter := `
# HELP profgate_tls_reloads_total Total number of attempts to re-read the API listener certificate, by result.
# TYPE profgate_tls_reloads_total counter
profgate_tls_reloads_total{result="applied"} 1
profgate_tls_reloads_total{result="failed"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantCounter), "profgate_tls_reloads_total"); err != nil {
		t.Errorf("profgate_tls_reloads_total: %v", err)
	}

	wantGauge := `
# HELP profgate_tls_certificate_expiry_seconds When the certificate the API listener serves stops being valid, in seconds since the epoch, or NaN before one is loaded.
# TYPE profgate_tls_certificate_expiry_seconds gauge
profgate_tls_certificate_expiry_seconds 1.8e+09
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantGauge), "profgate_tls_certificate_expiry_seconds"); err != nil {
		t.Errorf("profgate_tls_certificate_expiry_seconds: %v", err)
	}
}

// TestPrometheus_TLSBeforeAnyCertificate covers the gauge's construction-time state,
// before Loader.apply has ever run.
// An install without server.tls never calls TLSCertificateExpiry,
// so the gauge must not read as a certificate that expired at the epoch:
// NaN crosses no threshold the way 0 would,
// and the reload counter has no series at all until a load is attempted.
func TestPrometheus_TLSBeforeAnyCertificate(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	if got := testutil.ToFloat64(rec.tlsCertificateTTL); !math.IsNaN(got) {
		t.Errorf("profgate_tls_certificate_expiry_seconds before any certificate = %v, want NaN", got)
	}

	want := `
# HELP profgate_tls_certificate_expiry_seconds When the certificate the API listener serves stops being valid, in seconds since the epoch, or NaN before one is loaded.
# TYPE profgate_tls_certificate_expiry_seconds gauge
profgate_tls_certificate_expiry_seconds NaN
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_tls_certificate_expiry_seconds"); err != nil {
		t.Errorf("profgate_tls_certificate_expiry_seconds before any certificate: %v", err)
	}

	count, err := testutil.GatherAndCount(reg, "profgate_tls_reloads_total")
	if err != nil {
		t.Fatalf("gather profgate_tls_reloads_total: %v", err)
	}
	if count != 0 {
		t.Errorf("profgate_tls_reloads_total series before any load attempt = %d, want 0", count)
	}
}

// TestPrometheus_AuthFailure covers the counter an operator alerts on when a
// mode starts refusing everyone: the reason label says which check failed
// without the credential appearing anywhere.
func TestPrometheus_AuthFailure(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.AuthFailure("basic", "bad_credential")
	rec.AuthFailure("basic", "bad_credential")

	want := `
# HELP profgate_auth_failures_total Total number of authentication failures answered 401, 429, or 503, by mode and reason.
# TYPE profgate_auth_failures_total counter
profgate_auth_failures_total{mode="basic",reason="bad_credential"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_auth_failures_total"); err != nil {
		t.Errorf("profgate_auth_failures_total: %v", err)
	}
}

func TestPrometheus_AuthSessionIssued(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.AuthSessionIssued()

	want := `
# HELP profgate_auth_sessions_issued_total Total number of browser sessions minted.
# TYPE profgate_auth_sessions_issued_total counter
profgate_auth_sessions_issued_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_auth_sessions_issued_total"); err != nil {
		t.Errorf("profgate_auth_sessions_issued_total: %v", err)
	}
}

func TestPrometheus_JWKSRefresh(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.JWKSRefresh("ok")
	rec.JWKSRefresh("failed")

	want := `
# HELP profgate_oidc_jwks_refresh_total Total number of signing key fetches, by result.
# TYPE profgate_oidc_jwks_refresh_total counter
profgate_oidc_jwks_refresh_total{result="failed"} 1
profgate_oidc_jwks_refresh_total{result="ok"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_oidc_jwks_refresh_total"); err != nil {
		t.Errorf("profgate_oidc_jwks_refresh_total: %v", err)
	}
}

func TestPrometheus_JWKSKeys(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.JWKSKeys(3)

	want := `
# HELP profgate_oidc_jwks_keys Number of usable signing keys currently held.
# TYPE profgate_oidc_jwks_keys gauge
profgate_oidc_jwks_keys 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_oidc_jwks_keys"); err != nil {
		t.Errorf("profgate_oidc_jwks_keys: %v", err)
	}
}

// TestPrometheus_JWKSAge covers the gauge that makes a stalled key fetch
// alertable. The value is the age at scrape time, not at the last fetch, so a
// gateway that stops fetching is visible without a second series to subtract.
// Before the first successful fetch it reads NaN: the process start is not a
// fetch, and NaN crosses no threshold the way 0 or a growing age would.
func TestPrometheus_JWKSAge(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	if got := testutil.ToFloat64(rec.jwksAge); !math.IsNaN(got) {
		t.Errorf("profgate_oidc_jwks_age_seconds before any fetch = %v, want NaN", got)
	}

	wantUnfetched := `
# HELP profgate_oidc_jwks_age_seconds Seconds since the last successful signing key fetch, or NaN before the first.
# TYPE profgate_oidc_jwks_age_seconds gauge
profgate_oidc_jwks_age_seconds NaN
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantUnfetched), "profgate_oidc_jwks_age_seconds"); err != nil {
		t.Errorf("profgate_oidc_jwks_age_seconds before any fetch: %v", err)
	}

	rec.JWKSFetched(time.Now().Add(-90 * time.Second))

	// Read through the collector rather than the exposition: the value moves
	// between scrapes by construction, so a text comparison cannot hold it.
	got := testutil.ToFloat64(rec.jwksAge)
	if math.Abs(got-90) > 1 {
		t.Errorf("profgate_oidc_jwks_age_seconds = %v, want within 1s of 90", got)
	}
}

func TestPrometheus_AuthFileReload(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.AuthFileReload("users", "failed")
	rec.AuthFileReload("cookie_key", "ok")

	want := `
# HELP profgate_auth_file_reload_total Total number of re-reads of an authentication file, by file and result.
# TYPE profgate_auth_file_reload_total counter
profgate_auth_file_reload_total{file="cookie_key",result="ok"} 1
profgate_auth_file_reload_total{file="users",result="failed"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_auth_file_reload_total"); err != nil {
		t.Errorf("profgate_auth_file_reload_total: %v", err)
	}
}

// TestPrometheus_CookieKeys covers how an operator confirms a staged key
// rotation reached every replica. The series a replica exposes must be exactly
// the keys it currently holds, so a key dropped from the file has to take its
// series with it rather than linger at 1 forever.
func TestPrometheus_CookieKeys(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	rec := NewPrometheus(reg)

	rec.CookieKeys([]CookieKey{
		{Fingerprint: "aaaaaaaa", Role: "current"},
		{Fingerprint: "bbbbbbbb", Role: "previous"},
	})

	wantBoth := `
# HELP profgate_auth_cookie_key_info One per loaded cookie key, by fingerprint and role, always 1.
# TYPE profgate_auth_cookie_key_info gauge
profgate_auth_cookie_key_info{fingerprint="aaaaaaaa",role="current"} 1
profgate_auth_cookie_key_info{fingerprint="bbbbbbbb",role="previous"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantBoth), "profgate_auth_cookie_key_info"); err != nil {
		t.Errorf("profgate_auth_cookie_key_info after loading two keys: %v", err)
	}

	rec.CookieKeys([]CookieKey{{Fingerprint: "bbbbbbbb", Role: "current"}})

	wantOne := `
# HELP profgate_auth_cookie_key_info One per loaded cookie key, by fingerprint and role, always 1.
# TYPE profgate_auth_cookie_key_info gauge
profgate_auth_cookie_key_info{fingerprint="bbbbbbbb",role="current"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantOne), "profgate_auth_cookie_key_info"); err != nil {
		t.Errorf("profgate_auth_cookie_key_info after dropping a key: %v", err)
	}
}

func TestNoop(t *testing.T) {
	t.Run("methods record nothing and do not panic", func(_ *testing.T) {
		var rec Recorder = Noop{}
		rec.Request(EndpointTargets, "none", "ok", time.Second)
		rec.Confirm("ok")
		rec.ProfilesInFlight(1)
		rec.ProfilesInFlight(-1)
		rec.DiscoverySynced(true)
		rec.Collection("completed")
		rec.CollectionSample("ok")
		rec.CollectionDuration(time.Second)
		rec.ScheduleSlot("won")
		rec.SweeperDelete("artifact")
		rec.CollectionsActive(1)
		rec.CollectionsActive(-1)
		rec.NATSConnected(true)
		rec.TLSReload("unchanged")
		rec.TLSCertificateExpiry(time.Now())
		rec.AuthFailure("basic", "missing")
		rec.AuthSessionIssued()
		rec.JWKSRefresh("ok")
		rec.JWKSKeys(1)
		rec.JWKSFetched(time.Now())
		rec.AuthFileReload("users", "ok")
		rec.CookieKeys([]CookieKey{{Fingerprint: "aaaaaaaa", Role: "current"}})
	})
}
