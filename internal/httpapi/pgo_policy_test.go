package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/pgo"
)

// decodePolicy reads a policy response.
func decodePolicy(t *testing.T, body []byte) policyBody {
	t.Helper()

	var got policyBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("policy body %q is not readable: %v", body, err)
	}

	return got
}

// TestPolicyReadWithoutAnOverride pins what a Service that has never been
// configured answers: the operator's defaults, disabled, with no ETag to write
// against.
func TestPolicyReadWithoutAnOverride(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	got := h.doPGO(t, http.MethodGet, pgoPath, "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if etag := got.Header().Get("ETag"); etag != "" {
		t.Errorf("ETag = %q, want none for a Service without an override", etag)
	}
	body := decodePolicy(t, got.Body.Bytes())
	switch {
	case body.Source != sourceDefaults:
		t.Errorf("source = %q, want %q", body.Source, sourceDefaults)
	case body.Override != nil:
		t.Errorf("override = %+v, want null", body.Override)
	case body.Effective.Enabled:
		t.Error("effective.enabled is true; enabled has no operator default")
	case body.Violations == nil:
		t.Error("violations is null, want an empty list")
	}
	h.expectPGOAudit(t, http.StatusOK, codeOK)
}

// TestPolicyReadWithAnOverride pins the ETag: the revision of the stored key,
// which is what the next write is conditional on.
func TestPolicyReadWithAnOverride(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	enabled := true
	every := pgo.Duration(time.Hour)
	revision := h.seedOverride(t, &pgo.PolicyOverride{
		Enabled:  &enabled,
		Schedule: &pgo.ScheduleOverride{Every: &every},
	})

	got := h.doPGO(t, http.MethodGet, pgoPath, "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if etag := got.Header().Get("ETag"); etag != etagOf(revision) {
		t.Errorf("ETag = %q, want %q", etag, etagOf(revision))
	}
	body := decodePolicy(t, got.Body.Bytes())
	if body.Source != sourceOverride || body.Override == nil {
		t.Fatalf("body = %+v, want the stored override", body)
	}
	if !body.Effective.Enabled || body.Effective.Schedule.Every.Duration() != time.Hour {
		t.Errorf("effective = %+v, want the override layered on the defaults", body.Effective)
	}
	if body.Effective.Sampling.Rounds != testPGODefaults().Sampling.Rounds {
		t.Errorf("effective.sampling.rounds = %d, want the default", body.Effective.Sampling.Rounds)
	}
	if body.UpdatedBy != "anonymous" {
		t.Errorf("updatedBy = %q, want %q", body.UpdatedBy, "anonymous")
	}
}

// TestPolicyReadReportsViolations proves a stored policy is measured against
// the ceilings as they are now, so a lowered pgo.limits key shows up as a
// field a reader can see rather than as silence.
func TestPolicyReadReportsViolations(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{limits: testPGOLimits(func(l *config.PGOLimits) { l.MaxRounds = 2 })})
	rounds := 5
	h.seedOverride(t, &pgo.PolicyOverride{Sampling: &pgo.SamplingOverride{Rounds: &rounds}})

	got := h.doPGO(t, http.MethodGet, pgoPath, "", nil)

	body := decodePolicy(t, got.Body.Bytes())
	if len(body.Violations) != 1 || body.Violations[0].Field != "sampling.rounds" {
		t.Fatalf("violations = %+v, want the one field over its ceiling", body.Violations)
	}
	if body.Violations[0].Ceiling != "pgo.limits.maxRounds" {
		t.Errorf("ceiling = %q, want the key that refuses it", body.Violations[0].Ceiling)
	}
}

// TestPolicyIfMatchMatrix is the table of section "Policy": whether the key is
// there and what If-Match names decide together which of create, update, and
// refusal a write is.
func TestPolicyIfMatchMatrix(t *testing.T) {
	const body = `{"enabled":true,"sampling":{"rounds":3}}`

	cases := []struct {
		name string
		// seeded stores an override first, so the key is present.
		seeded bool
		// ifMatch is the header, with "current" standing for the stored revision.
		ifMatch string
		status  int
		code    string
	}{
		{"absent key, no If-Match", false, "", http.StatusCreated, ""},
		{"absent key, If-Match present", false, `"7"`, http.StatusPreconditionFailed, "precondition_failed"},
		{"present key, no If-Match", true, "", http.StatusPreconditionRequired, "precondition_required"},
		{"present key, If-Match current", true, "current", http.StatusOK, ""},
		{"present key, If-Match stale", true, "stale", http.StatusPreconditionFailed, "precondition_failed"},
		{"If-Match is a wildcard", true, `*`, http.StatusBadRequest, "invalid_parameter"},
		{"If-Match is unquoted", true, `42`, http.StatusBadRequest, "invalid_parameter"},
		{"If-Match is not a revision", true, `"W/\"42\""`, http.StatusBadRequest, "invalid_parameter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			header := http.Header{}
			if tc.seeded {
				enabled := false
				revision := h.seedOverride(t, &pgo.PolicyOverride{Enabled: &enabled})
				switch tc.ifMatch {
				case "current":
					tc.ifMatch = etagOf(revision)
				case "stale":
					tc.ifMatch = etagOf(revision + 1)
				}
			}
			if tc.ifMatch != "" {
				header = ifMatch(tc.ifMatch)
			}

			got := h.doPGO(t, http.MethodPut, pgoPath, body, header)

			if tc.code != "" {
				h.expectPGOError(t, got, tc.status, tc.code, tc.code)

				return
			}
			if got.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", got.Code, tc.status, got.Body.String())
			}
			if got.Header().Get("ETag") == "" {
				t.Error("a successful write carries no ETag")
			}
			stored := decodePolicy(t, got.Body.Bytes())
			if stored.Source != sourceOverride || stored.Effective.Sampling.Rounds != 3 {
				t.Errorf("body = %+v, want the written override", stored)
			}
		})
	}
}

// TestPolicyWriteRefusesACeilingViolation proves a write is measured before it
// is stored, so a policy the workers would refuse never reaches the bucket.
func TestPolicyWriteRefusesACeilingViolation(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	got := h.doPGO(t, http.MethodPut, pgoPath, `{"enabled":true,"sampling":{"rounds":99}}`, nil)

	h.expectPGOError(t, got, http.StatusBadRequest, "limit_exceeded", "limit_exceeded")
	if n := h.nats.config.countKeys(overrideKeyPrefix); n != 0 {
		t.Errorf("override keys = %d, want none written", n)
	}
}

// TestPolicyWriteLosingItsCreate proves the conditional write is the decision:
// a first write that loses its Create to another writer is a lost condition,
// which the client re-reads and decides again from.
func TestPolicyWriteLosingItsCreate(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.nats.config.createErr = natskv.ErrKeyExists

	got := h.doPGO(t, http.MethodPut, pgoPath, `{"enabled":true}`, nil)

	h.expectPGOError(t, got, http.StatusPreconditionFailed, "precondition_failed", "precondition_failed")
}

// TestPolicyDeleteMatrix pins the delete conditions, including the one that
// separates a delete from a read-then-delete: a key that moved between the two
// answers 412, the same as a stale If-Match.
func TestPolicyDeleteMatrix(t *testing.T) {
	t.Run("no override", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodDelete, pgoPath, "", ifMatch(`"3"`))

		h.expectPGOError(t, got, http.StatusNotFound, "pgo_override_not_found", "pgo_override_not_found")
	})

	t.Run("no If-Match", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		enabled := true
		h.seedOverride(t, &pgo.PolicyOverride{Enabled: &enabled})

		got := h.doPGO(t, http.MethodDelete, pgoPath, "", nil)

		h.expectPGOError(t, got, http.StatusPreconditionRequired, "precondition_required", "precondition_required")
	})

	t.Run("stale If-Match", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		enabled := true
		revision := h.seedOverride(t, &pgo.PolicyOverride{Enabled: &enabled})

		got := h.doPGO(t, http.MethodDelete, pgoPath, "", ifMatch(etagOf(revision-1)))

		h.expectPGOError(t, got, http.StatusPreconditionFailed, "precondition_failed", "precondition_failed")
		if n := h.nats.config.countKeys(overrideKeyPrefix); n != 1 {
			t.Errorf("override keys = %d, want the override kept", n)
		}
	})

	t.Run("current If-Match", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		enabled := true
		revision := h.seedOverride(t, &pgo.PolicyOverride{Enabled: &enabled})

		got := h.doPGO(t, http.MethodDelete, pgoPath, "", ifMatch(etagOf(revision)))

		if got.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %q)", got.Code, got.Body.String())
		}
		if n := h.nats.config.countKeys(overrideKeyPrefix); n != 0 {
			t.Errorf("override keys = %d, want the override gone", n)
		}
		h.expectPGOAudit(t, http.StatusNoContent, codeOK)
	})

	t.Run("the key moves between the read and the delete", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		enabled := true
		revision := h.seedOverride(t, &pgo.PolicyOverride{Enabled: &enabled})
		// The read gives the handler the revision it deletes at, and the key
		// moves before the delete reaches it: the same lost condition as a
		// stale If-Match, which the client re-reads and decides again from.
		var moved atomic.Uint64
		h.nats.config.afterGet = func() {
			h.nats.config.afterGet = nil
			moved.Store(h.nats.config.put(t, overrideKeyPrefix+fixtureNamespace+"."+fixtureService,
				pgo.StoredOverride{Policy: &pgo.PolicyOverride{}, UpdatedBy: "someone-else", UpdatedAt: pgoFixtureNow}))
		}

		got := h.doPGO(t, http.MethodDelete, pgoPath, "", ifMatch(etagOf(revision)))

		h.expectPGOError(t, got, http.StatusPreconditionFailed, "precondition_failed", "precondition_failed")
		if n := h.nats.config.countKeys(overrideKeyPrefix); n != 1 || moved.Load() == revision {
			t.Errorf("override keys = %d at revision %d, want the newer value kept", n, moved.Load())
		}
	})
}

// TestPolicyWritesRefusedWhenTheConfigAPIIsOff proves pgo.configAPI closes the
// two writes and leaves the read open.
func TestPolicyWritesRefusedWhenTheConfigAPIIsOff(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{configAPI: "disabled"})

			got := h.doPGO(t, method, pgoPath, `{"enabled":true}`, nil)

			h.expectPGOError(t, got, http.StatusForbidden, "config_api_disabled", "config_api_disabled")
			if n := h.nats.config.countKeys(overrideKeyPrefix); n != 0 {
				t.Errorf("override keys = %d, want none written", n)
			}
		})
	}

	t.Run("GET", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{configAPI: "disabled"})

		if got := h.doPGO(t, http.MethodGet, pgoPath, "", nil); got.Code != http.StatusOK {
			t.Errorf("status = %d, want the read to stay open", got.Code)
		}
	})
}

// TestPolicyStoreFailuresAreUnavailable proves a store the handler cannot
// reach is the client's 503 and never a decision taken from silence.
func TestPolicyStoreFailuresAreUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		body    string
		ifMatch http.Header
		fail    func(*fakeKV)
	}{
		{"the read fails", http.MethodGet, `{"enabled":true}`, nil, func(kv *fakeKV) { kv.getErr = natskv.ErrUnavailable }},
		{"the create fails", http.MethodPut, `{"enabled":true}`, nil, func(kv *fakeKV) { kv.createErr = natskv.ErrUnavailable }},
		// The delete takes no body, so the row that exercises it sends none.
		{"the delete's read fails", http.MethodDelete, "", ifMatch(`"1"`), func(kv *fakeKV) { kv.getErr = natskv.ErrUnavailable }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			tc.fail(h.nats.config)

			got := h.doPGO(t, tc.method, pgoPath, tc.body, tc.ifMatch)

			h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
		})
	}
}

// TestLimitExceededDetails pins the machine form of a ceiling refusal:
// one item per violation, in the order validation produced them,
// with the policy field written as a JSON pointer and the violation's own code beside it.
func TestLimitExceededDetails(t *testing.T) {
	t.Run("one field over its ceiling", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodPut, pgoPath, `{"enabled":true,"sampling":{"rounds":99}}`, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "limit_exceeded", "limit_exceeded")
		expectDetails(t, got, "limit_exceeded",
			[]errorDetail{{Field: "/sampling/rounds", Code: "above_maximum"}})
	})

	t.Run("several fields keep validation order", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		body := `{"enabled":true,"schedule":{"every":"48h"},"sampling":{"rounds":0},` +
			`"target":{"versionPolicy":"loose"}}`

		got := h.doPGO(t, http.MethodPut, pgoPath, body, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "limit_exceeded", "limit_exceeded")
		expectDetails(t, got, "limit_exceeded", []errorDetail{
			{Field: "/schedule/every", Code: "above_maximum"},
			{Field: "/sampling/rounds", Code: "below_minimum"},
			{Field: "/target/versionPolicy", Code: "not_permitted"},
		})
	})

	t.Run("a collection request answers the same way", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"rounds":99}}`, jsonType())

		h.expectPGOError(t, got, http.StatusBadRequest, "limit_exceeded", "limit_exceeded")
		expectDetails(t, got, "limit_exceeded",
			[]errorDetail{{Field: "/sampling/rounds", Code: "above_maximum"}})
	})
}

// TestPolicyViolationsCarryCode proves the two renderings of one vocabulary:
// GET /pgo keeps the dotted field its clients already read,
// and gains the same code the refusal's details item carries.
func TestPolicyViolationsCarryCode(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{limits: testPGOLimits(func(l *config.PGOLimits) { l.MaxRounds = 2 })})
	rounds := 5
	h.seedOverride(t, &pgo.PolicyOverride{Sampling: &pgo.SamplingOverride{Rounds: &rounds}})

	got := h.doPGO(t, http.MethodGet, pgoPath, "", nil)

	body := decodePolicy(t, got.Body.Bytes())
	if len(body.Violations) != 1 {
		t.Fatalf("violations = %+v, want one", body.Violations)
	}
	if body.Violations[0].Field != "sampling.rounds" {
		t.Errorf("field = %q, want the dotted form GET /pgo publishes", body.Violations[0].Field)
	}
	if body.Violations[0].Code != "above_maximum" {
		t.Errorf("code = %q, want above_maximum", body.Violations[0].Code)
	}
	if !bytes.Contains(got.Body.Bytes(), []byte(`"code":"above_maximum"`)) {
		t.Errorf("body %q does not publish the violation code", got.Body.String())
	}
}
