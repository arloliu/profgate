package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/pgo"
)

// pgoRoute is one row of the spec's route/method/realm-flag table.
type pgoRoute struct {
	name     string
	method   string
	path     func(id string) string
	body     string
	realm    config.RealmPGO
	endpoint metrics.Endpoint
	profile  string
}

// pgoRouteTable is every PGO route and method with the realm flag it needs and
// the metrics labels it records under.
func pgoRouteTable() []pgoRoute {
	read := config.RealmPGO{Read: true}
	collect := config.RealmPGO{Collect: true}
	configure := config.RealmPGO{Configure: true}
	service := func(string) string { return pgoPath }
	collections := func(string) string { return collectionsPath }

	return []pgoRoute{
		{"read the policy", http.MethodGet, service, "", read, metrics.EndpointPGOPolicy, labelNone},
		{"write the policy", http.MethodPut, service, `{"enabled":true}`, configure, metrics.EndpointPGOPolicy, labelNone},
		{"delete the policy", http.MethodDelete, service, "", configure, metrics.EndpointPGOPolicy, labelNone},
		{"list collections", http.MethodGet, collections, "", read, metrics.EndpointCollections, labelNone},
		{"create a collection", http.MethodPost, collections, `{}`, collect, metrics.EndpointCollections, labelNone},
		{
			"get a collection", http.MethodGet,
			func(id string) string { return collectionPath(id, "") }, "",
			read, metrics.EndpointCollection, labelCPU,
		},
		{
			"download a collection", http.MethodGet,
			func(id string) string { return collectionPath(id, "/profile") }, "",
			read, metrics.EndpointCollectionProfile, labelCPU,
		},
		{
			"cancel a collection", http.MethodPost,
			func(id string) string { return collectionPath(id, "/cancel") }, "",
			collect, metrics.EndpointCollectionCancel, labelCPU,
		},
	}
}

// TestPGORouteRealmFlags is the realm half of the route table: each route needs
// its own flag, and a realm without it never reaches the handler.
// A Service-scoped route denies with 403; a Collection-scoped one answers
// 404 collection_not_found, the same as a Collection that does not exist.
func TestPGORouteRealmFlags(t *testing.T) {
	flags := map[string]config.RealmPGO{
		"read":      {Read: true},
		"collect":   {Collect: true},
		"configure": {Configure: true},
		"none":      {},
	}

	for _, route := range pgoRouteTable() {
		for name, realm := range flags {
			t.Run(route.name+" with pgo."+name, func(t *testing.T) {
				scoped := wideRealm()
				scoped.PGO = realm
				h := newPGOHarness(t, pgoOpts{realm: &scoped})
				rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

				allowed := realm == route.realm
				got := h.doPGO(t, route.method, route.path(rec.ID), route.body, nil)

				collectionScoped := strings.HasPrefix(route.path(rec.ID), "/v1/collections/")
				switch {
				case allowed && (got.Code == http.StatusForbidden || denialOf(got) == "collection_not_found"):
					t.Errorf("status = %d (%s), want the route to be reached", got.Code, got.Body.String())
				case allowed:
				case collectionScoped:
					h.expectPGOError(t, got, http.StatusNotFound, "collection_not_found", "collection_not_found")
				default:
					h.expectPGOError(t, got, http.StatusForbidden, "realm_denied", "realm_denied")
				}
			})
		}
	}
}

// denialOf is the code of a response that carries an error envelope, or the
// empty string for one that does not.
func denialOf(rec *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return ""
	}

	return body.Code
}

// TestPGORoutesRejectOtherMethods pins the per-route Allow list of the spec's
// route table: the methods a route does not accept answer 405 naming the ones
// it does.
func TestPGORoutesRejectOtherMethods(t *testing.T) {
	cases := []struct {
		name    string
		path    func(id string) string
		refused []string
		allow   string
	}{
		{"policy", func(string) string { return pgoPath }, []string{http.MethodPost, http.MethodPatch}, "GET, PUT, DELETE"},
		{
			"collections", func(string) string { return collectionsPath },
			[]string{http.MethodPut, http.MethodDelete}, "GET, POST",
		},
		{
			"collection", func(id string) string { return collectionPath(id, "") },
			[]string{http.MethodPost, http.MethodDelete}, "GET",
		},
		{
			"download", func(id string) string { return collectionPath(id, "/profile") },
			[]string{http.MethodPost, http.MethodPut}, "GET",
		},
		{
			"cancel", func(id string) string { return collectionPath(id, "/cancel") },
			[]string{http.MethodGet, http.MethodDelete}, "POST",
		},
	}

	for _, tc := range cases {
		for _, method := range tc.refused {
			t.Run(tc.name+" refuses "+method, func(t *testing.T) {
				h := newPGOHarness(t, pgoOpts{})
				rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

				got := h.doPGO(t, method, tc.path(rec.ID), "", nil)

				h.expectPGOError(t, got, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
				if allow := got.Header().Get("Allow"); allow != tc.allow {
					t.Errorf("Allow = %q, want %q", allow, tc.allow)
				}
			})
		}
	}
}

// TestPGODisabledAnswers501 proves the step between readiness and
// authentication: a gateway that does not collect says so, and says it before
// anything about the realm the caller asked through.
func TestPGODisabledAnswers501(t *testing.T) {
	for _, route := range pgoRouteTable() {
		t.Run(route.name, func(t *testing.T) {
			denied := wideRealm()
			denied.PGO = config.RealmPGO{}
			h := newPGOHarness(t, pgoOpts{realm: &denied})
			id := h.newRecord(pgo.StatePending).ID
			h.configure(func(cfg *config.Config) { cfg.PGO.Enabled = false })

			got := h.doPGO(t, route.method, route.path(id), route.body, nil)

			h.expectPGOError(t, got, http.StatusNotImplemented, "pgo_disabled", "pgo_disabled")
		})
	}
}

// TestPGOUnavailableWhileUnbound proves the late-binding seam from the outside:
// the server serves interactive routes before the NATS preflight has passed,
// and every PGO route answers 503 until the runtime is bound.
func TestPGOUnavailableWhileUnbound(t *testing.T) {
	for _, route := range pgoRouteTable() {
		t.Run(route.name, func(t *testing.T) {
			h := newHarness(baseTarget())
			h.configure(func(cfg *config.Config) {
				cfg.PGO.Enabled = true
				cfg.PGO.Limits = testPGOLimits()
				cfg.PGO.Defaults = testPGODefaults()
				cfg.Realms["developer"] = wideRealm()
			})

			got := h.do(t, route.method, route.path("abcdefghjkmnpqrstv01"))

			h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
		})
	}

	t.Run("the interactive routes are unaffected", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.PGO.Enabled = true
			cfg.Realms["developer"] = wideRealm()
		})

		if got := h.do(t, http.MethodGet, targetsPath); got.Code != http.StatusOK {
			t.Errorf("targets status = %d, want 200 while the runtime is unbound", got.Code)
		}
	})
}

// TestPGOUnavailableBehindTheBarrier proves the other two ways the stores
// cannot be decided from: the connection is down, or the watches have not
// finished replaying under the current generation.
// Neither is a reason to take the replica out of the Service, so the
// interactive routes keep answering throughout.
func TestPGOUnavailableBehindTheBarrier(t *testing.T) {
	cases := []struct {
		name string
		hold func(*pgoHarness)
	}{
		{"the connection is down", func(h *pgoHarness) { h.nats.disconnect() }},
		{"the seam's watches have not replayed", func(h *pgoHarness) { h.nats.holdReplay() }},
	}

	for _, tc := range cases {
		for _, route := range pgoRouteTable() {
			t.Run(tc.name+", "+route.name, func(t *testing.T) {
				h := newPGOHarness(t, pgoOpts{})
				rec := h.seedRecord(t, h.newRecord(pgo.StatePending))
				tc.hold(h)

				got := h.doPGO(t, route.method, route.path(rec.ID), route.body, nil)

				h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
			})
		}

		t.Run(tc.name+", the interactive routes still answer", func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			tc.hold(h)

			if got := h.do(t, http.MethodGet, targetsPath); got.Code != http.StatusOK {
				t.Errorf("targets status = %d, want 200 while pgo state is unavailable", got.Code)
			}
		})
	}
}

// TestPGORouteGrammar pins what the routes match: the identifier grammar
// exactly, so a path carrying a separator or a traversal segment is a route
// the gateway does not have rather than an identifier it reads.
func TestPGORouteGrammar(t *testing.T) {
	paths := []string{
		"/v1/collections/",
		"/v1/collections/short",
		"/v1/collections/ABCDEFGHJKMNPQRSTV01",
		"/v1/collections/abcdefghjkmnpqrstv0i",
		"/v1/collections/abcdefghjkmnpqrstv01/",
		"/v1/collections/abcdefghjkmnpqrstv01/manifest",
		"/v1/collections/abcdefghjkmnpqrstv01/profile/extra",
		"/v1/namespaces/payment/services/payment-api/pgo/extra",
		"/v1/namespaces/payment/services/payment-api/collections/abcdefghjkmnpqrstv01",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})

			got := h.doPGO(t, http.MethodGet, path, "", nil)

			h.expectError(t, got, http.StatusNotFound, "route_unknown")
		})
	}
}

// TestPGOBodiesAreBounded pins the body rules every PGO write shares: at most
// 64 KiB, no field the type does not declare, and no body at all where the
// route takes none.
func TestPGOBodiesAreBounded(t *testing.T) {
	t.Run("a body over the limit is refused", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		body := `{"enabled":true,"target":{"version":"` + strings.Repeat("v", maxBodyBytes) + `"}}`

		got := h.doPGO(t, http.MethodPut, pgoPath, body, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
	})

	t.Run("an unknown field in a policy is refused", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodPut, pgoPath, `{"enabled":true,"sampling":{"round":3}}`, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
	})

	t.Run("an unknown field in a collection request is refused", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"duration":"10s"},"slot":"now"}`, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
	})

	for _, body := range []string{`{"enabled":true}`, `{"schedule":{"every":"1h"}}`} {
		t.Run("a collection request may not carry "+body, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})

			got := h.doPGO(t, http.MethodPost, collectionsPath, body, nil)

			h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
			if got := h.nats.jobs.countKeys(jobKeyPrefix); got != 0 {
				t.Errorf("job keys = %d, want none written", got)
			}
		})
	}

	t.Run("cancel takes no body", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

		got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), `{"reason":"mine"}`, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
		if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StatePending {
			t.Errorf("state = %q, want the record untouched", state)
		}
	})

	t.Run("a policy delete takes no body", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		enabled := true
		revision := h.seedOverride(t, &pgo.PolicyOverride{Enabled: &enabled})

		got := h.doPGO(t, http.MethodDelete, pgoPath, `{"enabled":false}`, ifMatch(etagOf(revision)))

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
		if n := h.nats.config.countKeys(overrideKeyPrefix); n != 1 {
			t.Errorf("override keys = %d, want the override kept", n)
		}
	})

	t.Run("query parameters are refused", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodGet, collectionsPath+"?state=completed", "", nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
	})
}

// TestPGOMetricsLabels pins the endpoint and profile labels of the new routes:
// a Collection profiles CPU and nothing else, and no namespace, Service, or
// identifier becomes a label.
func TestPGOMetricsLabels(t *testing.T) {
	for _, route := range pgoRouteTable() {
		t.Run(route.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

			h.doPGO(t, route.method, route.path(rec.ID), route.body, nil)

			h.expectMetric(t, route.endpoint, route.profile)
		})
	}
}

// TestPGOBodyFaultDetails reads the details array out of the encoded body of every body and header refusal,
// so a client is told which field of its own request to change:
// a pointer into the body, or the header by name.
func TestPGOBodyFaultDetails(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		header http.Header
		want   []errorDetail
	}{
		{
			name: "an unknown top-level field", method: http.MethodPut, path: pgoPath,
			body: `{"bogus":1}`,
			want: []errorDetail{{Field: "/bogus", Code: detailUnknownField}},
		},
		{
			name: "an unknown field in schedule", method: http.MethodPut, path: pgoPath,
			body: `{"schedule":{"bogus":"1h"}}`,
			want: []errorDetail{{Field: "/schedule/bogus", Code: detailUnknownField}},
		},
		{
			name: "an unknown field in sampling", method: http.MethodPut, path: pgoPath,
			body: `{"sampling":{"bogus":3}}`,
			want: []errorDetail{{Field: "/sampling/bogus", Code: detailUnknownField}},
		},
		{
			name: "an unknown field in target", method: http.MethodPut, path: pgoPath,
			body: `{"target":{"bogus":"x"}}`,
			want: []errorDetail{{Field: "/target/bogus", Code: detailUnknownField}},
		},
		{
			name: "an unknown field in artifact", method: http.MethodPut, path: pgoPath,
			body: `{"artifact":{"bogus":"1h"}}`,
			want: []errorDetail{{Field: "/artifact/bogus", Code: detailUnknownField}},
		},
		{
			name: "two unknown fields name the first", method: http.MethodPut, path: pgoPath,
			body: `{"bogus":1,"other":2}`,
			want: []errorDetail{{Field: "/bogus", Code: detailUnknownField}},
		},
		{
			name: "document order decides, not name order", method: http.MethodPut, path: pgoPath,
			body: `{"other":2,"bogus":1}`,
			want: []errorDetail{{Field: "/other", Code: detailUnknownField}},
		},
		{
			name: "a value of the wrong type is not an unknown field", method: http.MethodPut, path: pgoPath,
			body: `{"sampling":{"rounds":"three"}}`,
			want: []errorDetail{{Field: "", Code: detailBodyMalformed}},
		},
		{
			name: "an object where a value decodes itself", method: http.MethodPut, path: pgoPath,
			body: `{"sampling":{"replicas":{"bogus":1}}}`,
			want: []errorDetail{{Field: "", Code: detailBodyMalformed}},
		},
		{
			name: "a body that is not JSON", method: http.MethodPut, path: pgoPath,
			body: `not json`,
			want: []errorDetail{{Field: "", Code: detailBodyMalformed}},
		},
		{
			name: "a body over the limit", method: http.MethodPut, path: pgoPath,
			body: `{"enabled":true,"target":{"version":"` + strings.Repeat("v", maxBodyBytes) + `"}}`,
			want: []errorDetail{{Field: "", Code: detailBodyMalformed}},
		},
		{
			name: "a policy delete takes no body", method: http.MethodDelete, path: pgoPath,
			body: `{"enabled":false}`,
			want: []errorDetail{{Field: "", Code: detailBodyNotAllowed}},
		},
		{
			name: "a create sets neither enabled nor schedule", method: http.MethodPost, path: collectionsPath,
			body: `{"enabled":true,"schedule":{"every":"1h"}}`,
			want: []errorDetail{
				{Field: "/enabled", Code: detailFieldNotApplicable},
				{Field: "/schedule", Code: detailFieldNotApplicable},
			},
		},
		{
			name: "a create sets enabled alone", method: http.MethodPost, path: collectionsPath,
			body: `{"enabled":true}`,
			want: []errorDetail{{Field: "/enabled", Code: detailFieldNotApplicable}},
		},
		{
			name: "If-Match is a wildcard", method: http.MethodPut, path: pgoPath,
			body: `{"enabled":true}`, header: ifMatch(`*`),
			want: []errorDetail{{Field: "If-Match", Code: detailHeaderMalformed}},
		},
		{
			name: "If-Match is unquoted", method: http.MethodPut, path: pgoPath,
			body: `{"enabled":true}`, header: ifMatch(`42`),
			want: []errorDetail{{Field: "If-Match", Code: detailHeaderMalformed}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})

			got := h.doPGO(t, tc.method, tc.path, tc.body, tc.header)

			h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
			expectDetails(t, got, "invalid_parameter", tc.want)
		})
	}

	t.Run("cancel takes no body", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

		got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), `{"reason":"mine"}`, nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
		expectDetails(t, got, "invalid_parameter",
			[]errorDetail{{Field: "", Code: detailBodyNotAllowed}})
	})

	t.Run("a query parameter on a pgo route names itself", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})

		got := h.doPGO(t, http.MethodGet, collectionsPath+"?state=completed", "", nil)

		h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
		expectDetails(t, got, "invalid_parameter",
			[]errorDetail{{Field: "state", Code: detailUnknownParameter}})
	})
}
