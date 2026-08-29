package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// sampleValues is one legal value per path parameter,
// so a concrete path is derived from a template rather than restated beside it.
// A test that spelled its own paths could exercise a route the table no longer declares,
// which is the drift the table exists to stop.
func sampleValues() map[string]string {
	return map[string]string{
		paramNamespace: fixtureNamespace,
		paramService:   fixtureService,
		paramProfile:   "heap",
		paramID:        "abcdefghjkmnpqrstv01",
		paramFile:      "static/0123456789abcdef/app.js",
	}
}

// samplePath substitutes sampleValues into a template.
func samplePath(t *testing.T, template string) string {
	t.Helper()

	values := sampleValues()
	parts := strings.Split(template, "/")
	for i, part := range parts {
		name, isParam := strings.CutPrefix(part, "{")
		if !isParam {
			continue
		}
		value, ok := values[strings.TrimSuffix(name, "}")]
		if !ok {
			t.Fatalf("template %q names parameter %q, which has no sample value", template, part)
		}
		parts[i] = value
	}

	return strings.Join(parts, "/")
}

// refusedMethod is a method the declaration does not list.
func refusedMethod(t *testing.T, d declaration) string {
	t.Helper()

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if !slices.Contains(d.Methods, m) {
			return m
		}
	}
	t.Fatalf("declaration %q accepts every method a client can be refused with", d.Template)

	return ""
}

// endpointOf is the metrics endpoint each kind is counted under,
// written out here so the table cannot quietly move a route to another row of the metric.
func endpointOf(kind routeKind) metrics.Endpoint {
	switch kind {
	case kindTargets:
		return metrics.EndpointTargets
	case kindProfile:
		return metrics.EndpointProfile
	case kindPGOPolicy:
		return metrics.EndpointPGOPolicy
	case kindCollections:
		return metrics.EndpointCollections
	case kindCollection, kindCollectionLatest:
		return metrics.EndpointCollection
	case kindCollectionProfile, kindCollectionLatestProfile:
		return metrics.EndpointCollectionProfile
	case kindCollectionCancel:
		return metrics.EndpointCollectionCancel
	case kindNamespaces:
		return metrics.EndpointNamespaces
	case kindServices:
		return metrics.EndpointServices
	case kindWhoami:
		return metrics.EndpointWhoami
	case kindLimits:
		return metrics.EndpointLimits
	case kindAuth, kindAuthLogin, kindAuthCallback, kindAuthLogout:
		return metrics.EndpointAuth
	case kindOpenAPI:
		return metrics.EndpointOpenAPI
	case kindConsole:
		return metrics.EndpointUI
	default:
		return metrics.EndpointProfile
	}
}

// isV1 reports whether the declaration takes the /v1 algorithm in ServeHTTP,
// which is every kind but the console's and the three browser-login routes.
func isV1(kind routeKind) bool {
	return kind != kindConsole && !kind.isAuthRoute()
}

// profileLabelOf is the metrics profile label the kind carries:
// the requested name on the profile endpoint,
// cpu on the five routes that answer for a Collection, which profile CPU and nothing else,
// and none everywhere else.
func profileLabelOf(kind routeKind, name string) string {
	switch {
	case kind == kindProfile:
		return name
	case kind.isCollectionScoped():
		return labelCPU
	case kind == kindCollectionLatest || kind == kindCollectionLatestProfile:
		return labelCPU
	default:
		return labelNone
	}
}

// expectDeclarationError applies the assertion the declaration's audit shape owes.
// A PGO route writes the PGO record, /v1/auth writes none,
// and every other /v1 route writes the interactive one.
func expectDeclarationError(
	t *testing.T, h *harness, rec *httptest.ResponseRecorder, d declaration, status int, code string,
) {
	t.Helper()

	switch {
	case d.Kind == kindAuth:
		h.expectRouteError(t, rec, status, code)
	case d.Kind == kindOpenAPI:
		h.expectUnnarratedError(t, rec, status, code)
	case d.Kind.isPGO():
		h.expectPGOError(t, rec, status, code, code)
	default:
		h.expectError(t, rec, status, code)
	}
}

func TestRouteTableShape(t *testing.T) {
	table := declarations()
	if len(table) == 0 {
		t.Fatal("the route table is empty")
	}
	seen := map[string]bool{}
	kinds := map[routeKind]bool{}
	for _, d := range table {
		if seen[d.Template] {
			t.Errorf("template %q is declared twice", d.Template)
		}
		seen[d.Template] = true
		kinds[d.Kind] = true
		if !strings.HasPrefix(d.Template, "/") {
			t.Errorf("template %q is not an absolute path", d.Template)
		}
		if len(d.Methods) == 0 {
			t.Errorf("declaration %q lists no method", d.Template)
		}
	}
	for kind := kindTargets; kind <= kindConsole; kind++ {
		if !kinds[kind] {
			t.Errorf("kind %d has no declaration, so no request can reach its handler", kind)
		}
	}
}

func TestRouteTableIsCopied(t *testing.T) {
	first := declarations()
	first[0].Template = "/mutated"
	first[0].Methods[0] = "MUTATED"
	second := declarations()
	if second[0].Template == "/mutated" {
		t.Error("declarations hands out the table itself, not a copy")
	}
	if second[0].Methods[0] == "MUTATED" {
		t.Error("declarations shares the method slice with the table")
	}
}

func TestRouteTableCaptures(t *testing.T) {
	values := sampleValues()
	for _, d := range declarations() {
		t.Run(d.Template, func(t *testing.T) {
			path := samplePath(t, d.Template)
			rt, got, ok := match(path)
			if !ok {
				t.Fatalf("match(%q) found no declaration", path)
			}
			if got.Template != d.Template {
				t.Fatalf("match(%q) chose %q, want %q", path, got.Template, d.Template)
			}
			if rt.kind != d.Kind {
				t.Errorf("kind = %d, want %d", rt.kind, d.Kind)
			}
			// A field is filled when the template names its parameter and is
			// empty when it does not, so no route reads a segment it never captured.
			named := func(name string) string {
				if strings.Contains(d.Template, "{"+name+"}") {
					return values[name]
				}

				return ""
			}
			if rt.namespace != named(paramNamespace) {
				t.Errorf("namespace = %q, want %q", rt.namespace, named(paramNamespace))
			}
			if rt.service != named(paramService) {
				t.Errorf("service = %q, want %q", rt.service, named(paramService))
			}
			if rt.profile != named(paramProfile) {
				t.Errorf("profile = %q, want %q", rt.profile, named(paramProfile))
			}
			if rt.collection != named(paramID) {
				t.Errorf("collection = %q, want %q", rt.collection, named(paramID))
			}
		})
	}
}

func TestRouteTableAcceptedMethod(t *testing.T) {
	for _, d := range declarations() {
		if !isV1(d.Kind) {
			continue
		}
		for _, method := range d.Methods {
			t.Run(method+" "+d.Template, func(t *testing.T) {
				h := newHarness(baseTarget())
				h.ready = func() bool { return false }
				rec := h.doHeaders(t, method, samplePath(t, d.Template), clientHeaders(method))
				// Readiness is the step after the media type the two write routes require,
				// so reaching it proves the route matched and the method was accepted.
				expectDeclarationError(t, h, rec, d, http.StatusServiceUnavailable, CodeNotReady)
				h.expectMetric(t, endpointOf(d.Kind), profileLabelOf(d.Kind, sampleValues()[paramProfile]))
			})
		}
	}
}

func TestRouteTableRefusedMethod(t *testing.T) {
	for _, d := range declarations() {
		if !isV1(d.Kind) {
			continue
		}
		method := refusedMethod(t, d)
		t.Run(method+" "+d.Template, func(t *testing.T) {
			h := newHarness(baseTarget())
			rec := h.do(t, method, samplePath(t, d.Template))
			expectDeclarationError(t, h, rec, d, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
			if got, want := rec.Header().Get("Allow"), strings.Join(d.Methods, ", "); got != want {
				t.Errorf("Allow = %q, want %q", got, want)
			}
		})
	}
}

// browserLoginHarness is the harness with the three browser-login routes wired,
// which is what makes them reachable at all.
func browserLoginHarness() *harness {
	h := newHarness(baseTarget())
	h.configure(func(cfg *config.Config) {
		cfg.Auth.Mode = "oidc"
		cfg.Auth.AnonymousRealm = ""
	})
	h.routes = &fakeRoutes{outcome: auth.RouteOutcome{Status: http.StatusOK, Code: codeOK}}

	return h
}

func TestRouteTableAuthRoutes(t *testing.T) {
	for _, d := range declarations() {
		if !d.Kind.isAuthRoute() {
			continue
		}
		path := samplePath(t, d.Template)
		t.Run("refused method "+path, func(t *testing.T) {
			// The method check runs inside serveAuthRoute, before its readiness step,
			// so a refused method is answered while the caches are syncing.
			h := browserLoginHarness()
			h.ready = func() bool { return false }
			rec := h.do(t, refusedMethod(t, d), path)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405 (body %q)", rec.Code, rec.Body.String())
			}
			if code, _ := errorBodyOf(t, rec); code != CodeMethodNotAllowed {
				t.Errorf("code = %q, want %q", code, CodeMethodNotAllowed)
			}
			if got, want := rec.Header().Get("Allow"), strings.Join(d.Methods, ", "); got != want {
				t.Errorf("Allow = %q, want %q", got, want)
			}
			h.expectMetric(t, metrics.EndpointAuth, labelNone)
		})
		t.Run("not ready "+path, func(t *testing.T) {
			h := browserLoginHarness()
			h.ready = func() bool { return false }
			rec := h.do(t, http.MethodGet, path)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
			}
			if code, _ := errorBodyOf(t, rec); code != CodeNotReady {
				t.Errorf("code = %q, want %q", code, CodeNotReady)
			}
			h.expectMetric(t, metrics.EndpointAuth, labelNone)
		})
	}
}

// TestRouteTableAuthRoutesUnconfigured pins the metrics row of a browser-login
// path the binary was not built to serve: it is counted where an unmatched path
// is counted, not under the auth endpoint, because no auth route ran.
func TestRouteTableAuthRoutesUnconfigured(t *testing.T) {
	for _, d := range declarations() {
		if !d.Kind.isAuthRoute() {
			continue
		}
		t.Run(d.Template, func(t *testing.T) {
			h := newHarness(baseTarget())
			rec := h.do(t, http.MethodGet, samplePath(t, d.Template))
			h.expectError(t, rec, http.StatusNotFound, CodeRouteUnknown)
			h.expectMetric(t, metrics.EndpointProfile, labelNone)
			if record := h.expectAudit(t, http.StatusNotFound, CodeRouteUnknown); record["route"] != nil {
				t.Errorf("audit record names a route: %v", record)
			}
		})
	}
}

func TestRouteTableConsoleServesWhileSyncing(t *testing.T) {
	for _, d := range declarations() {
		if d.Kind != kindConsole {
			continue
		}
		t.Run(d.Template, func(t *testing.T) {
			c := &fakeConsole{status: http.StatusOK, body: "<html>"}
			h := consoleHarness(c)
			h.ready = func() bool { return false }
			rec := h.do(t, http.MethodGet, samplePath(t, d.Template))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: the console serves files the binary already holds", rec.Code)
			}
			if len(c.seen()) != 1 {
				t.Fatalf("console calls = %d, want 1", len(c.seen()))
			}
			h.expectUIMetric(t, codeOK)
		})
	}
}

// TestRouteTableConsoleAnswersItsOwnRefusal pins the split ui.md assigns:
// the declaration is the source of the console's methods,
// and the console writes the 405 itself, with its security headers.
// The Allow header those methods produce is asserted against the written one in the console's own package.
func TestRouteTableConsoleAnswersItsOwnRefusal(t *testing.T) {
	for _, d := range declarations() {
		if d.Kind != kindConsole {
			continue
		}
		t.Run(d.Template, func(t *testing.T) {
			if got, want := strings.Join(d.Methods, ", "), "GET, HEAD"; got != want {
				t.Errorf("declared methods = %q, want %q", got, want)
			}
			c := &fakeConsole{status: http.StatusMethodNotAllowed}
			h := consoleHarness(c)
			rec := h.do(t, http.MethodPost, samplePath(t, d.Template))
			if len(c.seen()) != 1 {
				t.Fatalf("console calls = %d, want 1: the router answered the refusal itself", len(c.seen()))
			}
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			h.expectUIMetric(t, CodeMethodNotAllowed)
		})
	}
}

func TestRouteUnknown(t *testing.T) {
	paths := []struct{ name, path string }{
		{"no such prefix", "/nope"},
		{"v1 root", "/v1"},
		{"trailing slash", "/v1/whoami/"},
		{"namespace is not a label", "/v1/namespaces/BAD/services/x/targets"},
		{"service is not a label", "/v1/namespaces/x/services/BAD_/targets"},
		{"empty namespace", "/v1/namespaces//services/x/targets"},
		{"identifier is too short", "/v1/collections/abc"},
		{"identifier carries a separator", "/v1/collections/abcdefghjkmnpqrstv01/../etc"},
		{"unknown collection suffix", "/v1/collections/abcdefghjkmnpqrstv01/bogus"},
		{"latest is a path segment and not an identifier", "/v1/collections/latest"},
		{"profile carries a separator", "/v1/namespaces/x/services/y/profiles/heap/extra"},
		{"unknown auth route", "/auth/bogus"},
		{"auth root", "/auth/"},
		{"ui without its slash", "/ui"},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := match(tc.path); ok {
				t.Fatalf("match(%q) resolved a declaration", tc.path)
			}
			h := consoleHarness(&fakeConsole{status: http.StatusOK, body: "<html>"})
			rec := h.do(t, http.MethodGet, tc.path)
			h.expectError(t, rec, http.StatusNotFound, CodeRouteUnknown)
		})
	}
}

// TestMatchIsTheOnlyPathDispatch reads the package's own source.
// The table would describe only part of what the listener serves
// if a second place decided a route from the request path.
func TestMatchIsTheOnlyPathDispatch(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package source: %v", err)
	}
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name) //nolint:gosec // G304: name comes from a glob of this package's own directory
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		n := strings.Count(string(b), "r.URL.Path")
		if n > 0 && name != "server.go" {
			t.Errorf("%s reads the request path %d time(s); only the match in server.go may", name, n)
		}
		found += n
	}
	if found != 1 {
		t.Errorf("the package reads the request path %d times, want 1", found)
	}
}
