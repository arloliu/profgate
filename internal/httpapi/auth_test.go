package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/pgo"
)

const (
	authFailureBody = `{"error":"authentication required","code":"unauthenticated"}` + "\n"
	sessionCookie   = "__Host-profgate_session"
)

// fakeAuth answers every request with one programmed outcome and remembers
// the configuration pointer it was handed.
type fakeAuth struct {
	mu        sync.Mutex
	principal auth.Principal
	err       error
	calls     int
	cfgs      []*config.Config
	// onCall runs inside Authenticate, before it answers.
	onCall func()
}

func admitAs(name, realm string) *fakeAuth {
	return &fakeAuth{principal: auth.Principal{Name: name, Realm: realm}}
}

func refuse(f *auth.Failure) *fakeAuth { return &fakeAuth{err: f} }

func (f *fakeAuth) Authenticate(_ context.Context, _ *http.Request, cfg *config.Config) (auth.Principal, error) {
	f.mu.Lock()
	f.calls++
	f.cfgs = append(f.cfgs, cfg)
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall()
	}

	return f.principal, f.err
}

func (f *fakeAuth) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// fakeRoutes writes a programmed outcome the way the real routes do: the
// whole response, then the outcome for the audit line.
type fakeRoutes struct {
	mu      sync.Mutex
	outcome auth.RouteOutcome
	calls   int
	cfgs    []*config.Config
}

func (f *fakeRoutes) ServeAuth(w http.ResponseWriter, _ *http.Request, cfg *config.Config) auth.RouteOutcome {
	f.mu.Lock()
	f.calls++
	f.cfgs = append(f.cfgs, cfg)
	f.mu.Unlock()
	switch f.outcome.Status {
	case http.StatusFound:
		w.Header().Set("Location", "https://issuer.example/authorize")
		w.WriteHeader(http.StatusFound)
	case http.StatusOK:
		w.WriteHeader(http.StatusOK)
	default:
		WriteError(w, f.outcome.Status, f.outcome.Code, "authentication required")
	}

	return f.outcome
}

// authHarness is the harness under auth.mode basic with a programmed
// authenticator; every row builds its own.
func authHarness(mode string, a *fakeAuth) *harness {
	h := newHarness(baseTarget())
	h.configure(func(cfg *config.Config) {
		cfg.Auth.Mode = mode
		cfg.Auth.AnonymousRealm = ""
	})
	h.auth = a

	return h
}

// bearerRequest is a targets request carrying a valid-looking bearer header.
func (h *harness) doBearer(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer token")
	h.handler().ServeHTTP(rec, req)
	assertNoLeak(t, rec)

	return rec
}

// expectAuthFailure checks the fixed 401 shape: the envelope, the challenge,
// and one audit record naming the reason with principal "-".
func (h *harness) expectAuthFailure(t *testing.T, rec *httptest.ResponseRecorder, challenge, reason string) map[string]any {
	t.Helper()

	h.expectError(t, rec, http.StatusUnauthorized, "unauthenticated")
	if got := rec.Body.String(); got != authFailureBody {
		t.Errorf("body = %q, want %q", got, authFailureBody)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != challenge {
		t.Errorf("WWW-Authenticate = %q, want %q", got, challenge)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	return h.expectAuthAudit(t, http.StatusUnauthorized, "unauthenticated", reason)
}

// expectAuthAudit checks the one audit record for principal "-" and a reason
// from the closed set.
func (h *harness) expectAuthAudit(t *testing.T, status int, code, reason string) map[string]any {
	t.Helper()

	audit := h.expectAudit(t, status, code)
	if got := audit["principal"]; got != "-" {
		t.Errorf("audit principal = %v, want -", got)
	}
	expectReason(t, audit, reason)

	return audit
}

// expectReason checks auth_reason on an audit record and that the value is one
// the authentication package can emit.
func expectReason(t *testing.T, audit map[string]any, reason string) {
	t.Helper()

	got, ok := audit["auth_reason"]
	if reason == "" {
		if ok {
			t.Errorf("audit carries auth_reason %v on a record that should not", got)
		}

		return
	}
	if got != reason {
		t.Errorf("audit auth_reason = %v, want %q", got, reason)
	}
	if !slices.Contains(auth.Reasons(), reason) {
		t.Errorf("auth_reason %q is not one of auth.Reasons()", reason)
	}
}

func (h *harness) expectAuthFailureMetric(t *testing.T, want ...authFailureCall) {
	t.Helper()

	if got := h.rec.authFailureRows(); !slices.Equal(got, want) {
		t.Errorf("AuthFailure calls = %v, want %v", got, want)
	}
}

func TestAuthOrder(t *testing.T) {
	t.Run("composed order: route", func(t *testing.T) {
		a := refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing})
		h := authHarness("basic", a)
		rec := h.do(t, http.MethodGet, "/v1/bogus")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
		if a.called() != 0 {
			t.Error("Authenticate called before the route step")
		}
	})

	t.Run("composed order: method", func(t *testing.T) {
		a := refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing})
		h := authHarness("basic", a)
		rec := h.do(t, http.MethodPost, targetsPath)
		h.expectError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if a.called() != 0 {
			t.Error("Authenticate called before the method step")
		}
	})

	t.Run("composed order: not ready", func(t *testing.T) {
		a := refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing})
		h := authHarness("basic", a)
		h.ready = func() bool { return false }
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")
		if a.called() != 0 {
			t.Error("Authenticate called before the readiness step")
		}
	})

	t.Run("readiness is the closure", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.disc.synced = true
		h.ready = func() bool { return false }
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")
	})

	t.Run("readiness default", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.disc.synced = false
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")

		h = newHarness(baseTarget())
		h.disc.synced = true
		if rec := h.do(t, http.MethodGet, targetsPath); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("composed order: pgo disabled", func(t *testing.T) {
		a := refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing})
		h := authHarness("basic", a)
		rec := h.do(t, http.MethodGet, "/v1/namespaces/payment/services/payment-api/pgo")
		h.expectPGOError(t, rec, http.StatusNotImplemented, "pgo_disabled", "pgo_disabled")
		if a.called() != 0 {
			t.Error("Authenticate called before the PGO step")
		}
	})
}

func TestAccessTokenQuery(t *testing.T) {
	rows := []struct {
		name   string
		target string
	}{
		{"access_token first", targetsPath + "?access_token=x"},
		{"access_token on the profile route", profilePath + "heap?access_token="},
		{"access_token empty value", targetsPath + "?access_token"},
		{"access_token malformed value", targetsPath + "?access_token=%ZZ"},
		{"access_token after semicolon", targetsPath + "?a=1;access_token=x"},
		{"access_token encoded key", targetsPath + "?%61ccess_token=x"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			a := admitAs("alice", "developer")
			h := authHarness("oidc", a)
			rec := h.doBearer(t, tc.target)
			h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
			if a.called() != 0 {
				t.Error("Authenticate called with access_token in the query")
			}
		})
	}

	t.Run("access_token on the collection route", func(t *testing.T) {
		a := admitAs("alice", "developer")
		p := newPGOHarness(t, pgoOpts{})
		p.configure(func(cfg *config.Config) { cfg.Auth.Mode = "oidc" })
		p.auth = a
		rec := p.doBearer(t, "/v1/collections/7h2k9m4p6r8t0v1w3x5y?access_token=x")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
		if code, _ := errorBodyOf(t, rec); code != "invalid_parameter" {
			t.Errorf("code = %q, want invalid_parameter", code)
		}
		if a.called() != 0 {
			t.Error("Authenticate called with access_token in the query")
		}
	})

	t.Run("access_token key case", func(t *testing.T) {
		a := admitAs("alice", "developer")
		h := authHarness("oidc", a)
		rec := h.doBearer(t, targetsPath+"?Access_Token=x")
		h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if a.called() != 1 {
			t.Errorf("Authenticate calls = %d, want 1: the key comparison is case-sensitive", a.called())
		}
	})
}

func TestHasAccessToken(t *testing.T) {
	rows := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"seconds=5", false},
		{"access_token=x", true},
		{"access_token", true},
		{"access_token=", true},
		{"access_token=%ZZ", true},
		{"a=1;access_token=x", true},
		{"a=1&access_token=x", true},
		{"%61ccess_token=x", true},
		{"%ZZaccess_token=x", false},
		{"Access_Token=x", false},
		{"xaccess_token=x", false},
		{"access_token_x=x", false},
		{"a=access_token", false},
	}
	for _, tc := range rows {
		t.Run(tc.query, func(t *testing.T) {
			if got := hasAccessToken(tc.query); got != tc.want {
				t.Errorf("hasAccessToken(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestAuthFailures(t *testing.T) {
	t.Run("401 shape", func(t *testing.T) {
		h := authHarness("basic", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonBadCredential}))
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectAuthFailure(t, rec, `Basic realm="profgate"`, auth.ReasonBadCredential)
		h.expectAuthFailureMetric(t, authFailureCall{"basic", auth.ReasonBadCredential})
		h.expectMetric(t, metrics.EndpointTargets, "none")
		h.expectCounts(t, 0, 0)
	})

	t.Run("401 bearer challenge", func(t *testing.T) {
		h := authHarness("oidc", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonSignature}))
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectAuthFailure(t, rec, `Bearer realm="profgate"`, auth.ReasonSignature)
		h.expectAuthFailureMetric(t, authFailureCall{"oidc", auth.ReasonSignature})
	})

	t.Run("uniformity", func(t *testing.T) {
		other := map[string]int{
			auth.ReasonThrottled: 429,
			auth.ReasonExchange:  503, auth.ReasonKeysStale: 503, auth.ReasonEntropy: 503, auth.ReasonInternal: 503,
		}
		var first *httptest.ResponseRecorder
		for _, reason := range auth.Reasons() {
			if other[reason] != 0 {
				continue
			}
			h := authHarness("oidc", refuse(&auth.Failure{Status: 401, Reason: reason}))
			rec := h.do(t, http.MethodGet, targetsPath)
			h.expectAuthFailure(t, rec, `Bearer realm="profgate"`, reason)
			if first == nil {
				first = rec

				continue
			}
			if rec.Code != first.Code || rec.Body.String() != first.Body.String() {
				t.Errorf("reason %s: response differs from the first: %d %q", reason, rec.Code, rec.Body.String())
			}
			for name := range first.Header() {
				// The identifier names the request rather than the failure,
				// and differs on every response by design.
				if name == requestIDHeader {
					continue
				}
				if got, want := rec.Header().Values(name), first.Header().Values(name); !slices.Equal(got, want) {
					t.Errorf("reason %s: header %s = %v, want %v", reason, name, got, want)
				}
			}
		}
	})

	t.Run("denied namespace is 401", func(t *testing.T) {
		h := authHarness("basic", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing}))
		h.configure(func(cfg *config.Config) {
			cfg.Realms["developer"] = config.Realm{Namespaces: []string{"billing"}, Services: []string{"*"}, Profiles: []string{"*"}}
		})
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectAuthFailure(t, rec, `Basic realm="profgate"`, auth.ReasonMissing)
	})

	t.Run("429", func(t *testing.T) {
		h := authHarness("basic", refuse(&auth.Failure{Status: 429, Reason: auth.ReasonThrottled}))
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusTooManyRequests, "too_many_auth")
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want 1", got)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("WWW-Authenticate = %q, want none", got)
		}
		h.expectAuthAudit(t, http.StatusTooManyRequests, "too_many_auth", auth.ReasonThrottled)
		h.expectAuthFailureMetric(t, authFailureCall{"basic", auth.ReasonThrottled})
	})

	t.Run("503", func(t *testing.T) {
		h := authHarness("oidc", refuse(&auth.Failure{Status: 503, Reason: auth.ReasonKeysStale}))
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "auth_unavailable")
		if got := rec.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("WWW-Authenticate = %q, want none", got)
		}
		h.expectAuthAudit(t, http.StatusServiceUnavailable, "auth_unavailable", auth.ReasonKeysStale)
		h.expectAuthFailureMetric(t, authFailureCall{"oidc", auth.ReasonKeysStale})
	})

	t.Run("redirect", func(t *testing.T) {
		const location = "/auth/login?return=%2Fv1%2Fx"
		h := authHarness("oidc", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing, Redirect: location}))
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want 302", rec.Code)
		}
		if got := rec.Header().Get("Location"); got != location {
			t.Errorf("Location = %q, want %q", got, location)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}
		h.expectAuthAudit(t, http.StatusFound, "auth_redirect", auth.ReasonMissing)
		h.expectMetricCode(t, "auth_redirect")
		h.expectAuthFailureMetric(t)
	})

	t.Run("clear session", func(t *testing.T) {
		h := authHarness("oidc", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonSession, ClearSession: true}))
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectAuthFailure(t, rec, `Bearer realm="profgate"`, auth.ReasonSession)
		expectSessionDeleted(t, rec)
	})

	t.Run("clear session on redirect", func(t *testing.T) {
		h := authHarness("oidc", refuse(&auth.Failure{
			Status: 401, Reason: auth.ReasonSession, ClearSession: true, Redirect: "/auth/login?return=%2Fv1%2Fx",
		}))
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want 302", rec.Code)
		}
		expectSessionDeleted(t, rec)
		h.expectAuthAudit(t, http.StatusFound, "auth_redirect", auth.ReasonSession)
	})

	t.Run("no realm", func(t *testing.T) {
		h := authHarness("basic", admitAs("alice", "nobody"))
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectAuthFailure(t, rec, `Basic realm="profgate"`, auth.ReasonNoRealm)
		h.expectAuthFailureMetric(t, authFailureCall{"basic", auth.ReasonNoRealm})
	})

	t.Run("admitted", func(t *testing.T) {
		h := authHarness("basic", admitAs("alice", "developer"))
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		audit := h.expectAudit(t, http.StatusOK, "ok")
		if got := audit["principal"]; got != "alice" {
			t.Errorf("audit principal = %v, want alice", got)
		}
		expectReason(t, audit, "")
		h.expectAuthFailureMetric(t)
	})

	t.Run("realm still applies", func(t *testing.T) {
		h := authHarness("basic", admitAs("alice", "narrow"))
		h.configure(func(cfg *config.Config) {
			cfg.Realms["narrow"] = config.Realm{Namespaces: []string{"billing"}, Services: []string{"*"}, Profiles: []string{"*"}}
		})
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusForbidden, "realm_denied")
		h.expectAuthFailureMetric(t)
	})

	t.Run("collection routes", func(t *testing.T) {
		p := newPGOHarness(t, pgoOpts{})
		stored := p.seedRecord(t, p.newRecord(pgo.StateCompleted))
		p.configure(func(cfg *config.Config) { cfg.Auth.Mode = "oidc" })
		p.auth = refuse(&auth.Failure{Status: 401, Reason: auth.ReasonExpired})
		rec := p.do(t, http.MethodGet, "/v1/collections/"+stored.ID)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		audit := p.expectPGOAudit(t, http.StatusUnauthorized, "unauthenticated")
		expectReason(t, audit, auth.ReasonExpired)
		if got := audit["namespace"]; got != "" {
			t.Errorf("audit namespace = %v: the record was read before authentication", got)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		a := admitAs("alice", "developer")
		h := authHarness("basic", a)
		entry := h.cfg.Load()
		a.onCall = func() {
			swapped := *entry
			swapped.Realms = map[string]config.Realm{}
			h.cfg.Store(&swapped)
		}
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: the realm step must read the snapshot the request loaded", rec.Code)
		}
		if len(a.cfgs) != 1 || a.cfgs[0] != entry {
			t.Errorf("Authenticate received %v, want the entry snapshot %p", a.cfgs, entry)
		}
	})

	t.Run("non-Failure error", func(t *testing.T) {
		h := authHarness("oidc", &fakeAuth{err: errors.New("boom")})
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "auth_unavailable")
		if got := rec.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		h.expectAuthAudit(t, http.StatusServiceUnavailable, "auth_unavailable", auth.ReasonInternal)
		h.expectAuthFailureMetric(t, authFailureCall{"oidc", auth.ReasonInternal})
		if logs := h.logs.String(); !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, "boom") {
			t.Errorf("no error log line carrying the error: %s", logs)
		}
	})

	t.Run("disabled default", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		audit := h.expectAudit(t, http.StatusOK, "ok")
		if got := audit["principal"]; got != "anonymous" {
			t.Errorf("audit principal = %v, want anonymous", got)
		}
	})
}

// expectSessionDeleted checks for a Set-Cookie that deletes the session cookie.
func expectSessionDeleted(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if c.Value != "" || c.MaxAge != -1 {
				t.Errorf("session cookie = %q max-age %d, want a deletion", c.Value, c.MaxAge)
			}

			return
		}
	}
	t.Errorf("no Set-Cookie deleting %s: %v", sessionCookie, rec.Header().Values("Set-Cookie"))
}

// structTagRE matches a struct tag key:"value" pair.
var structTagRE = regexp.MustCompile(`\w+:"[^"]*"`)

// paramNameRE matches the declaration of a query parameter's name.
// A route's parameter is named by the client and answered with a 400,
// which is a different vocabulary from the audit reasons this test closes.
var paramNameRE = regexp.MustCompile(`(?m)^\s*\w+Param\s+=\s+"[^"]*"`)

// TestReasonsClosedSet checks that this package names audit reasons only
// through the constants of internal/auth, so no reason can exist that the
// closed set does not hold.
func TestReasonsClosedSet(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		// A struct tag such as json:"state" is not a reason,
		// and neither is the name of a query parameter such as stateParam.
		src := structTagRE.ReplaceAllString(string(raw), "")
		src = paramNameRE.ReplaceAllString(src, "")
		for _, reason := range auth.Reasons() {
			if strings.Contains(src, `"`+reason+`"`) {
				t.Errorf("%s names the reason %q as a literal; use auth.Reason* constants", name, reason)
			}
		}
	}
}

// TestEnvelopeMatchesAuth pins WriteError to the bytes internal/auth writes
// for its own errors, so the two envelopes stay identical.
func TestEnvelopeMatchesAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnauthorized, "unauthenticated", "authentication required")
	if got := rec.Body.String(); got != authFailureBody {
		t.Errorf("body = %q, want %q", got, authFailureBody)
	}
}

func TestAuthRoutes(t *testing.T) {
	routed := func(outcome auth.RouteOutcome) (*harness, *fakeRoutes) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.Auth.Mode = "oidc"
			cfg.Auth.AnonymousRealm = ""
		})
		routes := &fakeRoutes{outcome: outcome}
		h.routes = routes

		return h, routes
	}

	expectRouteAudit := func(t *testing.T, h *harness, route, principal string, status int, code, reason string) {
		t.Helper()

		records := h.audits(t)
		if len(records) != 1 {
			t.Fatalf("audit records = %d, want 1: %s", len(records), h.logs.String())
		}
		audit := records[0]
		if got := audit["route"]; got != route {
			t.Errorf("audit route = %v, want %q", got, route)
		}
		if got := audit["principal"]; got != principal {
			t.Errorf("audit principal = %v, want %q", got, principal)
		}
		if got, _ := audit["status"].(float64); int(got) != status {
			t.Errorf("audit status = %v, want %d", audit["status"], status)
		}
		if got := audit["code"]; got != code {
			t.Errorf("audit code = %v, want %q", got, code)
		}
		for _, key := range []string{"namespace", "service", "pod", "collection"} {
			if _, ok := audit[key]; ok {
				t.Errorf("an /auth/ audit record carries %q: %v", key, audit)
			}
		}
		expectReason(t, audit, reason)
	}

	t.Run("auth routes absent", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, "/auth/login")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
	})

	t.Run("auth routes unknown path", func(t *testing.T) {
		h, routes := routed(auth.RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"})
		rec := h.do(t, http.MethodGet, "/auth/bogus")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
		if routes.calls != 0 {
			t.Error("ServeAuth called for an unknown path")
		}
	})

	t.Run("auth routes method", func(t *testing.T) {
		h, routes := routed(auth.RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"})
		rec := h.do(t, http.MethodPost, "/auth/login")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET" {
			t.Errorf("Allow = %q, want GET", got)
		}
		expectRouteAudit(t, h, "auth_login", "-", http.StatusMethodNotAllowed, "method_not_allowed", "")
		h.expectMetricCode(t, "method_not_allowed")
		h.expectMetric(t, metrics.EndpointAuth, "none")
		if routes.calls != 0 {
			t.Error("ServeAuth called for a POST")
		}
	})

	t.Run("auth routes readiness", func(t *testing.T) {
		h, routes := routed(auth.RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"})
		h.disc.synced = true
		h.ready = func() bool { return false }
		rec := h.do(t, http.MethodGet, "/auth/callback?code=x")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if code, _ := errorBodyOf(t, rec); code != "not_ready" {
			t.Errorf("code = %q, want not_ready", code)
		}
		expectRouteAudit(t, h, "auth_callback", "-", http.StatusServiceUnavailable, "not_ready", "")
		if routes.calls != 0 {
			t.Error("ServeAuth called before readiness")
		}
	})

	t.Run("auth routes dispatch", func(t *testing.T) {
		h, routes := routed(auth.RouteOutcome{Status: 200, Code: "ok", Principal: "alice"})
		rec := h.do(t, http.MethodGet, "/auth/callback?code=x")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if routes.calls != 1 || routes.cfgs[0] != h.cfg.Load() {
			t.Errorf("ServeAuth calls = %d with %v, want one call with the snapshot", routes.calls, routes.cfgs)
		}
		expectRouteAudit(t, h, "auth_callback", "alice", http.StatusOK, "ok", "")
		h.expectMetricCode(t, "ok")
		h.expectMetric(t, metrics.EndpointAuth, "none")
		h.expectAuthFailureMetric(t)
	})

	t.Run("auth routes login", func(t *testing.T) {
		h, _ := routed(auth.RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"})
		h.do(t, http.MethodGet, "/auth/login")
		expectRouteAudit(t, h, "auth_login", "-", http.StatusFound, "auth_redirect", "")
		h.expectAuthFailureMetric(t)
	})

	t.Run("auth routes logout", func(t *testing.T) {
		h, _ := routed(auth.RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"})
		h.do(t, http.MethodGet, "/auth/logout")
		expectRouteAudit(t, h, "auth_logout", "-", http.StatusFound, "auth_redirect", "")
	})

	t.Run("auth routes failure", func(t *testing.T) {
		h, _ := routed(auth.RouteOutcome{Status: 401, Code: "unauthenticated", Reason: auth.ReasonState, Principal: "-"})
		rec := h.do(t, http.MethodGet, "/auth/callback?code=x")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		expectRouteAudit(t, h, "auth_callback", "-", http.StatusUnauthorized, "unauthenticated", auth.ReasonState)
		h.expectAuthFailureMetric(t, authFailureCall{"oidc", auth.ReasonState})
	})

	t.Run("auth routes 503", func(t *testing.T) {
		h, _ := routed(auth.RouteOutcome{Status: 503, Code: "auth_unavailable", Reason: auth.ReasonEntropy, Principal: "-"})
		h.do(t, http.MethodGet, "/auth/login")
		expectRouteAudit(t, h, "auth_login", "-", http.StatusServiceUnavailable, "auth_unavailable", auth.ReasonEntropy)
		h.expectAuthFailureMetric(t, authFailureCall{"oidc", auth.ReasonEntropy})
		h.expectMetricCode(t, "auth_unavailable")
		h.expectMetric(t, metrics.EndpointAuth, "none")
	})

	t.Run("auth routes not under /v1", func(t *testing.T) {
		h, routes := routed(auth.RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"})
		rec := h.do(t, http.MethodGet, "/v1/auth/login")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
		if routes.calls != 0 {
			t.Error("ServeAuth called under /v1")
		}
	})
}
