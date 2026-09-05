package httpapi

import "slices"

// The complete set of codes the gateway writes into an error envelope:
// the interactive gateway's own and the PGO ones.
// No handler writes a code that is not here,
// and the OpenAPI document enumerates exactly this set.
// The constants are exported because the console writes two of them through
// [ErrorEnvelope], and because the case distinguishes an envelope code from the
// audit-only outcomes named below, which are spelled in lower case.
const (
	// CodeInvalidParameter refuses a query parameter, a header, or a body field.
	CodeInvalidParameter = "invalid_parameter"
	// CodeSecondsExceedsLimit refuses an effective duration above the profile's ceiling.
	CodeSecondsExceedsLimit = "seconds_exceeds_limit"
	// CodePortNotAllowed refuses a port selection discovery.pprof.allowedSelections does not admit.
	CodePortNotAllowed = "port_not_allowed"
	// CodeLimitExceeded refuses an effective policy above a ceiling.
	CodeLimitExceeded = "limit_exceeded"
	// CodeUnauthenticated is every authentication failure, whatever its reason.
	CodeUnauthenticated = "unauthenticated"
	// CodeRealmDenied is every realm refusal, whether or not the Service exists.
	CodeRealmDenied = "realm_denied"
	// CodeConfigAPIDisabled refuses a policy write while pgo.configAPI is disabled.
	CodeConfigAPIDisabled = "config_api_disabled"
	// CodeRouteUnknown is a path no declaration of the route table matches.
	CodeRouteUnknown = "route_unknown"
	// CodeServiceNotFound is a Service the cache does not hold.
	CodeServiceNotFound = "service_not_found"
	// CodePodNotFound is a Pod that is not an eligible target of the Service.
	CodePodNotFound = "pod_not_found"
	// CodeProfileUnknown is a profile name the gateway does not serve.
	CodeProfileUnknown = "profile_unknown"
	// CodeCollectionNotFound is a Collection that does not exist and one the realm denies.
	CodeCollectionNotFound = "collection_not_found"
	// CodePGOOverrideNotFound is a Service with no stored policy override.
	CodePGOOverrideNotFound = "pgo_override_not_found"
	// CodeMethodNotAllowed is a method the matched declaration does not list.
	CodeMethodNotAllowed = "method_not_allowed"
	// CodeVersionConflict refuses a Collection whose targets carry more than one version.
	CodeVersionConflict = "version_conflict"
	// CodeVersionMissing refuses a Collection whose targets carry no usable version.
	CodeVersionMissing = "version_missing"
	// CodeCollectionNotCompleted is a Collection with no stored profile.
	CodeCollectionNotCompleted = "collection_not_completed"
	// CodeCollectionInitializing is a Collection still being published.
	CodeCollectionInitializing = "collection_initializing"
	// CodeCollectionTerminal is a Collection that has already finished.
	CodeCollectionTerminal = "collection_terminal"
	// CodeIdempotencyMismatch is an idempotency key that already stands for another request.
	// POST /collections is the one route that answers it.
	CodeIdempotencyMismatch = "idempotency_mismatch"
	// CodeArtifactGone is a Collection whose merged profile is no longer stored.
	CodeArtifactGone = "artifact_gone"
	// CodePreconditionFailed is an If-Match naming a revision the policy has moved past.
	CodePreconditionFailed = "precondition_failed"
	// CodeServiceSelectorless is a Service with no selector.
	CodeServiceSelectorless = "service_selectorless"
	// CodePreconditionRequired is a policy write over an existing override with no If-Match.
	CodePreconditionRequired = "precondition_required"
	// CodeTooManyProfiles is the admission gate with no free slot.
	CodeTooManyProfiles = "too_many_profiles"
	// CodeTooManyAuth is the authentication rate limit.
	CodeTooManyAuth = "too_many_auth"
	// CodeCollectionInProgress is a Service that already has a live Collection.
	CodeCollectionInProgress = "collection_in_progress"
	// CodeRateLimited is the on-demand Collection rate limit.
	CodeRateLimited = "rate_limited"
	// CodeCapacityExhausted is this replica running its limit of live Collections.
	CodeCapacityExhausted = "capacity_exhausted"
	// CodePGODisabled is a PGO route while pgo.enabled is false.
	CodePGODisabled = "pgo_disabled"
	// CodeUpstreamUnreachable is a target that could not be dialled.
	CodeUpstreamUnreachable = "upstream_unreachable"
	// CodeUpstreamRedirect is a target that answered with a redirect.
	CodeUpstreamRedirect = "upstream_redirect"
	// CodeNotReady is the readiness step before the caches have synced.
	CodeNotReady = "not_ready"
	// CodeNoTargets is a Service with no eligible target.
	CodeNoTargets = "no_targets"
	// CodeTargetChanged is a target the API server no longer vouches for.
	CodeTargetChanged = "target_changed"
	// CodeDiscoveryUnavailable is a cache read the gateway cannot complete.
	CodeDiscoveryUnavailable = "discovery_unavailable"
	// CodeAuthUnavailable is an authenticator that cannot decide.
	CodeAuthUnavailable = "auth_unavailable"
	// CodePGOUnavailable is a PGO store this replica cannot decide from.
	CodePGOUnavailable = "pgo_unavailable"
	// CodeCollectorUnavailable is a store that is reachable with nothing running Collections.
	// No route answers it in this build.
	CodeCollectorUnavailable = "collector_unavailable"
	// CodeUpstreamTimeout is a target that did not answer in time.
	CodeUpstreamTimeout = "upstream_timeout"
)

// envelopeCodes is the registry, in the order of the two error tables it restates:
// by status, and within a status as those tables list it.
// It is immutable; EnvelopeCodes hands out a copy.
//
// The audit-only outcomes are deliberately absent.
// cas_contended, artifact_stream_failed, client_gone, upstream_stream_failed,
// drain_expired, internal_error, and ok name what happened to a request
// in its audit record and its metrics row, and none is ever written into an envelope.
// A status passed through under upstream_<status> carries the application's body, not an envelope.
var envelopeCodes = [...]string{
	CodeInvalidParameter,
	CodeSecondsExceedsLimit,
	CodePortNotAllowed,
	CodeLimitExceeded,
	CodeUnauthenticated,
	CodeRealmDenied,
	CodeConfigAPIDisabled,
	CodeRouteUnknown,
	CodeServiceNotFound,
	CodePodNotFound,
	CodeProfileUnknown,
	CodeCollectionNotFound,
	CodePGOOverrideNotFound,
	CodeMethodNotAllowed,
	CodeVersionConflict,
	CodeVersionMissing,
	CodeCollectionNotCompleted,
	CodeCollectionInitializing,
	CodeCollectionTerminal,
	CodeIdempotencyMismatch,
	CodeArtifactGone,
	CodePreconditionFailed,
	CodeServiceSelectorless,
	CodePreconditionRequired,
	CodeTooManyProfiles,
	CodeTooManyAuth,
	CodeCollectionInProgress,
	CodeRateLimited,
	CodeCapacityExhausted,
	CodePGODisabled,
	CodeUpstreamUnreachable,
	CodeUpstreamRedirect,
	CodeNotReady,
	CodeNoTargets,
	CodeTargetChanged,
	CodeDiscoveryUnavailable,
	CodeAuthUnavailable,
	CodePGOUnavailable,
	CodeCollectorUnavailable,
	CodeUpstreamTimeout,
}

// EnvelopeCodes returns the registry in a stable order.
// The caller gets a copy, so nothing it does reaches the registry itself.
func EnvelopeCodes() []string {
	return slices.Clone(envelopeCodes[:])
}
