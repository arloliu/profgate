package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/pgo"
)

// The two values the policy response's source carries.
const (
	sourceOverride = "override"
	sourceDefaults = "defaults"
)

// ifMatchRE is the only If-Match form the policy route accepts:
// the quoted decimal revision a previous response's ETag carried.
// Anything else, `*` included, is a parameter the gateway will not guess at.
var ifMatchRE = regexp.MustCompile(`^"[0-9]+"$`)

// The errors the policy route answers with.
var (
	errPreconditionFailed = &requestError{
		status:  http.StatusPreconditionFailed,
		code:    "precondition_failed",
		message: "the policy has moved since the revision If-Match names",
	}
	errPreconditionRequired = &requestError{
		status:  http.StatusPreconditionRequired,
		code:    "precondition_required",
		message: "the service already has a policy override; send If-Match with its ETag",
	}
	errOverrideNotFound = &requestError{
		status:  http.StatusNotFound,
		code:    "pgo_override_not_found",
		message: "the service has no policy override",
	}
	errConfigAPIDisabled = &requestError{
		status:  http.StatusForbidden,
		code:    "config_api_disabled",
		message: "the policy configuration api is disabled",
	}
)

// policyBody is what the policy route answers with:
// what is stored, what it means once layered on the operator's defaults, and
// which of its fields a current ceiling would now refuse.
type policyBody struct {
	Namespace  string              `json:"namespace"`
	Service    string              `json:"service"`
	Source     string              `json:"source"`
	Override   *pgo.PolicyOverride `json:"override"`
	Effective  pgo.Policy          `json:"effective"`
	Violations []pgo.Violation     `json:"violations"`
	UpdatedBy  string              `json:"updatedBy,omitempty"`
	UpdatedAt  *time.Time          `json:"updatedAt,omitempty"`
}

// servePolicyRead answers the Service's stored override, what it comes to, and
// the ceilings it no longer fits.
// The read is the authoritative one, because its revision is the ETag the
// client's next write is conditional on.
func (s *server) servePolicyRead(w http.ResponseWriter, r *http.Request, q *request, sess *pgo.Session) {
	rt := q.route
	stored, revision, err := sess.ReadOverride(r.Context(), rt.namespace, rt.service)
	if errors.Is(err, natskv.ErrKeyNotFound) {
		effective, violations := sess.Effective()
		q.audit.status = http.StatusOK
		q.audit.code = codeOK
		writeJSON(w, http.StatusOK, policyBody{
			Namespace:  rt.namespace,
			Service:    rt.service,
			Source:     sourceDefaults,
			Effective:  effective,
			Violations: normalizeViolations(violations),
		})

		return
	}
	if err != nil {
		q.fail(w, errPGOUnavailable)

		return
	}

	effective, violations := sess.Effective(stored.Policy)
	w.Header().Set("ETag", etagOf(revision))
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	writeJSON(w, http.StatusOK, storedPolicyBody(rt.namespace, rt.service, stored, effective, violations))
}

// servePolicyWrite creates or replaces the Service's override.
// Which of the two it is, and whether the client may do it at all, is decided
// by the key's presence and the revision If-Match names; the conditional write
// itself is what settles a race.
func (s *server) servePolicyWrite(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, sess *pgo.Session, principal string,
) {
	if cfg.PGO.ConfigAPI == configAPIDisabled {
		q.fail(w, errConfigAPIDisabled)

		return
	}

	expected, present, perr := parseIfMatch(r.Header.Get("If-Match"))
	if perr != nil {
		q.fail(w, perr)

		return
	}

	var override pgo.PolicyOverride
	if berr := decodeBody(w, r, &override, false); berr != nil {
		q.fail(w, berr)

		return
	}

	effective, violations := sess.Effective(&override)
	if len(violations) > 0 {
		q.fail(w, limitExceeded(violations))

		return
	}

	rt := q.route
	stored := pgo.StoredOverride{Policy: &override, UpdatedBy: principal, UpdatedAt: sess.Now()}
	_, revision, err := sess.ReadOverride(r.Context(), rt.namespace, rt.service)

	switch {
	case errors.Is(err, natskv.ErrKeyNotFound):
		if present {
			// If-Match against a key that is not there: the client is acting on
			// a revision that no longer exists, so it re-reads and decides again.
			q.fail(w, errPreconditionFailed)

			return
		}
		revision, err = sess.CreateOverride(r.Context(), rt.namespace, rt.service, stored)
		if errors.Is(err, natskv.ErrKeyExists) {
			q.fail(w, errPreconditionFailed)

			return
		}
		if err != nil {
			q.fail(w, errPGOUnavailable)

			return
		}
		s.writePolicy(w, q, http.StatusCreated, revision, rt, stored, effective, violations)
	case err != nil:
		q.fail(w, errPGOUnavailable)
	case !present:
		// The Service already has an override, so a write without a condition
		// would silently replace whatever someone else last stored.
		q.fail(w, errPreconditionRequired)
	case expected != revision:
		q.fail(w, errPreconditionFailed)
	default:
		revision, err = sess.UpdateOverride(r.Context(), rt.namespace, rt.service, stored, expected)
		if errors.Is(err, natskv.ErrRevisionMismatch) {
			q.fail(w, errPreconditionFailed)

			return
		}
		if err != nil {
			q.fail(w, errPGOUnavailable)

			return
		}
		s.writePolicy(w, q, http.StatusOK, revision, rt, stored, effective, violations)
	}
}

// servePolicyDelete removes the Service's override, returning it to the
// operator's defaults and stopping its scheduling.
// The handler reads the key for its revision and for the absent-or-present
// distinction, then deletes at that revision:
// a key that moved in between is a lost condition, the same as a stale
// If-Match, and the client re-reads and decides again.
func (s *server) servePolicyDelete(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, sess *pgo.Session,
) {
	if cfg.PGO.ConfigAPI == configAPIDisabled {
		q.fail(w, errConfigAPIDisabled)

		return
	}

	if berr := rejectBody(w, r); berr != nil {
		q.fail(w, berr)

		return
	}

	expected, present, perr := parseIfMatch(r.Header.Get("If-Match"))
	if perr != nil {
		q.fail(w, perr)

		return
	}

	rt := q.route
	_, revision, err := sess.ReadOverride(r.Context(), rt.namespace, rt.service)
	switch {
	case errors.Is(err, natskv.ErrKeyNotFound):
		q.fail(w, errOverrideNotFound)

		return
	case err != nil:
		q.fail(w, errPGOUnavailable)

		return
	case !present:
		q.fail(w, errPreconditionRequired)

		return
	case expected != revision:
		q.fail(w, errPreconditionFailed)

		return
	}

	switch err := sess.DeleteOverride(r.Context(), rt.namespace, rt.service, expected); {
	case err == nil:
	case errors.Is(err, natskv.ErrRevisionMismatch), errors.Is(err, natskv.ErrKeyNotFound):
		q.fail(w, errPreconditionFailed)

		return
	default:
		q.fail(w, errPGOUnavailable)

		return
	}

	q.audit.status = http.StatusNoContent
	q.audit.code = codeOK
	w.WriteHeader(http.StatusNoContent)
}

// writePolicy answers a successful policy write with the ETag of the revision
// it committed and the same body a read would give.
func (s *server) writePolicy(
	w http.ResponseWriter, q *request, status int, revision uint64,
	rt route, stored pgo.StoredOverride, effective pgo.Policy, violations []pgo.Violation,
) {
	w.Header().Set("ETag", etagOf(revision))
	q.audit.status = status
	q.audit.code = codeOK
	writeJSON(w, status, storedPolicyBody(rt.namespace, rt.service, stored, effective, violations))
}

// storedPolicyBody is the response for a Service that has an override.
func storedPolicyBody(
	namespace, service string, stored pgo.StoredOverride, effective pgo.Policy, violations []pgo.Violation,
) policyBody {
	updatedAt := stored.UpdatedAt

	return policyBody{
		Namespace:  namespace,
		Service:    service,
		Source:     sourceOverride,
		Override:   stored.Policy,
		Effective:  effective,
		Violations: normalizeViolations(violations),
		UpdatedBy:  stored.UpdatedBy,
		UpdatedAt:  &updatedAt,
	}
}

// normalizeViolations writes an empty list as `[]` rather than `null`,
// so a client can read the field without a nil check.
func normalizeViolations(violations []pgo.Violation) []pgo.Violation {
	if violations == nil {
		return []pgo.Violation{}
	}

	return violations
}

// etagOf is the ETag of one KV revision: a quoted decimal, the only form
// If-Match is accepted in.
func etagOf(revision uint64) string { return `"` + strconv.FormatUint(revision, 10) + `"` }

// parseIfMatch reads the header's revision.
// Absent is no condition; any form but a quoted decimal, `*` included, is a
// parameter the gateway refuses rather than reads as "whatever is there".
func parseIfMatch(header string) (revision uint64, present bool, err *requestError) {
	if header == "" {
		return 0, false, nil
	}
	if !ifMatchRE.MatchString(header) {
		return 0, false, invalidParameter("If-Match must be a quoted decimal revision, as the ETag carries it")
	}
	revision, parseErr := strconv.ParseUint(strings.Trim(header, `"`), 10, 64)
	if parseErr != nil {
		return 0, false, invalidParameter("If-Match must be a quoted decimal revision, as the ETag carries it")
	}

	return revision, true, nil
}

// limitExceeded is the refusal of a policy that a current ceiling would not
// hold, naming the fields so the caller knows what to lower.
func limitExceeded(violations []pgo.Violation) *requestError {
	fields := make([]string, 0, len(violations))
	for _, v := range violations {
		fields = append(fields, v.Field+" ("+v.Detail+")")
	}

	return &requestError{
		status:  http.StatusBadRequest,
		code:    "limit_exceeded",
		message: "the effective policy exceeds a limit: " + strings.Join(fields, ", "),
	}
}
