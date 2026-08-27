package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/admit"
	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/proxy"
)

func TestRouting(t *testing.T) {
	t.Run("route unknown", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, "/v1/bogus")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
		h.expectMetric(t, metrics.EndpointProfile, "none")
		h.expectCounts(t, 0, 0)
	})

	t.Run("trailing slash", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, targetsPath+"/")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
	})

	t.Run("bad namespace segment", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, "/v1/namespaces/Bad_/services/x/targets")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
	})

	t.Run("bad service segment", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, "/v1/namespaces/payment/services/Bad_/profiles/heap")
		h.expectError(t, rec, http.StatusNotFound, "route_unknown")
	})

	t.Run("bad route beats method", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/v1/namespaces/Bad_/services/x/targets"},
			{http.MethodHead, "/v1/bogus"},
		} {
			t.Run(tc.method+" "+tc.path, func(t *testing.T) {
				h := newHarness(baseTarget())
				rec := h.do(t, tc.method, tc.path)
				h.expectError(t, rec, http.StatusNotFound, "route_unknown")
				if rec.Header().Get("Allow") != "" {
					t.Errorf("Allow header set on a route failure")
				}
			})
		}
	})

	t.Run("method", func(t *testing.T) {
		for _, tc := range []struct {
			method, path string
			endpoint     metrics.Endpoint
			profile      string
		}{
			{http.MethodPost, targetsPath, metrics.EndpointTargets, "none"},
			{http.MethodHead, profilePath + "heap", metrics.EndpointProfile, "heap"},
		} {
			t.Run(tc.method, func(t *testing.T) {
				h := newHarness(baseTarget())
				rec := h.do(t, tc.method, tc.path)
				h.expectError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
				if got := rec.Header().Get("Allow"); got != "GET" {
					t.Errorf("Allow = %q, want GET", got)
				}
				h.expectMetric(t, tc.endpoint, tc.profile)
				h.expectCounts(t, 0, 0)
			})
		}
	})

	t.Run("profile unknown", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, profilePath+"bogus")
		h.expectError(t, rec, http.StatusNotFound, "profile_unknown")
		h.expectMetric(t, metrics.EndpointProfile, "none")
		h.expectCounts(t, 0, 0)
	})
}

func TestGate(t *testing.T) {
	t.Run("not synced", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.disc.synced = false
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")
		h.expectMetric(t, metrics.EndpointTargets, "none")
		h.expectCounts(t, 0, 0)
	})

	denyNamespace := func(cfg *config.Config) {
		cfg.Realms["developer"] = config.Realm{Namespaces: []string{"billing"}, Services: []string{"*"}, Profiles: []string{"*"}}
	}

	t.Run("realm denied, service exists", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(denyNamespace)
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusForbidden, "realm_denied")
		h.expectCounts(t, 0, 0)
	})

	t.Run("realm denied, service missing", func(t *testing.T) {
		exists := newHarness(baseTarget())
		exists.configure(denyNamespace)
		existsRec := exists.do(t, http.MethodGet, targetsPath)

		missing := newHarness()
		missing.configure(denyNamespace)
		missing.disc.err = k8s.ErrServiceNotFound
		rec := missing.do(t, http.MethodGet, targetsPath)
		missing.expectError(t, rec, http.StatusForbidden, "realm_denied")
		if rec.Body.String() != existsRec.Body.String() {
			t.Errorf("denial body differs by Service existence: %q vs %q", rec.Body.String(), existsRec.Body.String())
		}
		missing.expectCounts(t, 0, 0)
	})

	t.Run("realm denies profile", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.Realms["developer"] = config.Realm{Namespaces: []string{"*"}, Services: []string{"*"}, Profiles: []string{"heap"}}
		})
		rec := h.do(t, http.MethodGet, profilePath+"cpu")
		h.expectError(t, rec, http.StatusForbidden, "realm_denied")
		h.expectMetric(t, metrics.EndpointProfile, "cpu")
		h.expectCounts(t, 0, 0)
	})

	t.Run("realm missing from config denies", func(t *testing.T) {
		// Validation refuses this configuration; a snapshot that carries it
		// anyway fails closed as a principal mapped to no realm.
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) { cfg.Auth.AnonymousRealm = "nobody" })
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusUnauthorized, "unauthenticated")
		h.expectAuthAudit(t, http.StatusUnauthorized, "unauthenticated", auth.ReasonNoRealm)
	})

	t.Run("snapshot", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.disc.onTargets = func() {
			cfg := testConfig()
			denyNamespace(cfg)
			h.cfg.Store(cfg)
		}
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: the request must finish under the config it loaded", rec.Code)
		}
		h.expectCounts(t, 1, 0)
	})
}

func TestTargets(t *testing.T) {
	t.Run("targets query", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, targetsPath+"?x=1")
		h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
		h.expectMetric(t, metrics.EndpointTargets, "none")
		h.expectCounts(t, 0, 0)
	})

	t.Run("targets ok", func(t *testing.T) {
		h := newHarness(namedTarget("payment-api-2", "2.0"), namedTarget("payment-api-1", ""))
		rec := h.do(t, http.MethodGet, targetsPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body struct {
			Namespace string              `json:"namespace"`
			Service   string              `json:"service"`
			Targets   []map[string]string `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		if body.Namespace != fixtureNamespace || body.Service != fixtureService {
			t.Errorf("namespace/service = %q/%q", body.Namespace, body.Service)
		}
		if len(body.Targets) != 2 || body.Targets[0]["pod"] != "payment-api-1" || body.Targets[1]["pod"] != "payment-api-2" {
			t.Errorf("targets not sorted by pod: %v", body.Targets)
		}
		for _, target := range body.Targets {
			if len(target) != 3 {
				t.Errorf("target has keys other than pod,node,version: %v", target)
			}
			if _, ok := target["version"]; !ok {
				t.Errorf("version key missing: %v", target)
			}
		}
		if body.Targets[0]["version"] != "" || body.Targets[1]["version"] != "2.0" {
			t.Errorf("versions = %q,%q", body.Targets[0]["version"], body.Targets[1]["version"])
		}
		h.expectAudit(t, http.StatusOK, "ok")
		h.expectMetric(t, metrics.EndpointTargets, "none")
		h.expectMetricCode(t, "ok")
		h.expectCounts(t, 1, 0)
	})

	t.Run("targets empty", func(t *testing.T) {
		h := newHarness()
		rec := h.do(t, http.MethodGet, targetsPath)
		want := `{"namespace":"payment","service":"payment-api","targets":[]}` + "\n"
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("status %d body %q, want 200 %q", rec.Code, rec.Body.String(), want)
		}
	})

	t.Run("service not found", func(t *testing.T) {
		h := newHarness()
		h.disc.err = k8s.ErrServiceNotFound
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusNotFound, "service_not_found")
	})

	t.Run("selectorless", func(t *testing.T) {
		h := newHarness()
		h.disc.err = k8s.ErrServiceSelectorless
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		h.expectError(t, rec, http.StatusUnprocessableEntity, "service_selectorless")
		h.expectCounts(t, 1, 0)
	})

	t.Run("other discovery error", func(t *testing.T) {
		h := newHarness()
		h.disc.err = errors.New("cache exploded")
		rec := h.do(t, http.MethodGet, targetsPath)
		h.expectError(t, rec, http.StatusServiceUnavailable, "discovery_unavailable")
	})
}

func TestProfileParameters(t *testing.T) {
	invalid := func(name, target string) {
		t.Run(name, func(t *testing.T) {
			h := newHarness(baseTarget())
			rec := h.do(t, http.MethodGet, target)
			h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
			h.expectCounts(t, 0, 0)
		})
	}

	invalid("seconds on heap", profilePath+"heap?seconds=1")
	for _, q := range []string{"abc", "0", "-1", "1&seconds=2", "", "99999999999", "+5", "86401"} {
		invalid("seconds grammar "+q, profilePath+"cpu?seconds="+q)
	}
	invalid("unknown param", profilePath+"heap?foo=1")
	for _, q := range []string{"", "a&pod=b", "Bad_"} {
		invalid("pod grammar "+q, profilePath+"heap?pod="+q)
	}
	for _, q := range []string{"", "a&version=b"} {
		invalid("version grammar "+q, profilePath+"heap?version="+q)
	}
	for _, q := range []string{"roundrobin", ""} {
		invalid("strategy grammar "+q, profilePath+"heap?strategy="+q)
	}
	invalid("malformed query", profilePath+"heap?pod=%zz")

	t.Run("seconds over limit", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, profilePath+"cpu?seconds=61")
		h.expectError(t, rec, http.StatusBadRequest, "seconds_exceeds_limit")
		h.expectCounts(t, 0, 0)
	})

	t.Run("default over limit", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) { cfg.Limits.CPUSeconds = 10 })
		rec := h.do(t, http.MethodGet, profilePath+"cpu")
		h.expectError(t, rec, http.StatusBadRequest, "seconds_exceeds_limit")
		h.expectCounts(t, 0, 0)
	})

	t.Run("trace over limit", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) { cfg.Limits.TraceSeconds = 5 })
		rec := h.do(t, http.MethodGet, profilePath+"trace?seconds=6")
		h.expectError(t, rec, http.StatusBadRequest, "seconds_exceeds_limit")
	})

	t.Run("effective sent", func(t *testing.T) {
		for _, tc := range []struct {
			profile string
			seconds int
			path    string
		}{
			{"cpu", 30, "/debug/pprof/profile"},
			{"trace", 1, "/debug/pprof/trace"},
			{"heap", 0, "/debug/pprof/heap"},
		} {
			t.Run(tc.profile, func(t *testing.T) {
				h := newHarness(baseTarget())
				rec := h.do(t, http.MethodGet, profilePath+tc.profile)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
				}
				reqs := h.up.requests()
				if len(reqs) != 1 {
					t.Fatalf("Upstream.Do calls = %d, want 1", len(reqs))
				}
				if reqs[0].Seconds != tc.seconds || reqs[0].Path != tc.path {
					t.Errorf("request = %d %q, want %d %q", reqs[0].Seconds, reqs[0].Path, tc.seconds, tc.path)
				}
				audit := h.expectAudit(t, http.StatusOK, "ok")
				if got, _ := audit["seconds"].(float64); int(got) != tc.seconds {
					t.Errorf("audit seconds = %v, want %d", audit["seconds"], tc.seconds)
				}
			})
		}
	})

	t.Run("explicit seconds sent", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.do(t, http.MethodGet, profilePath+"cpu?seconds=7")
		if reqs := h.up.requests(); len(reqs) != 1 || reqs[0].Seconds != 7 {
			t.Errorf("requests = %+v, want one with Seconds 7", reqs)
		}
	})
}

func TestSelection(t *testing.T) {
	t.Run("pod subdomain ok", func(t *testing.T) {
		h := newHarness(namedTarget("a.b-c", "1.0"))
		rec := h.do(t, http.MethodGet, profilePath+"heap?pod=a.b-c")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if reqs := h.up.requests(); len(reqs) != 1 || reqs[0].Target.Pod != "a.b-c" {
			t.Errorf("requests = %+v", reqs)
		}
	})

	t.Run("pod with strategy accepted", func(t *testing.T) {
		h := newHarness(namedTarget("a", "1.0"), namedTarget("b", "1.0"))
		var chooseCalls atomic.Int32
		h.choose = func(int) int { chooseCalls.Add(1); return 0 }
		rec := h.do(t, http.MethodGet, profilePath+"heap?pod=b&strategy=random")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if reqs := h.up.requests(); len(reqs) != 1 || reqs[0].Target.Pod != "b" {
			t.Errorf("requests = %+v, want pod b", reqs)
		}
		if chooseCalls.Load() != 0 {
			t.Errorf("Choose called %d times with pod given; strategy has nothing to choose from", chooseCalls.Load())
		}
	})

	for _, query := range []string{"", "?strategy=random"} {
		name := "strategy default"
		if query != "" {
			name = "strategy explicit"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(namedTarget("a", "1.0"), namedTarget("b", "1.0"), namedTarget("c", "1.0"))
			var ns []int
			h.choose = func(n int) int { ns = append(ns, n); return 1 }
			rec := h.do(t, http.MethodGet, profilePath+"heap"+query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
			}
			if len(ns) != 1 || ns[0] != 3 {
				t.Errorf("Choose calls = %v, want [3]", ns)
			}
			if reqs := h.up.requests(); len(reqs) != 1 || reqs[0].Target.Pod != "b" {
				t.Errorf("requests = %+v, want index 1 (pod b)", reqs)
			}
		})
	}

	t.Run("version filter", func(t *testing.T) {
		h := newHarness(namedTarget("a", "1.0"), namedTarget("b", "2.0"), namedTarget("c", ""))
		var ns []int
		h.choose = func(n int) int { ns = append(ns, n); return 0 }
		rec := h.do(t, http.MethodGet, profilePath+"heap?version=1.0")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if len(ns) != 1 || ns[0] != 1 {
			t.Errorf("Choose calls = %v, want [1]: only the matching target remains", ns)
		}
		if reqs := h.up.requests(); len(reqs) != 1 || reqs[0].Target.Pod != "a" {
			t.Errorf("requests = %+v, want pod a", reqs)
		}
	})

	t.Run("pod wrong version", func(t *testing.T) {
		h := newHarness(namedTarget("a", "1.0"))
		rec := h.do(t, http.MethodGet, profilePath+"heap?pod=a&version=2.0")
		h.expectError(t, rec, http.StatusNotFound, "pod_not_found")
		h.expectCounts(t, 1, 0)
	})

	t.Run("pod not target", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, profilePath+"heap?pod=zzz")
		h.expectError(t, rec, http.StatusNotFound, "pod_not_found")
	})

	t.Run("no targets", func(t *testing.T) {
		h := newHarness()
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		h.expectError(t, rec, http.StatusServiceUnavailable, "no_targets")
		h.expectCounts(t, 1, 0)
	})

	t.Run("version filters everything out", func(t *testing.T) {
		h := newHarness(namedTarget("a", "1.0"))
		rec := h.do(t, http.MethodGet, profilePath+"heap?version=9.9")
		h.expectError(t, rec, http.StatusServiceUnavailable, "no_targets")
	})
}

func TestAdmission(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) { cfg.Limits.MaxConcurrentProfiles = 1 })
		release := make(chan struct{})
		h.up.release = release
		handler := h.handler()

		first := httptest.NewRecorder()
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.ServeHTTP(first, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
		}()
		go func() {
			for h.up.calls.Load() == 0 {
				time.Sleep(time.Millisecond)
			}
			close(started)
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the first request never reached the upstream")
		}

		second := httptest.NewRecorder()
		handler.ServeHTTP(second, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
		assertNoLeak(t, second)
		if second.Code != http.StatusTooManyRequests {
			t.Errorf("second status = %d, want 429", second.Code)
		}
		if code, _ := errorBodyOf(t, second); code != "too_many_profiles" {
			t.Errorf("second code = %q, want too_many_profiles", code)
		}
		if _, _, inFlight := h.rec.snapshot(); inFlight != 1 {
			t.Errorf("ProfilesInFlight = %d while one profile is held, want 1", inFlight)
		}

		close(release)
		<-done
		if first.Code != http.StatusOK {
			t.Errorf("first status = %d, want 200", first.Code)
		}
		requests, _, inFlight := h.rec.snapshot()
		if inFlight != 0 {
			t.Errorf("ProfilesInFlight net = %d, want 0", inFlight)
		}
		if len(requests) != 2 {
			t.Errorf("Recorder.Request calls = %v, want one per request", requests)
		}

		third := httptest.NewRecorder()
		handler.ServeHTTP(third, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
		if third.Code != http.StatusOK {
			t.Errorf("status after release = %d, want 200: the slot was not returned", third.Code)
		}
	})

	t.Run("slots come from the startup config", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) { cfg.Limits.MaxConcurrentProfiles = 1 })
		release := make(chan struct{})
		h.up.release = release
		handler := h.handler()
		// A later config with a higher cap does not grow the slots: the field is restart-only.
		h.configure(func(cfg *config.Config) { cfg.Limits.MaxConcurrentProfiles = 8 })

		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
		}()
		for h.up.calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		second := httptest.NewRecorder()
		handler.ServeHTTP(second, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
		if second.Code != http.StatusTooManyRequests {
			t.Errorf("second status = %d, want 429", second.Code)
		}
		close(release)
		<-done
	})

	t.Run("budget before confirm", func(t *testing.T) {
		for _, tc := range []struct {
			profile string
			budget  time.Duration
		}{
			{"cpu", 60 * time.Second},
			{"trace?seconds=5", 35 * time.Second},
			{"heap", 30 * time.Second},
		} {
			t.Run(tc.profile, func(t *testing.T) {
				h := newHarness(baseTarget())
				before := time.Now()
				h.do(t, http.MethodGet, profilePath+tc.profile)
				deadline, _ := h.disc.confirmed()
				if deadline.IsZero() {
					t.Fatal("Confirm saw no deadline; the budget must start before confirmation")
				}
				want := before.Add(tc.budget)
				if diff := deadline.Sub(want); diff < -time.Second || diff > time.Second {
					t.Errorf("Confirm deadline = %v, want within 1s of %v", deadline, want)
				}
			})
		}
	})
}

func TestConfirm(t *testing.T) {
	t.Run("confirm changed", func(t *testing.T) {
		tr := newTrap(t, nil)
		h := newHarness(tr.target())
		h.disc.confirmErr = k8s.ErrTargetChanged
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		h.expectError(t, rec, http.StatusServiceUnavailable, "target_changed")
		h.expectCounts(t, 1, 0)
		if tr.hits.Load() != 0 {
			t.Errorf("trap hits = %d, want 0", tr.hits.Load())
		}
		if _, confirms, _ := h.rec.snapshot(); len(confirms) != 1 || confirms[0] != "changed" {
			t.Errorf("Recorder.Confirm calls = %v, want [changed]", confirms)
		}
	})

	t.Run("confirm unavailable", func(t *testing.T) {
		tr := newTrap(t, nil)
		h := newHarness(tr.target())
		h.disc.confirmErr = k8s.ErrDiscoveryUnavailable
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		h.expectError(t, rec, http.StatusServiceUnavailable, "discovery_unavailable")
		h.expectCounts(t, 1, 0)
		if tr.hits.Load() != 0 {
			t.Errorf("trap hits = %d, want 0", tr.hits.Load())
		}
		if _, confirms, _ := h.rec.snapshot(); len(confirms) != 1 || confirms[0] != "unavailable" {
			t.Errorf("Recorder.Confirm calls = %v, want [unavailable]", confirms)
		}
	})

	t.Run("confirm unknown error fails closed", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.disc.confirmErr = errors.New("something else")
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		h.expectError(t, rec, http.StatusServiceUnavailable, "discovery_unavailable")
		h.expectCounts(t, 1, 0)
	})

	t.Run("confirm ok", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		h.expectCounts(t, 1, 1)
		if _, confirms, _ := h.rec.snapshot(); len(confirms) != 1 || confirms[0] != "ok" {
			t.Errorf("Recorder.Confirm calls = %v, want [ok]", confirms)
		}
		if _, target := h.disc.confirmed(); target != baseTarget() {
			t.Errorf("confirmed target = %+v, want the selected one", target)
		}
	})

	t.Run("no dial on confirm failure (real proxy)", func(t *testing.T) {
		tr := newTrap(t, nil)
		h := newHarness(tr.target())
		h.upstream = proxy.New(proxy.Options{})
		h.disc.confirmErr = k8s.ErrTargetChanged
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		h.expectError(t, rec, http.StatusServiceUnavailable, "target_changed")
		if tr.hits.Load() != 0 {
			t.Errorf("trap hits = %d, want 0: the handler dialed a target it failed to confirm", tr.hits.Load())
		}
	})

	t.Run("mutation: without confirm the trap is dialed", func(t *testing.T) {
		// The row above fails under this mutation, which is what makes it worth having.
		tr := newTrap(t, nil)
		h := newHarness(tr.target())
		h.upstream = proxy.New(proxy.Options{})
		h.disc.confirmErr = k8s.ErrTargetChanged
		h.discovery = noConfirm{h.disc}
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		if rec.Code != http.StatusOK || tr.hits.Load() != 1 {
			t.Errorf("status %d, trap hits %d; want 200 and 1 under the no-confirm mutation", rec.Code, tr.hits.Load())
		}
	})

	t.Run("residual window (real proxy)", func(t *testing.T) {
		// Nothing after Confirm re-checks the address: a Pod replaced between the read and the dial is reached.
		// The spec states this as a residual risk; this row pins it.
		tr := newTrap(t, nil)
		h := newHarness(tr.target())
		h.upstream = proxy.New(proxy.Options{})
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		if rec.Code != http.StatusOK || rec.Body.String() != "trap-body" {
			t.Errorf("status %d body %q, want 200 trap-body", rec.Code, rec.Body.String())
		}
		if tr.hits.Load() != 1 {
			t.Errorf("trap hits = %d, want 1", tr.hits.Load())
		}
		if strings.Contains(rec.Body.String(), tr.host) {
			t.Errorf("body leaks the trap address")
		}
		h.expectAudit(t, http.StatusOK, "ok")
	})
}

func TestProxyOutcomes(t *testing.T) {
	t.Run("success carries target headers", func(t *testing.T) {
		h := newHarness(namedTarget("payment-api-1", ""))
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		if rec.Code != http.StatusOK || rec.Body.String() != "profile-bytes" {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		want := map[string]string{"X-Pprof-Target-Pod": "payment-api-1", "X-Pprof-Target-Node": fixtureNode, "X-Pprof-Target-Version": ""}
		for name, value := range want {
			values, ok := rec.Header()[name]
			if !ok || len(values) != 1 || values[0] != value {
				t.Errorf("%s = %v, want %q present", name, values, value)
			}
		}
		audit := h.expectAudit(t, http.StatusOK, "ok")
		if audit["pod"] != "payment-api-1" || audit["profile"] != "heap" || audit["principal"] != "anonymous" {
			t.Errorf("audit = %v", audit)
		}
		h.expectMetric(t, metrics.EndpointProfile, "heap")
		h.expectMetricCode(t, "ok")
	})

	t.Run("target headers on forwarded errors", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.up.outcome = proxy.Outcome{Code: "upstream_500", Status: http.StatusInternalServerError, Committed: true}
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if got := rec.Header().Get("X-Pprof-Target-Pod"); got != fixturePod {
			t.Errorf("X-Pprof-Target-Pod = %q, want %q", got, fixturePod)
		}
		h.expectAudit(t, http.StatusInternalServerError, "upstream_500")
		h.expectMetricCode(t, "upstream_500")
	})

	for _, tc := range []struct {
		code   string
		status int
	}{
		{"upstream_unreachable", http.StatusBadGateway},
		{"upstream_timeout", http.StatusGatewayTimeout},
		{"upstream_redirect", http.StatusBadGateway},
	} {
		t.Run("envelope for "+tc.code, func(t *testing.T) {
			h := newHarness(baseTarget())
			h.up.outcome = proxy.Outcome{Code: tc.code, Status: tc.status}
			rec := h.do(t, http.MethodGet, profilePath+"heap")
			h.expectError(t, rec, tc.status, tc.code)
			if _, msg := errorBodyOf(t, rec); !strings.Contains(msg, fixturePod) {
				t.Errorf("envelope %q does not name the Pod", msg)
			}
		})
	}

	t.Run("client gone writes nothing", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.up.outcome = proxy.Outcome{Code: "client_gone"}
		rec := h.do(t, http.MethodGet, profilePath+"heap")
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want nothing: there is nobody to write to", rec.Body.String())
		}
		h.expectAudit(t, 0, "client_gone")
		h.expectMetricCode(t, "client_gone")
	})

	t.Run("committed stream failure aborts the connection", func(t *testing.T) {
		upstream := newTrap(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			// More than net/http's write buffer, so the headers and part of the body reach the client before the cut;
			// a shorter prefix would be dropped whole with the abort.
			_, _ = w.Write(bytes.Repeat([]byte("half-of-the-body"), 4096))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("Flush() error = %v", err)
			}
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("Hijack() error = %v", err)

				return
			}
			_ = conn.Close()
		})
		h := newHarness(upstream.target())
		h.upstream = proxy.New(proxy.Options{})
		gateway := httptest.NewServer(h.handler())
		t.Cleanup(gateway.Close)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, gateway.URL+profilePath+"heap", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200: the failure came after the headers", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("ReadAll() error = %v (body %q), want io.ErrUnexpectedEOF: the client must see a truncation, not a clean end", err, body)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			records := h.audits(t)
			if len(records) == 1 {
				if records[0]["code"] != "upstream_stream_failed" {
					t.Errorf("audit code = %v, want upstream_stream_failed", records[0]["code"])
				}

				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("audit records = %d, want 1", len(records))
			}
			time.Sleep(5 * time.Millisecond)
		}
		h.expectMetricCode(t, "upstream_stream_failed")
		if _, _, inFlight := h.rec.snapshot(); inFlight != 0 {
			t.Errorf("ProfilesInFlight net = %d after the abort, want 0", inFlight)
		}
	})
}

func TestAuditAndMetrics(t *testing.T) {
	t.Run("audit never names the address on success", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.do(t, http.MethodGet, profilePath+"cpu?seconds=2")
		audit := h.expectAudit(t, http.StatusOK, "ok")
		want := map[string]any{
			"principal": "anonymous", "namespace": fixtureNamespace, "service": fixtureService,
			"pod": fixturePod, "profile": "cpu", "seconds": float64(2), "code": "ok",
		}
		for key, value := range want {
			if audit[key] != value {
				t.Errorf("audit %s = %v, want %v", key, audit[key], value)
			}
		}
		if _, ok := audit["duration_ms"].(float64); !ok {
			t.Errorf("audit duration_ms = %v, want a number", audit["duration_ms"])
		}
	})

	t.Run("audit names the route on gateway errors", func(t *testing.T) {
		h := newHarness()
		h.disc.err = k8s.ErrServiceNotFound
		h.do(t, http.MethodGet, profilePath+"heap?pod=x")
		audit := h.expectAudit(t, http.StatusNotFound, "service_not_found")
		if audit["namespace"] != fixtureNamespace || audit["service"] != fixtureService || audit["profile"] != "heap" {
			t.Errorf("audit = %v", audit)
		}
	})

	t.Run("metrics labels", func(t *testing.T) {
		for _, tc := range []struct {
			name, path string
			endpoint   metrics.Endpoint
			profile    string
			code       string
		}{
			{"targets", targetsPath, metrics.EndpointTargets, "none", "ok"},
			{"profile", profilePath + "mutex", metrics.EndpointProfile, "mutex", "ok"},
			{"unknown profile", profilePath + "nope", metrics.EndpointProfile, "none", "profile_unknown"},
			{"unknown route", "/v2/x", metrics.EndpointProfile, "none", "route_unknown"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(baseTarget())
				h.do(t, http.MethodGet, tc.path)
				h.expectMetric(t, tc.endpoint, tc.profile)
				h.expectMetricCode(t, tc.code)
			})
		}
	})
}

// TestConcurrentRequestsAreIndependent runs many successful profile requests through one handler
// and checks the slot bookkeeping and the per-request audit records hold up under -race.
func TestConcurrentRequestsAreIndependent(t *testing.T) {
	h := newHarness(baseTarget())
	handler := h.handler()
	const n = 32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
			if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	requests, _, inFlight := h.rec.snapshot()
	if len(requests) != n || inFlight != 0 {
		t.Errorf("Recorder.Request calls = %d, in flight %d; want %d and 0", len(requests), inFlight, n)
	}
	if got := len(h.audits(t)); got != n {
		t.Errorf("audit records = %d, want %d", got, n)
	}
}

// TestSharedGateLeavesRoomForInteractiveRequests proves the guarantee that
// holds only because there is one gate: configuration keeps
// pgo.limits.maxParallel × maxActiveCollections below
// limits.maxConcurrentProfiles, so however many slots a Collection's samplers
// hold, an interactive request always finds one.
// The second half is what makes the sharing observable: with one slot left,
// two requests at once take it and refuse the other, which a handler holding
// a gate of its own would not do.
// internal/pgo proves the sampler's half, that a Collection's fan-out never
// takes more than maxParallel of the same gate.
func TestSharedGateLeavesRoomForInteractiveRequests(t *testing.T) {
	const (
		capacity    = 3
		maxParallel = 2
	)

	shared := admit.New(capacity)
	h := newHarness(baseTarget())
	h.gate = shared
	h.configure(func(cfg *config.Config) { cfg.Limits.MaxConcurrentProfiles = capacity })

	// A Collection's samplers take their slots the way internal/pgo does.
	releases := make([]func(), 0, maxParallel)
	for i := range maxParallel {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		release, err := shared.Acquire(ctx)
		cancel()
		if err != nil {
			t.Fatalf("a collection sampler could not take slot %d: %v", i, err)
		}
		releases = append(releases, release)
	}

	// Every interactive request still finds the slot the inequality reserves.
	for i := range 5 {
		rec := h.do(t, http.MethodGet, profilePath+"cpu")
		if rec.Code != http.StatusOK {
			t.Fatalf("interactive request %d got %d, want 200 while a collection holds %d of %d slots",
				i, rec.Code, maxParallel, capacity)
		}
	}

	// One slot left and two requests at once: the handler admits on the gate
	// it was given and nowhere else, so exactly one of them is refused.
	block := make(chan struct{})
	h.up.release = block
	handler := h.handler()
	admitted := h.up.calls.Load()

	first := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"cpu", nil))
		first <- rec.Code
	}()

	deadline := time.Now().Add(5 * time.Second)
	for h.up.calls.Load() == admitted {
		if time.Now().After(deadline) {
			t.Fatal("the first of the two concurrent requests never reached the upstream")
		}
		time.Sleep(time.Millisecond)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"cpu", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("the second concurrent request got %d, want 429: the last slot was already taken", second.Code)
	}
	if code, _ := errorBodyOf(t, second); code != "too_many_profiles" {
		t.Errorf("the refused request answered %q, want too_many_profiles", code)
	}

	close(block)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the admitted request got %d, want 200", code)
	}

	for _, release := range releases {
		release()
	}
}

// TestPortGrammar covers the port and portName grammar on both interactive
// endpoints: every row is 400 invalid_parameter, discovery is never reached,
// and the audit port field is empty unless the selection itself was valid.
func TestPortGrammar(t *testing.T) {
	endpoints := []struct{ name, path string }{
		{"profile", profilePath + "heap?"},
		{"targets", targetsPath + "?"},
	}
	rows := []struct {
		name, query string
		auditPort   string
	}{
		{"port zero", "port=0", ""},
		{"port over", "port=65536", ""},
		{"port letters", "port=abc", ""},
		{"port signed", "port=+80", ""},
		{"port empty", "port=", ""},
		{"port repeated", "port=6060&port=6061", ""},
		{"portName empty", "portName=", ""},
		{"portName repeated", "portName=a&portName=b", ""},
		{"portName too long", "portName=" + strings.Repeat("a", 16), ""},
		{"portName uppercase", "portName=Pprof", ""},
		{"portName leading hyphen", "portName=-pprof", ""},
		{"portName trailing hyphen", "portName=pprof-", ""},
		{"portName all digits", "portName=6060", ""},
		{"portName consecutive hyphens", "portName=pp--rof", ""},
		{"both given", "port=6060&portName=pprof", ""},
		{"query malformed", "port=%zz", ""},
		{"valid port, unknown parameter", "port=6060&x=1", "6060"},
	}
	for _, ep := range endpoints {
		for _, tc := range rows {
			t.Run(ep.name+" "+tc.name, func(t *testing.T) {
				h := newHarness(baseTarget())
				rec := h.do(t, http.MethodGet, ep.path+tc.query)
				h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
				h.expectCounts(t, 0, 0)
				if got := h.audits(t)[0]["port"]; got != tc.auditPort {
					t.Errorf("audit port = %v, want %q", got, tc.auditPort)
				}
			})
		}
	}

	t.Run("valid port, invalid seconds", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, profilePath+"cpu?port=6060&seconds=abc")
		h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
		h.expectCounts(t, 0, 0)
		if got := h.audits(t)[0]["port"]; got != "6060" {
			t.Errorf("audit port = %v, want 6060: the selection was valid, only seconds was not", got)
		}
	})

	t.Run("targets seconds", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, targetsPath+"?seconds=2")
		h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
		h.expectCounts(t, 0, 0)
		if got := h.audits(t)[0]["port"]; got != "" {
			t.Errorf("audit port = %v, want empty", got)
		}
	})
}

// TestPortAllowlist covers the allowlist step: its place after the realm and
// the parameter grammar, before discovery, the snapshot it reads, and what the
// audit line and the response say about a selection.
func TestPortAllowlist(t *testing.T) {
	pprof := func(p config.PprofConfig) func(*config.Config) {
		return func(cfg *config.Config) { cfg.Discovery.Pprof = p }
	}
	expectSelections := func(t *testing.T, h *harness, want ...k8s.PortSelection) {
		t.Helper()
		if got := h.disc.selectionsSeen(); !slices.Equal(got, want) {
			t.Errorf("selections = %v, want %v", got, want)
		}
	}
	expectOK := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	}

	t.Run("portName abc is valid", func(t *testing.T) {
		h := newHarness(baseTarget())
		expectOK(t, h.do(t, http.MethodGet, targetsPath+"?portName=abc"))
		expectSelections(t, h, k8s.PortSelection{PortName: "abc"})
	})

	t.Run("empty lists accept any port", func(t *testing.T) {
		h := newHarness(baseTarget())
		expectOK(t, h.do(t, http.MethodGet, profilePath+"heap?port=9999"))
		expectSelections(t, h, k8s.PortSelection{Port: 9999})
		h.expectCounts(t, 1, 1)
	})

	t.Run("disallowed port", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}))
		rec := h.do(t, http.MethodGet, profilePath+"heap?port=6061")
		h.expectError(t, rec, http.StatusBadRequest, "port_not_allowed")
		h.expectCounts(t, 0, 0)
		_, message := errorBodyOf(t, rec)
		if !strings.Contains(message, "6061") {
			t.Errorf("message %q does not name the value sent", message)
		}
		for _, other := range []string{"6060", "8080", "9999"} {
			if strings.Contains(rec.Body.String(), other) {
				t.Errorf("body %q names a number the client did not send", rec.Body.String())
			}
		}
	})

	t.Run("disallowed name", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPortNames: []string{"pprof"}}))
		rec := h.do(t, http.MethodGet, targetsPath+"?portName=pprof-alt")
		h.expectError(t, rec, http.StatusBadRequest, "port_not_allowed")
		h.expectCounts(t, 0, 0)
		if _, message := errorBodyOf(t, rec); !strings.Contains(message, "pprof-alt") {
			t.Errorf("message %q does not name the value sent", message)
		}
	})

	t.Run("lists are independent", func(t *testing.T) {
		t.Run("ports bound, any name", func(t *testing.T) {
			h := newHarness(baseTarget())
			h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}))
			expectOK(t, h.do(t, http.MethodGet, targetsPath+"?portName=anything"))
		})
		t.Run("names bound, any port", func(t *testing.T) {
			h := newHarness(baseTarget())
			h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPortNames: []string{"pprof"}}))
			expectOK(t, h.do(t, http.MethodGet, profilePath+"heap?port=9999"))
		})
	})

	t.Run("default number always passes", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{7070}}))
		expectOK(t, h.do(t, http.MethodGet, targetsPath+"?port=6060"))
	})

	t.Run("default name always passes", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{PortName: "pprof", AllowedPortNames: []string{"metrics"}}))
		expectOK(t, h.do(t, http.MethodGet, targetsPath+"?portName=pprof"))
	})

	narrowed := pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{7070}, AllowedPortNames: []string{"metrics"}})

	t.Run("no selection under narrowed lists, targets", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(narrowed)
		expectOK(t, h.do(t, http.MethodGet, targetsPath))
		expectSelections(t, h, k8s.PortSelection{})
	})

	t.Run("no selection under narrowed lists, profile", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(narrowed)
		expectOK(t, h.do(t, http.MethodGet, profilePath+"heap"))
		h.expectCounts(t, 1, 1)
	})

	t.Run("allowlist reads the request's snapshot", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060, 7070}}))
		h.beforeAllowlist = func() {
			cfg := testConfig()
			cfg.Discovery.Pprof = config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}
			h.cfg.Store(cfg)
		}
		rec := h.do(t, http.MethodGet, profilePath+"heap?port=7070")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: the request must finish under the config it loaded (body %q)", rec.Code, rec.Body.String())
		}
		expectSelections(t, h, k8s.PortSelection{Port: 7070})
	})

	t.Run("realm before allowlist", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.Realms["developer"] = config.Realm{Namespaces: []string{"other"}, Services: []string{"*"}, Profiles: []string{"*"}}
			cfg.Discovery.Pprof = config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}
		})
		rec := h.do(t, http.MethodGet, profilePath+"heap?port=6061")
		h.expectError(t, rec, http.StatusForbidden, "realm_denied")
		h.expectCounts(t, 0, 0)
	})

	t.Run("grammar before allowlist", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}))
		rec := h.do(t, http.MethodGet, targetsPath+"?port=abc")
		h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
	})

	t.Run("allowlist before discovery error", func(t *testing.T) {
		h := newHarness()
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}))
		h.disc.err = k8s.ErrServiceNotFound
		rec := h.do(t, http.MethodGet, targetsPath+"?port=6061")
		h.expectError(t, rec, http.StatusBadRequest, "port_not_allowed")
		h.expectCounts(t, 0, 0)
	})

	t.Run("parameters before allowlist", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(pprof(config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}))
		rec := h.do(t, http.MethodGet, profilePath+"cpu?port=6061&seconds=61")
		h.expectError(t, rec, http.StatusBadRequest, "seconds_exceeds_limit")
		h.expectCounts(t, 0, 0)
	})
}

// TestPortResolution covers what a selection does past the allowlist: the
// per-Pod name resolution the fake models, the audit field, the metric
// labels, the pass-through of a non-pprof listener, and that nothing the
// gateway writes carries the number a name resolved to.
func TestPortResolution(t *testing.T) {
	const altPort = 6061
	altTarget := func(pod string) k8s.Target {
		t := namedTarget(pod, fixtureVersion)
		t.Port = altPort

		return t
	}
	// altHarness answers pprof-alt with pod-1 at 6061 and nothing else.
	altHarness := func() *harness {
		h := newHarness(namedTarget("pod-1", fixtureVersion), namedTarget("pod-2", fixtureVersion))
		h.disc.byName = map[string][]k8s.Target{"pprof-alt": {altTarget("pod-1")}}

		return h
	}
	auditPort := func(t *testing.T, h *harness, status int, code, want string) {
		t.Helper()
		audit := h.expectAudit(t, status, code)
		if audit["port"] != want {
			t.Errorf("audit port = %v, want %q", audit["port"], want)
		}
	}
	targetPods := func(t *testing.T, rec *httptest.ResponseRecorder) []string {
		t.Helper()
		var body struct {
			Targets []map[string]string `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		pods := make([]string, 0, len(body.Targets))
		for _, target := range body.Targets {
			pods = append(pods, target["pod"])
		}

		return pods
	}

	t.Run("name on one Pod of two", func(t *testing.T) {
		h := altHarness()
		rec := h.do(t, http.MethodGet, targetsPath+"?portName=pprof-alt")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if pods := targetPods(t, rec); !slices.Equal(pods, []string{"pod-1"}) {
			t.Errorf("targets = %v, want [pod-1]", pods)
		}
	})

	t.Run("name on no Pod", func(t *testing.T) {
		t.Run("targets", func(t *testing.T) {
			h := altHarness()
			rec := h.do(t, http.MethodGet, targetsPath+"?portName=nowhere")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"targets":[]`) {
				t.Errorf("status %d body %q, want 200 with an empty list", rec.Code, rec.Body.String())
			}
		})
		t.Run("profile", func(t *testing.T) {
			h := altHarness()
			rec := h.do(t, http.MethodGet, profilePath+"heap?portName=nowhere")
			h.expectError(t, rec, http.StatusServiceUnavailable, "no_targets")
			h.expectCounts(t, 1, 0)
		})
	})

	t.Run("audit absent", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.do(t, http.MethodGet, profilePath+"heap")
		auditPort(t, h, http.StatusOK, codeOK, "")
	})

	t.Run("audit numeric", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.do(t, http.MethodGet, profilePath+"heap?port=6061")
		auditPort(t, h, http.StatusOK, codeOK, "6061")
	})

	t.Run("audit name", func(t *testing.T) {
		h := altHarness()
		h.do(t, http.MethodGet, profilePath+"heap?portName=pprof-alt")
		auditPort(t, h, http.StatusOK, codeOK, "pprof-alt")
		if strings.Contains(h.logs.String(), strconv.Itoa(altPort)) {
			t.Errorf("audit line names the resolved port: %s", h.logs.String())
		}
	})

	t.Run("audit malformed", func(t *testing.T) {
		for _, query := range []string{"port=abc", "port=1&port=2", "port=1&portName=a"} {
			t.Run(query, func(t *testing.T) {
				h := newHarness(baseTarget())
				h.do(t, http.MethodGet, profilePath+"heap?"+query)
				auditPort(t, h, http.StatusBadRequest, "invalid_parameter", "")
			})
		}
	})

	t.Run("audit disallowed", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.Discovery.Pprof = config.PprofConfig{Port: 6060, AllowedPorts: []int32{6060}}
		})
		h.do(t, http.MethodGet, profilePath+"heap?port=6061")
		auditPort(t, h, http.StatusBadRequest, "port_not_allowed", "6061")
	})

	t.Run("no metric label", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.do(t, http.MethodGet, profilePath+"heap?port=6061")
		h.expectMetric(t, metrics.EndpointProfile, "heap")
	})

	t.Run("no number in the response", func(t *testing.T) {
		leak := strconv.Itoa(altPort)
		check := func(t *testing.T, rec *httptest.ResponseRecorder) {
			t.Helper()
			for name, values := range rec.Header() {
				for _, v := range values {
					if strings.Contains(v, leak) {
						t.Errorf("header %s carries the resolved port: %q", name, v)
					}
				}
			}
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("body carries the resolved port: %q", rec.Body.String())
			}
		}
		t.Run("targets body", func(t *testing.T) {
			h := altHarness()
			check(t, h.do(t, http.MethodGet, targetsPath+"?portName=pprof-alt"))
		})
		t.Run("profile headers", func(t *testing.T) {
			h := altHarness()
			rec := h.do(t, http.MethodGet, profilePath+"heap?portName=pprof-alt")
			if rec.Code != http.StatusOK || rec.Header().Get("X-Pprof-Target-Pod") != "pod-1" {
				t.Fatalf("status %d headers %v", rec.Code, rec.Header())
			}
			check(t, rec)
		})
		t.Run("gateway errors", func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				setup func(h *harness)
				path  string
			}{
				{"pod not found", func(*harness) {}, profilePath + "heap?portName=pprof-alt&pod=pod-2"},
				{"confirm changed", func(h *harness) { h.disc.confirmErr = k8s.ErrTargetChanged }, profilePath + "heap?portName=pprof-alt"},
				{"upstream unreachable", func(h *harness) {
					h.up.outcome = proxy.Outcome{Code: "upstream_unreachable", Status: http.StatusBadGateway}
				}, profilePath + "heap?portName=pprof-alt"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					h := altHarness()
					tc.setup(h)
					rec := h.do(t, http.MethodGet, tc.path)
					if rec.Code < http.StatusBadRequest {
						t.Fatalf("status = %d, want an error", rec.Code)
					}
					check(t, rec)
				})
			}
		})
	})

	t.Run("non-pprof listener passes through", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.up.outcome = proxy.Outcome{Code: "upstream_404", Status: http.StatusNotFound, Committed: true}
		rec := h.do(t, http.MethodGet, profilePath+"heap?port=8080")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		auditPort(t, h, http.StatusNotFound, "upstream_404", "8080")
		expectSel := k8s.PortSelection{Port: 8080}
		if seen := h.disc.selectionsSeen(); len(seen) != 1 || seen[0] != expectSel {
			t.Errorf("selections = %v, want [%v]", seen, expectSel)
		}
	})
}
