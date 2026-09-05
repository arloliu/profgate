package httpapi

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/natskv"
)

// specCodes is the two error tables written out: the gateway's own and the PGO ones.
// The registry is compared against this rather than against itself,
// so a constant added without a place in either table fails here.
func specCodes() []string {
	return []string{
		// The gateway's Errors table, by status.
		"invalid_parameter", "seconds_exceeds_limit", "port_not_allowed",
		"unauthenticated",
		"realm_denied",
		"route_unknown", "service_not_found", "pod_not_found", "profile_unknown",
		"method_not_allowed",
		"service_selectorless",
		"too_many_profiles", "too_many_auth",
		"upstream_unreachable", "upstream_redirect",
		"not_ready", "no_targets", "target_changed", "discovery_unavailable", "auth_unavailable",
		"upstream_timeout",
		// The PGO Errors table, by status.
		"limit_exceeded",
		"config_api_disabled",
		"collection_not_found", "pgo_override_not_found",
		"version_conflict", "version_missing", "collection_not_completed",
		"collection_initializing", "collection_terminal", "idempotency_mismatch",
		"artifact_gone",
		"precondition_failed",
		"precondition_required",
		"collection_in_progress", "rate_limited", "capacity_exhausted",
		"pgo_disabled",
		"pgo_unavailable", "collector_unavailable",
	}
}

func TestEnvelopeCodesMatchTheErrorTables(t *testing.T) {
	got := slices.Sorted(slices.Values(EnvelopeCodes()))
	want := slices.Sorted(slices.Values(specCodes()))
	if !slices.Equal(got, want) {
		t.Errorf("the registry is %v, want %v", got, want)
	}
}

func TestEnvelopeCodesHasNoDuplicate(t *testing.T) {
	sorted := slices.Sorted(slices.Values(EnvelopeCodes()))
	if i := slices.Compact(slices.Clone(sorted)); len(i) != len(sorted) {
		t.Errorf("the registry holds a code twice: %v", sorted)
	}
}

func TestEnvelopeCodesIsCopied(t *testing.T) {
	first := EnvelopeCodes()
	first[0] = "mutated"
	if second := EnvelopeCodes(); second[0] == "mutated" {
		t.Error("EnvelopeCodes hands out the registry itself, not a copy")
	}
}

// TestAuditOnlyCodesAreNotRegistered pins the boundary the registry draws:
// an outcome that names what happened to a request but is never written into an
// envelope is not a code the OpenAPI document has to enumerate.
func TestAuditOnlyCodesAreNotRegistered(t *testing.T) {
	auditOnly := []string{
		codeOK, codeStreamFailed, codeInternalError, codeAuthRedirect,
		codeCASContended, codeArtifactStreamFail, codeClientGone, codeDrainExpired,
		"upstream_404", "upstream_500",
	}
	for _, code := range auditOnly {
		if slices.Contains(EnvelopeCodes(), code) {
			t.Errorf("%q is in the registry; it is never written into an envelope", code)
		}
	}
}

func TestStoreErrorCodes(t *testing.T) {
	rows := []struct {
		name string
		err  error
		want string
	}{
		{"key not found", natskv.ErrKeyNotFound, CodeCollectionNotFound},
		{"unavailable", natskv.ErrUnavailable, CodePGOUnavailable},
		{"anything else", errors.New("boom"), CodePGOUnavailable},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := storeError(row.err)
			if got.code != row.want {
				t.Errorf("code = %q, want %q", got.code, row.want)
			}
			if !slices.Contains(EnvelopeCodes(), got.code) {
				t.Errorf("code %q is not in the registry", got.code)
			}
		})
	}
}

func TestDiscoveryErrorCodes(t *testing.T) {
	rows := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"service not found", k8s.ErrServiceNotFound, http.StatusNotFound, CodeServiceNotFound},
		{"selectorless", k8s.ErrServiceSelectorless, http.StatusUnprocessableEntity, CodeServiceSelectorless},
		{"anything else", errors.New("boom"), http.StatusServiceUnavailable, CodeDiscoveryUnavailable},
	}
	rt := route{kind: kindTargets, namespace: fixtureNamespace, service: fixtureService}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := discoveryError(rt, row.err)
			if got.status != row.status {
				t.Errorf("status = %d, want %d", got.status, row.status)
			}
			if got.code != row.want {
				t.Errorf("code = %q, want %q", got.code, row.want)
			}
			if !slices.Contains(EnvelopeCodes(), got.code) {
				t.Errorf("code %q is not in the registry", got.code)
			}
		})
	}
}
