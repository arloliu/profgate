package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
)

// The four listing routes.
const (
	namespacesPath = "/v1/namespaces"
	servicesPath   = "/v1/namespaces/payments/services"
	whoamiPath     = "/v1/whoami"
	limitsPath     = "/v1/limits"
)

// listingPaths returns every listing route, with the namespace the Service list is asked for.
func listingPaths() map[string]string {
	return map[string]string{
		"namespaces": namespacesPath,
		"services":   servicesPath,
		"whoami":     whoamiPath,
		"limits":     limitsPath,
	}
}

// listingCatalog is what the fake Service cache holds unless a row says otherwise:
// two Services in payments, two in orders, one in staging, every one with a selector.
func listingCatalog() []k8s.ServiceRef {
	return []k8s.ServiceRef{
		{Namespace: "orders", Name: "api"},
		{Namespace: "orders", Name: "legacy"},
		{Namespace: "payments", Name: "checkout"},
		{Namespace: "payments", Name: "ledger"},
		{Namespace: "staging", Name: "only"},
	}
}

// listingHarness is a harness whose fake holds listingCatalog and a Target carrying an address,
// so a listing response has something to leak.
func listingHarness() *harness {
	h := newHarness(baseTarget())
	h.disc.catalog = listingCatalog()

	return h
}

// realmLists narrows the developer realm's namespaces and services lists.
func (h *harness) realmLists(namespaces, services []string) {
	h.configure(func(cfg *config.Config) {
		realm := cfg.Realms["developer"]
		realm.Namespaces = namespaces
		realm.Services = services
		cfg.Realms["developer"] = realm
	})
}

// doWith is do with request headers.
func (h *harness) doWith(t *testing.T, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	h.handler().ServeHTTP(rec, req)
	assertNoLeak(t, rec)

	return rec
}

// expectJSON checks a 200 with the JSON headers whose body equals want as a JSON value.
func expectJSON(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var got, expected any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatalf("want %q is not JSON: %v", want, err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("body = %s, want %s", rec.Body.String(), want)
	}
}

// expectNoCORS fails when any response header is a CORS header.
func expectNoCORS(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	for name := range rec.Header() {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("response carries %s", name)
		}
	}
}

func TestListingRoutes(t *testing.T) {
	t.Run("route table", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				h := listingHarness()
				rec := h.do(t, http.MethodGet, path)
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
				}
				expectNoCORS(t, rec)
			})
		}
		for _, path := range []string{"/v1/namespaces/", "/v1/namespaces/x/services/", "/v1/whoami/x", "/v1/limit"} {
			t.Run(path, func(t *testing.T) {
				h := listingHarness()
				rec := h.do(t, http.MethodGet, path)
				h.expectError(t, rec, http.StatusNotFound, "route_unknown")
			})
		}
	})

	t.Run("namespace label", func(t *testing.T) {
		for _, ns := range []string{"Bad_NS", strings.Repeat("a", 64)} {
			t.Run(ns, func(t *testing.T) {
				h := listingHarness()
				rec := h.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/services")
				h.expectError(t, rec, http.StatusNotFound, "route_unknown")
			})
		}
	})

	t.Run("method", func(t *testing.T) {
		for name, path := range listingPaths() {
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
				t.Run(method+" "+name, func(t *testing.T) {
					h := listingHarness()
					rec := h.do(t, method, path)
					h.expectError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
					if got := rec.Header().Get("Allow"); got != "GET" {
						t.Errorf("Allow = %q, want GET", got)
					}
				})
			}
		}
	})

	t.Run("not ready", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				h := listingHarness()
				h.disc.synced = false
				rec := h.do(t, http.MethodGet, path)
				h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")
				if got := h.disc.catalogCalls.Load(); got != 0 {
					t.Errorf("Catalog calls = %d, want 0", got)
				}
			})
		}
	})

	t.Run("readiness is the closure", func(t *testing.T) {
		h := listingHarness()
		h.ready = func() bool { return false }
		rec := h.do(t, http.MethodGet, namespacesPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")
	})

	t.Run("access_token", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				a := admitAs("alice", "developer")
				h := authHarness("basic", a)
				h.disc.catalog = listingCatalog()
				rec := h.do(t, http.MethodGet, path+"?access_token=x")
				h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
				if a.called() != 0 {
					t.Errorf("Authenticate called %d times, want 0", a.called())
				}
			})
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				h := authHarness("basic", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing}))
				h.disc.catalog = listingCatalog()
				rec := h.do(t, http.MethodGet, path)
				h.expectAuthFailure(t, rec, `Basic realm="profgate"`, auth.ReasonMissing)
				expectNoCORS(t, rec)
				if got := h.disc.catalogCalls.Load(); got != 0 {
					t.Errorf("Catalog calls = %d, want 0", got)
				}
			})
		}
	})

	t.Run("oidc fetch is a 401", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				h := authHarness("oidc", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing}))
				h.disc.catalog = listingCatalog()
				rec := h.doWith(t, http.MethodGet, path, map[string]string{
					"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin",
				})
				h.expectAuthFailure(t, rec, `Bearer realm="profgate"`, auth.ReasonMissing)
			})
		}
	})

	t.Run("oidc navigation is redirected", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				location := "/auth/login?return=" + path
				h := authHarness("oidc", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing, Redirect: location}))
				h.disc.catalog = listingCatalog()
				rec := h.doWith(t, http.MethodGet, path, map[string]string{
					"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
				})
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
				if got := h.disc.catalogCalls.Load(); got != 0 {
					t.Errorf("Catalog calls = %d, want 0", got)
				}
			})
		}
	})

	t.Run("basic challenge", func(t *testing.T) {
		h := authHarness("basic", refuse(&auth.Failure{Status: 401, Reason: auth.ReasonMissing}))
		rec := h.do(t, http.MethodGet, whoamiPath)
		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="profgate"` {
			t.Errorf("WWW-Authenticate = %q", got)
		}
	})

	t.Run("unknown query", func(t *testing.T) {
		for name, path := range listingPaths() {
			for _, query := range []string{"?x=1", "?port=6060"} {
				t.Run(name+query, func(t *testing.T) {
					h := listingHarness()
					rec := h.do(t, http.MethodGet, path+query)
					h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
				})
			}
			t.Run(name+" bare ?", func(t *testing.T) {
				h := listingHarness()
				rec := h.do(t, http.MethodGet, path+"?")
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
				}
			})
		}
	})

	t.Run("query after realm", func(t *testing.T) {
		h := listingHarness()
		h.realmLists([]string{"payments"}, []string{"*"})
		rec := h.do(t, http.MethodGet, "/v1/namespaces/staging/services?x=1")
		h.expectError(t, rec, http.StatusForbidden, "realm_denied")
	})
}

func TestListingFilter(t *testing.T) {
	type expectation struct {
		path   string
		status int
		body   string // the JSON body on 200, the code otherwise
	}
	cases := []struct {
		name       string
		namespaces []string
		services   []string
		expect     []expectation
	}{
		{"both wildcards", []string{"*"}, []string{"*"}, []expectation{
			{namespacesPath, 200, `{"namespaces":["orders","payments","staging"]}`},
			{servicesPath, 200, `{"namespace":"payments","services":["checkout","ledger"]}`},
		}},
		{"named services", []string{"*"}, []string{"checkout", "api"}, []expectation{
			{namespacesPath, 200, `{"namespaces":["orders","payments"]}`},
			{servicesPath, 200, `{"namespace":"payments","services":["checkout"]}`},
			{"/v1/namespaces/staging/services", 200, `{"namespace":"staging","services":[]}`},
		}},
		{"named namespaces", []string{"payments"}, []string{"*"}, []expectation{
			{namespacesPath, 200, `{"namespaces":["payments"]}`},
			{servicesPath, 200, `{"namespace":"payments","services":["checkout","ledger"]}`},
			{"/v1/namespaces/orders/services", 403, "realm_denied"},
		}},
		{"both named", []string{"payments", "orders"}, []string{"ledger"}, []expectation{
			{namespacesPath, 200, `{"namespaces":["payments"]}`},
			{"/v1/namespaces/orders/services", 200, `{"namespace":"orders","services":[]}`},
		}},
		{"named namespace the cache lacks", []string{"payments", "missing"}, []string{"*"}, []expectation{
			{namespacesPath, 200, `{"namespaces":["payments"]}`},
			{"/v1/namespaces/missing/services", 200, `{"namespace":"missing","services":[]}`},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, e := range tc.expect {
				t.Run(e.path, func(t *testing.T) {
					h := listingHarness()
					h.realmLists(tc.namespaces, tc.services)
					rec := h.do(t, http.MethodGet, e.path)
					if e.status == http.StatusOK {
						expectJSON(t, rec, e.body)
						h.expectAudit(t, http.StatusOK, codeOK)
					} else {
						h.expectError(t, rec, e.status, e.body)
					}
				})
			}
		})
	}

	t.Run("denied namespace, present or absent", func(t *testing.T) {
		h := listingHarness()
		h.realmLists([]string{"payments"}, []string{"*"})
		present := h.do(t, http.MethodGet, "/v1/namespaces/orders/services")
		absent := h.do(t, http.MethodGet, "/v1/namespaces/nowhere/services")
		for _, rec := range []*httptest.ResponseRecorder{present, absent} {
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if code, _ := errorBodyOf(t, rec); code != "realm_denied" {
				t.Errorf("code = %q, want realm_denied", code)
			}
			expectNoCORS(t, rec)
		}
		if present.Body.String() != absent.Body.String() {
			t.Errorf("bodies differ: %q vs %q", present.Body.String(), absent.Body.String())
		}
		if got := h.disc.catalogCalls.Load(); got != 0 {
			t.Errorf("Catalog calls = %d, want 0", got)
		}
		calls, _, _ := h.rec.snapshot()
		if len(calls) != 2 {
			t.Fatalf("Recorder.Request calls = %d, want 2", len(calls))
		}
		for _, call := range calls {
			if call.endpoint != metrics.EndpointServices || call.profile != labelNone || call.code != "realm_denied" {
				t.Errorf("Recorder.Request = %+v, want (services, none, realm_denied)", call)
			}
		}
	})

	t.Run("catalog error", func(t *testing.T) {
		for _, path := range []string{namespacesPath, servicesPath} {
			t.Run(path, func(t *testing.T) {
				h := listingHarness()
				h.disc.catalogErr = errors.New("boom")
				rec := h.do(t, http.MethodGet, path)
				h.expectError(t, rec, http.StatusServiceUnavailable, "discovery_unavailable")
				if strings.Contains(rec.Body.String(), "boom") {
					t.Errorf("body names the cause: %q", rec.Body.String())
				}
			})
		}
	})

	t.Run("empty catalog", func(t *testing.T) {
		h := listingHarness()
		h.disc.catalog = []k8s.ServiceRef{}
		rec := h.do(t, http.MethodGet, namespacesPath)
		if got := rec.Body.String(); got != `{"namespaces":[]}`+"\n" {
			t.Errorf("body = %q, want %q", got, `{"namespaces":[]}`)
		}
		h = listingHarness()
		h.disc.catalog = []k8s.ServiceRef{}
		rec = h.do(t, http.MethodGet, servicesPath)
		if got := rec.Body.String(); got != `{"namespace":"payments","services":[]}`+"\n" {
			t.Errorf("body = %q", got)
		}
	})

	t.Run("sorted", func(t *testing.T) {
		unsorted := []k8s.ServiceRef{
			{Namespace: "payments", Name: "ledger"},
			{Namespace: "payments", Name: "checkout"},
			{Namespace: "orders", Name: "api"},
		}
		h := listingHarness()
		h.disc.catalog = unsorted
		expectJSON(t, h.do(t, http.MethodGet, namespacesPath), `{"namespaces":["orders","payments"]}`)
		h = listingHarness()
		h.disc.catalog = unsorted
		expectJSON(t, h.do(t, http.MethodGet, servicesPath), `{"namespace":"payments","services":["checkout","ledger"]}`)
	})

	t.Run("catalog namespace argument", func(t *testing.T) {
		h := listingHarness()
		h.do(t, http.MethodGet, servicesPath)
		if got := h.disc.catalogNamespacesSeen(); !reflect.DeepEqual(got, []string{"payments"}) {
			t.Errorf("Catalog namespaces = %q, want [payments]", got)
		}
		h = listingHarness()
		h.do(t, http.MethodGet, namespacesPath)
		if got := h.disc.catalogNamespacesSeen(); !reflect.DeepEqual(got, []string{""}) {
			t.Errorf("Catalog namespaces = %q, want [\"\"]", got)
		}
	})
}

func TestListingWhoami(t *testing.T) {
	developer := func(cfg *config.Config) {
		cfg.Realms["developer"] = config.Realm{
			Namespaces: []string{"*"}, Services: []string{"checkout"}, Profiles: []string{"cpu", "heap"},
			PGO: config.RealmPGO{Read: true},
		}
	}
	const realmJSON = `{"name":"developer","namespaces":["*"],"services":["checkout"],"profiles":["cpu","heap"],` +
		`"pgo":{"read":true,"collect":false,"configure":false}}`

	t.Run("verbatim", func(t *testing.T) {
		h := listingHarness()
		h.configure(developer)
		rec := h.do(t, http.MethodGet, whoamiPath)
		expectJSON(t, rec, `{"principal":"anonymous","realm":`+realmJSON+`,"auth":{"mode":"disabled"}}`)
		if strings.Contains(rec.Body.String(), "logout") {
			t.Errorf("body carries logout: %q", rec.Body.String())
		}
	})

	t.Run("logout", func(t *testing.T) {
		h := authHarness("oidc", admitAs("alice", "developer"))
		h.configure(developer)
		h.configure(func(cfg *config.Config) {
			cfg.Auth.OIDC = &config.OIDCConfig{Browser: &config.OIDCBrowser{}}
		})
		rec := h.do(t, http.MethodGet, whoamiPath)
		expectJSON(t, rec, `{"principal":"alice","realm":`+realmJSON+`,"auth":{"mode":"oidc","logout":"/auth/logout"}}`)
		audit := h.expectAudit(t, http.StatusOK, codeOK)
		if audit["principal"] != "alice" {
			t.Errorf("audit principal = %v, want alice", audit["principal"])
		}
	})

	t.Run("no logout under oidc without browser", func(t *testing.T) {
		h := authHarness("oidc", admitAs("alice", "developer"))
		h.configure(func(cfg *config.Config) { cfg.Auth.OIDC = &config.OIDCConfig{} })
		rec := h.do(t, http.MethodGet, whoamiPath)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "logout") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("under basic", func(t *testing.T) {
		h := authHarness("basic", admitAs("alice", "developer"))
		rec := h.do(t, http.MethodGet, whoamiPath)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"basic"`) ||
			strings.Contains(rec.Body.String(), "logout") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})
}

func TestListingLimits(t *testing.T) {
	profiles, _ := json.Marshal(config.Profiles())

	t.Run("numeric default", func(t *testing.T) {
		h := listingHarness()
		h.configure(func(cfg *config.Config) {
			cfg.Discovery.Pprof = config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060, 6061}, AllowedPortNames: []string{"pprof", "pprof-alt"}}
			cfg.Limits.CPUSeconds = 60
			cfg.Limits.TraceSeconds = 30
			cfg.PGO.Enabled = true
		})
		rec := httptest.NewRecorder()
		h.handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, limitsPath, nil))
		expectJSON(t, rec, `{"cpuSeconds":60,"traceSeconds":30,"profiles":`+string(profiles)+
			`,"pprof":{"default":{"port":6060},"allowedPorts":[6060,6061],"allowedPortNames":["pprof","pprof-alt"]},`+
			`"pgo":{"enabled":true}}`)
	})

	t.Run("named default", func(t *testing.T) {
		h := listingHarness()
		h.configure(func(cfg *config.Config) { cfg.Discovery.Pprof = config.PprofConfig{PortName: "pprof"} })
		rec := h.do(t, http.MethodGet, limitsPath)
		if !strings.Contains(rec.Body.String(), `"default":{"portName":"pprof"}`) {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("empty allowlists", func(t *testing.T) {
		h := listingHarness()
		rec := h.do(t, http.MethodGet, limitsPath)
		body := rec.Body.String()
		if !strings.Contains(body, `"allowedPorts":[]`) || !strings.Contains(body, `"allowedPortNames":[]`) || strings.Contains(body, "null") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("pgo off", func(t *testing.T) {
		h := listingHarness()
		rec := h.do(t, http.MethodGet, limitsPath)
		if !strings.Contains(rec.Body.String(), `"pgo":{"enabled":false}`) {
			t.Errorf("body = %q", rec.Body.String())
		}
	})
}

func TestListingDisclosure(t *testing.T) {
	ipRE := regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)

	t.Run("no address leaks", func(t *testing.T) {
		for name, path := range listingPaths() {
			t.Run(name, func(t *testing.T) {
				h := listingHarness()
				h.disc.targets = []k8s.Target{{Namespace: "payments", Service: "checkout", Pod: "checkout-1", PodIP: "10.1.2.3", Port: 6060}}
				rec := h.do(t, http.MethodGet, path)
				body := rec.Body.String()
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d", rec.Code)
				}
				if ipRE.MatchString(body) || strings.Contains(body, "podIP") {
					t.Errorf("body names an address: %q", body)
				}
				if (path == namespacesPath || path == servicesPath) && strings.Contains(body, "6060") {
					t.Errorf("body names a port: %q", body)
				}
				expectNoCORS(t, rec)
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q", got)
				}
			})
		}
	})

	t.Run("hostile names", func(t *testing.T) {
		const principal = `<b>"alice"&'`
		hostile := func() *harness {
			h := authHarness("basic", admitAs(principal, "developer"))
			h.configure(func(cfg *config.Config) {
				cfg.Realms["developer"] = config.Realm{
					Namespaces: []string{"*", "<x>"}, Services: []string{"*", "<x>"}, Profiles: []string{"cpu", "<x>"},
				}
			})
			h.disc.catalog = []k8s.ServiceRef{
				{Namespace: "payments", Name: "<svc>"},
				{Namespace: "<ns>&'", Name: "checkout"},
			}

			return h
		}
		// assertEscaped checks a 200 whose body carries no bare metacharacter and
		// holds every escape encoding/json emits for the ones the row configured.
		assertEscaped := func(t *testing.T, rec *httptest.ResponseRecorder, escapes ...string) {
			t.Helper()
			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body %q)", rec.Code, body)
			}
			if strings.ContainsAny(body, "<>&") {
				t.Errorf("body holds a bare metacharacter: %q", body)
			}
			for _, esc := range escapes {
				if !strings.Contains(body, esc) {
					t.Errorf("body lacks %s: %q", esc, body)
				}
			}
		}

		h := hostile()
		rec := h.do(t, http.MethodGet, whoamiPath)
		assertEscaped(t, rec, `\u003c`, `\u003e`, `\u0026`, `\"`)
		var who whoamiBody
		if err := json.Unmarshal(rec.Body.Bytes(), &who); err != nil {
			t.Fatal(err)
		}
		if who.Principal != principal || who.Realm.Namespaces[1] != "<x>" || who.Realm.Services[1] != "<x>" || who.Realm.Profiles[1] != "<x>" {
			t.Errorf("whoami = %+v", who)
		}

		h = hostile()
		rec = h.do(t, http.MethodGet, namespacesPath)
		assertEscaped(t, rec, `\u003c`, `\u003e`, `\u0026`)
		var ns namespacesBody
		if err := json.Unmarshal(rec.Body.Bytes(), &ns); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ns.Namespaces, []string{"<ns>&'", "payments"}) {
			t.Errorf("namespaces = %q", ns.Namespaces)
		}

		h = hostile()
		rec = h.do(t, http.MethodGet, servicesPath)
		assertEscaped(t, rec, `\u003c`, `\u003e`)
		var svc servicesBody
		if err := json.Unmarshal(rec.Body.Bytes(), &svc); err != nil {
			t.Fatal(err)
		}
		if svc.Namespace != "payments" || !reflect.DeepEqual(svc.Services, []string{"<svc>"}) {
			t.Errorf("services = %+v", svc)
		}
	})
}

func TestRouteKinds(t *testing.T) {
	get := []string{http.MethodGet}
	table := []struct {
		kind                 routeKind
		pgo, scoped, listing bool
		methods              []string
	}{
		{kindTargets, false, false, false, get},
		{kindProfile, false, false, false, get},
		{kindPGOPolicy, true, false, false, []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
		{kindCollections, true, false, false, []string{http.MethodGet, http.MethodPost}},
		{kindCollection, true, true, false, get},
		{kindCollectionProfile, true, true, false, get},
		{kindCollectionCancel, true, true, false, []string{http.MethodPost}},
		{kindNamespaces, false, false, true, get},
		{kindServices, false, false, true, get},
		{kindWhoami, false, false, true, get},
		{kindLimits, false, false, true, get},
	}
	if len(table) != int(kindLimits)+1 {
		t.Fatalf("table has %d rows, want %d: a kind was added without a row", len(table), int(kindLimits)+1)
	}
	for i, row := range table {
		t.Run(fmt.Sprintf("kind %d", row.kind), func(t *testing.T) {
			if row.kind != routeKind(i) {
				t.Errorf("row %d is kind %d", i, row.kind)
			}
			if got := row.kind.isPGO(); got != row.pgo {
				t.Errorf("kind %d isPGO = %v, want %v", row.kind, got, row.pgo)
			}
			if got := row.kind.isCollectionScoped(); got != row.scoped {
				t.Errorf("kind %d isCollectionScoped = %v, want %v", row.kind, got, row.scoped)
			}
			if got := row.kind.isListing(); got != row.listing {
				t.Errorf("kind %d isListing = %v, want %v", row.kind, got, row.listing)
			}
			if got := row.kind.methods(); !reflect.DeepEqual(got, row.methods) {
				t.Errorf("kind %d methods = %v, want %v", row.kind, got, row.methods)
			}
		})
	}
}

func TestListingAuditAndMetrics(t *testing.T) {
	endpoints := map[string]metrics.Endpoint{
		"namespaces": metrics.EndpointNamespaces,
		"services":   metrics.EndpointServices,
		"whoami":     metrics.EndpointWhoami,
		"limits":     metrics.EndpointLimits,
	}
	for name, path := range listingPaths() {
		t.Run(name, func(t *testing.T) {
			h := listingHarness()
			h.do(t, http.MethodGet, path)
			audit := h.expectAudit(t, http.StatusOK, codeOK)
			wantNamespace := ""
			if name == "services" {
				wantNamespace = "payments"
			}
			want := map[string]any{
				"principal": "anonymous", "namespace": wantNamespace, "service": "", "pod": "",
				"profile": "", "port": "", "seconds": float64(0), "code": codeOK,
			}
			for key, value := range want {
				if audit[key] != value {
					t.Errorf("audit %s = %v, want %v", key, audit[key], value)
				}
			}
			h.expectMetric(t, endpoints[name], labelNone)
			h.expectMetricCode(t, codeOK)
		})
	}
}
