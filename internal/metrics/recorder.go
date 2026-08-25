// Package metrics defines the Recorder interface through
// which the gateway reports requests, confirmations, and cache state,
// along with a Prometheus implementation and a no-op standing in for callers
// that do not need one.
package metrics

import "time"

// Endpoint names the route family a recorded request belongs to.
type Endpoint string

const (
	// EndpointTargets is the targets endpoint route family.
	EndpointTargets Endpoint = "targets"
	// EndpointProfile is the profile-fetch endpoint route family.
	EndpointProfile Endpoint = "profile"
	// EndpointPGOPolicy is the PGO policy endpoint route family.
	EndpointPGOPolicy Endpoint = "pgo_policy"
	// EndpointCollections is the PGO collection-list/create endpoint route family.
	EndpointCollections Endpoint = "collections"
	// EndpointCollection is the PGO single-Collection endpoint route family.
	// profile is fixed to "cpu" for this endpoint.
	EndpointCollection Endpoint = "collection"
	// EndpointCollectionProfile is the PGO Collection download endpoint route family.
	// profile is fixed to "cpu" for this endpoint.
	EndpointCollectionProfile Endpoint = "collection_profile"
	// EndpointCollectionCancel is the PGO Collection cancel endpoint route family.
	// profile is fixed to "cpu" for this endpoint.
	EndpointCollectionCancel Endpoint = "collection_cancel"
)

// Recorder records the metrics the gateway's handlers produce.
type Recorder interface {
	// Request records one completed /v1 request.
	// endpoint and profile come from the resolved route when there is one (method failures included):
	// targets → ("targets","none"), a known profile route → ("profile", name).
	// Requests that fail before a route resolves, or name an unknown profile, record ("profile","none").
	Request(endpoint Endpoint, profile, code string, d time.Duration)
	// Confirm records the outcome of one Pod confirmation call: "ok", "changed", or "unavailable".
	Confirm(result string)
	// ProfilesInFlight adjusts the count of profile fetches currently in progress by delta.
	ProfilesInFlight(delta int)
	// DiscoverySynced reports whether the discovery cache is currently synced.
	DiscoverySynced(synced bool)
	// Collection records the terminal outcome of one Collection: "completed", "failed", "cancelled", or "expired".
	Collection(result string)
	// CollectionSample records the outcome of one worker sample: "ok" or "failed".
	CollectionSample(result string)
	// CollectionDuration records the wall-clock duration of one completed Collection.
	CollectionDuration(d time.Duration)
	// ScheduleSlot records the outcome of one scheduling attempt: "won", "lost", "busy", or "capacity".
	ScheduleSlot(result string)
	// SweeperDelete records one sweeper deletion, by kind:
	// "artifact", "record", "slot", "active", "orphan", or "probe".
	SweeperDelete(kind string)
	// CollectionsActive adjusts the count of Collections currently active by delta.
	CollectionsActive(delta int)
	// NATSConnected reports whether the NATS connection is currently up.
	NATSConnected(up bool)
}

// Noop implements Recorder with empty methods, for callers that need a
// Recorder but do not want to record anything.
type Noop struct{}

// Request implements Recorder and does nothing.
func (Noop) Request(Endpoint, string, string, time.Duration) {}

// Confirm implements Recorder and does nothing.
func (Noop) Confirm(string) {}

// ProfilesInFlight implements Recorder and does nothing.
func (Noop) ProfilesInFlight(int) {}

// DiscoverySynced implements Recorder and does nothing.
func (Noop) DiscoverySynced(bool) {}

// Collection implements Recorder and does nothing.
func (Noop) Collection(string) {}

// CollectionSample implements Recorder and does nothing.
func (Noop) CollectionSample(string) {}

// CollectionDuration implements Recorder and does nothing.
func (Noop) CollectionDuration(time.Duration) {}

// ScheduleSlot implements Recorder and does nothing.
func (Noop) ScheduleSlot(string) {}

// SweeperDelete implements Recorder and does nothing.
func (Noop) SweeperDelete(string) {}

// CollectionsActive implements Recorder and does nothing.
func (Noop) CollectionsActive(int) {}

// NATSConnected implements Recorder and does nothing.
func (Noop) NATSConnected(bool) {}
