package metrics

import (
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
# HELP profgate_discovery_synced Whether the discovery cache is currently synced: 1 if synced, 0 otherwise.
# TYPE profgate_discovery_synced gauge
profgate_discovery_synced 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_discovery_synced"); err != nil {
		t.Errorf("profgate_discovery_synced: %v", err)
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
	})
}
