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
	// EndpointAuth is the route family of the three /auth/ routes.
	EndpointAuth Endpoint = "auth"
	// EndpointNamespaces is the namespace listing route family; profile is fixed to "none".
	EndpointNamespaces Endpoint = "namespaces"
	// EndpointServices is the Service listing route family; profile is fixed to "none".
	EndpointServices Endpoint = "services"
	// EndpointWhoami is the caller description route family; profile is fixed to "none".
	EndpointWhoami Endpoint = "whoami"
	// EndpointLimits is the limits description route family; profile is fixed to "none".
	EndpointLimits Endpoint = "limits"
	// EndpointUI covers /ui/, every path under it, and /; profile is fixed to "none".
	EndpointUI Endpoint = "ui"
	// EndpointOpenAPI is the document route; profile is fixed to "none".
	// Its codes are "ok", "not_ready", "method_not_allowed", and
	// "invalid_parameter", which is every answer the route has.
	EndpointOpenAPI Endpoint = "openapi"
)

// CookieKey is one loaded cookie key as the info gauge reports it.
// Fingerprint is the first 8 hex digits of SHA-256 over the key, which lets an
// operator watch a key rotation reach every replica without reading key
// material. Role is "current" or "previous".
type CookieKey struct {
	Fingerprint string
	Role        string
}

// Recorder records the metrics the gateway's handlers produce.
type Recorder interface {
	// Request records one completed /v1 request.
	// endpoint and profile come from the resolved route when there is one (method failures included):
	// targets → ("targets","none"), a known profile route → ("profile", name),
	// the listing routes → ("namespaces","none"), ("services","none"), ("whoami","none"), ("limits","none"),
	// the document route → ("openapi","none"),
	// and the console → ("ui","none").
	// Requests that fail before a route resolves, or name an unknown profile, record ("profile","none").
	Request(endpoint Endpoint, profile, code string, d time.Duration)
	// Confirm records the outcome of one Pod confirmation call: "ok", "changed", or "unavailable".
	Confirm(result string)
	// ProfilesInFlight adjusts the count of profile fetches currently in progress by delta.
	ProfilesInFlight(delta int)
	// DiscoverySynced reports whether the initial informer sync has completed.
	// It is called once, on the branch that reports that sync,
	// and never reports false afterwards.
	DiscoverySynced(synced bool)
	// PGOSyncedFrom registers read as the source of the PGO replay barrier gauge.
	// The gauge exists from this call on, and never before it,
	// because the series exists only when pgo.enabled;
	// read answers whether both halves of the barrier hold under the store generation current at that moment,
	// and a scrape calls it rather than reading a value pushed earlier.
	PGOSyncedFrom(read func() bool)
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
	// TLSReload records one attempt to re-read the API listener's certificate:
	// "applied", "unchanged", or "failed".
	TLSReload(result string)
	// TLSCertificateExpiry reports when the certificate the API listener is
	// serving stops being valid.
	TLSCertificateExpiry(notAfter time.Time)
	// AuthFailure records one authentication failure answered 401, 429, or 503.
	// A redirect into the browser flow is not a failure.
	AuthFailure(mode, reason string)
	// AuthSessionIssued records one browser session minted.
	AuthSessionIssued()
	// JWKSRefresh records one signing key fetch: "ok" or "failed".
	JWKSRefresh(result string)
	// JWKSKeys reports how many usable signing keys are held.
	JWKSKeys(n int)
	// JWKSFetched reports when the last successful key fetch happened.
	// The age reported to a scrape is derived from it.
	JWKSFetched(at time.Time)
	// AuthFileReload records one poll of a re-read file:
	// file is "users" or "cookie_key", result is "ok" or "failed".
	AuthFileReload(file, result string)
	// CookieKeys replaces the set of loaded cookie keys the info gauge reports,
	// so a key dropped from the file stops being reported.
	CookieKeys(keys []CookieKey)
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

// PGOSyncedFrom implements Recorder and does nothing.
func (Noop) PGOSyncedFrom(func() bool) {}

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

// TLSReload implements Recorder and does nothing.
func (Noop) TLSReload(string) {}

// TLSCertificateExpiry implements Recorder and does nothing.
func (Noop) TLSCertificateExpiry(time.Time) {}

// AuthFailure implements Recorder and does nothing.
func (Noop) AuthFailure(string, string) {}

// AuthSessionIssued implements Recorder and does nothing.
func (Noop) AuthSessionIssued() {}

// JWKSRefresh implements Recorder and does nothing.
func (Noop) JWKSRefresh(string) {}

// JWKSKeys implements Recorder and does nothing.
func (Noop) JWKSKeys(int) {}

// JWKSFetched implements Recorder and does nothing.
func (Noop) JWKSFetched(time.Time) {}

// AuthFileReload implements Recorder and does nothing.
func (Noop) AuthFileReload(string, string) {}

// CookieKeys implements Recorder and does nothing.
func (Noop) CookieKeys([]CookieKey) {}
