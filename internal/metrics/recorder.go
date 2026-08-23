// Package metrics defines the Recorder interface through which the gateway
// reports requests, confirmations, and cache state, along with a Prometheus
// implementation and a no-op standing in for callers that do not need one.
package metrics

import "time"

// Endpoint names the route family a recorded request belongs to.
type Endpoint string

const (
	// EndpointTargets is the targets endpoint route family.
	EndpointTargets Endpoint = "targets"
	// EndpointProfile is the profile-fetch endpoint route family.
	EndpointProfile Endpoint = "profile"
)

// Recorder records the metrics the gateway's handlers produce.
type Recorder interface {
	// Request records one completed /v1 request. endpoint and profile come from the resolved route
	// when there is one (method failures included): targets → ("targets","none"), a known profile
	// route → ("profile", name). Requests that fail before a route resolves, or name an unknown
	// profile, record ("profile","none").
	Request(endpoint Endpoint, profile, code string, d time.Duration)
	// Confirm records the outcome of one Pod confirmation call: "ok", "changed", or "unavailable".
	Confirm(result string)
	// ProfilesInFlight adjusts the count of profile fetches currently in progress by delta.
	ProfilesInFlight(delta int)
	// DiscoverySynced reports whether the discovery cache is currently synced.
	DiscoverySynced(synced bool)
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
