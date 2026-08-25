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
# HELP profgate_nats_connected Whether the NATS connection is currently up: 1 if up, 0 otherwise.
# TYPE profgate_nats_connected gauge
profgate_nats_connected 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "profgate_nats_connected"); err != nil {
		t.Errorf("profgate_nats_connected: %v", err)
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
	})
}
