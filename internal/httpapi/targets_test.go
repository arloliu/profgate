package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
)

func TestWriteTargets(t *testing.T) {
	t.Run("sorted, address-free, version always present", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeTargets(rec, "payment", "payment-api", []k8s.Target{
			{Pod: "b", Node: "n2", Version: "2", PodIP: "10.0.0.5", Port: 6060, UID: "u2"},
			{Pod: "a", Node: "n1", PodIP: "10.0.0.6", Port: 6060, UID: "u1"},
		})
		want := `{"namespace":"payment","service":"payment-api","targets":[` +
			`{"pod":"a","node":"n1","version":""},{"pod":"b","node":"n2","version":"2"}]}` + "\n"
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("status %d body %q, want 200 %q", rec.Code, rec.Body.String(), want)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	})

	t.Run("nil is an empty array", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeTargets(rec, "payment", "payment-api", nil)
		want := `{"namespace":"payment","service":"payment-api","targets":[]}` + "\n"
		if rec.Body.String() != want {
			t.Errorf("body %q, want %q", rec.Body.String(), want)
		}
	})

	t.Run("input order is untouched", func(t *testing.T) {
		targets := []k8s.Target{{Pod: "b"}, {Pod: "a"}}
		writeTargets(httptest.NewRecorder(), "payment", "payment-api", targets)
		if targets[0].Pod != "b" {
			t.Error("writeTargets sorted the caller's slice in place")
		}
	})
}

const (
	// droppedPod is the target every pod= row drops; its name appears in no other fixture value,
	// so a byte scan that finds it has found a leak.
	droppedPod     = "zz-dropped-sentinel-pod"
	droppedVersion = "9.9"
)

// explainFixture is the seam's answer for the explain rows:
// three targets carrying the fixture address, and two cache-derived reasons.
// SelectorMatched is the target count plus the sum of the counts, as the seam promises.
func explainFixture() k8s.Explanation {
	return k8s.Explanation{
		Targets: []k8s.Target{
			namedTarget("payment-api-2", "2.0"),
			namedTarget(droppedPod, droppedVersion),
			namedTarget("payment-api-1", "1.0"),
		},
		SelectorMatched: 6,
		Excluded: []k8s.Exclusion{
			{Reason: k8s.ReasonPodNotReady, Count: 2},
			{Reason: k8s.ReasonPortNameNotDeclared, Count: 1},
		},
	}
}

// explainHarness is a harness whose Targets and Explain answer from the same three targets.
func explainHarness() *harness {
	ex := explainFixture()
	h := newHarness(ex.Targets...)
	h.disc.explanation = ex

	return h
}

// explainResponse is the explain body as decoded for assertions.
type explainResponse struct {
	Namespace       string          `json:"namespace"`
	Service         string          `json:"service"`
	Targets         []targetView    `json:"targets"`
	SelectorMatched int             `json:"selectorMatched"`
	Excluded        []exclusionView `json:"excluded"`
	Raw             map[string]any  `json:"-"`
}

// decodeExplain decodes a 200 body and checks the sum invariant of the spec.
func decodeExplain(t *testing.T, rec *httptest.ResponseRecorder) explainResponse {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body explainResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body.Raw); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	for _, field := range []string{"serviceFound", "cacheSynced"} {
		if _, ok := body.Raw[field]; ok {
			t.Errorf("body carries %q: %s", field, rec.Body.String())
		}
	}
	if _, ok := body.Raw["excluded"]; ok {
		sum := len(body.Targets)
		for _, ex := range body.Excluded {
			sum += ex.Count
		}
		if sum != body.SelectorMatched {
			t.Errorf("selectorMatched = %d, want targets + counts = %d: %s", body.SelectorMatched, sum, rec.Body.String())
		}
		if !slices.IsSortedFunc(body.Excluded, func(a, b exclusionView) int {
			return slices.Index(k8s.ExclusionReasons(), a.Reason) - slices.Index(k8s.ExclusionReasons(), b.Reason)
		}) {
			t.Errorf("excluded is not in vocabulary order: %v", body.Excluded)
		}
	}

	return body
}

// podNames is the pod of every target in order.
func podNames(views []targetView) []string {
	names := make([]string, 0, len(views))
	for _, v := range views {
		names = append(names, v.Pod)
	}

	return names
}

// plainTargetsBody is today's body for the three fixture targets, sorted.
const plainTargetsBody = `{"namespace":"payment","service":"payment-api","targets":[` +
	`{"pod":"payment-api-1","node":"worker-1","version":"1.0"},` +
	`{"pod":"payment-api-2","node":"worker-1","version":"2.0"},` +
	`{"pod":"` + droppedPod + `","node":"worker-1","version":"` + droppedVersion + `"}]}` + "\n"

// TestTargetsGrammar restates the parameter table of the spec's targets endpoint:
// every fault is 400 invalid_parameter, the first in name order is the one reported,
// and the answer does not depend on the order the query terms were sent in.
func TestTargetsGrammar(t *testing.T) {
	rows := []struct {
		name  string
		terms []string
		// faulty is the parameter the message must name.
		faulty string
	}{
		{"explain=1", []string{"explain=1"}, "explain"},
		{"explain=TRUE", []string{"explain=TRUE"}, "explain"},
		{"explain=yes", []string{"explain=yes"}, "explain"},
		{"explain repeated", []string{"explain=true", "explain=false"}, "explain"},
		{"explain empty", []string{"explain="}, "explain"},
		{"pod repeated", []string{"pod=a", "pod=b"}, "pod"},
		{"version repeated", []string{"version=1", "version=2"}, "version"},
		{"port repeated", []string{"port=6060", "port=6061"}, "port"},
		{"portName repeated", []string{"portName=a", "portName=b"}, "portName"},
		{"pod empty", []string{"pod="}, "pod"},
		{"version empty", []string{"version="}, "version"},
		{"port empty", []string{"port="}, "port"},
		{"portName empty", []string{"portName="}, "portName"},
		{"pod outside DNS-1123", []string{"pod=BAD_Pod"}, "pod"},
		{"port zero", []string{"port=0"}, "port"},
		{"port over", []string{"port=65536"}, "port"},
		{"port signed", []string{"port=%2B1"}, "port"},
		{"port trailing space", []string{"port=6060%20"}, "port"},
		{"explain before port", []string{"explain=yes", "port=bad"}, "explain"},
		{"pod before version", []string{"pod=BAD", "version="}, "pod"},
		{"port before version", []string{"port=bad", "version="}, "port"},
		{"portName before version", []string{"portName=BAD", "version="}, "portName"},
		{"unknown before version", []string{"unknown=1", "version="}, "unknown"},
		{"port and portName", []string{"port=6061", "portName=pprof"}, "portName"},
		{"explain before the pair", []string{"port=6061", "portName=pprof", "explain=yes"}, "explain"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			reversed := slices.Clone(tc.terms)
			slices.Reverse(reversed)
			var codes, messages []string
			for _, terms := range [][]string{tc.terms, reversed} {
				h := explainHarness()
				rec := h.do(t, http.MethodGet, targetsPath+"?"+strings.Join(terms, "&"))
				h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
				h.expectCounts(t, 0, 0)
				if got := h.disc.explainCalls.Load(); got != 0 {
					t.Errorf("Explain calls = %d, want 0", got)
				}
				code, message := errorBodyOf(t, rec)
				codes = append(codes, code)
				messages = append(messages, message)
			}
			if codes[0] != codes[1] || messages[0] != messages[1] {
				t.Errorf("answer depends on term order: %q/%q vs %q/%q", codes[0], messages[0], codes[1], messages[1])
			}
			if !strings.Contains(messages[0], tc.faulty) {
				t.Errorf("message %q does not name %q", messages[0], tc.faulty)
			}
		})
	}

	t.Run("every parameter accepted at once", func(t *testing.T) {
		h := explainHarness()
		h.configure(func(cfg *config.Config) {
			cfg.Discovery.Pprof.AllowedSelections = []config.Selection{{Kind: config.SelectionPort, Value: "6061"}}
		})
		rec := h.do(t, http.MethodGet, targetsPath+"?version=1.0&pod=payment-api-1&explain=true&port=6061")
		body := decodeExplain(t, rec)
		if got := podNames(body.Targets); !slices.Equal(got, []string{"payment-api-1"}) {
			t.Errorf("targets = %v, want payment-api-1 alone", got)
		}
		if got := h.disc.explainSelectionsSeen(); !slices.Equal(got, []k8s.PortSelection{{Port: 6061}}) {
			t.Errorf("Explain selections = %v, want port 6061", got)
		}
		h.expectCounts(t, 0, 0)
	})

	t.Run("a port the allowlist refuses never reaches discovery", func(t *testing.T) {
		h := explainHarness()
		rec := h.do(t, http.MethodGet, targetsPath+"?explain=true&port=6061")
		h.expectError(t, rec, http.StatusBadRequest, "port_not_allowed")
		h.expectCounts(t, 0, 0)
		if got := h.disc.explainCalls.Load(); got != 0 {
			t.Errorf("Explain calls = %d, want 0", got)
		}
	})

	t.Run("audit port over failing rows", func(t *testing.T) {
		rows := []struct{ query, port string }{
			{"explain=yes&port=6061", "6061"},
			{"explain=yes&portName=pprof", "pprof"},
			{"explain=yes&port=bad", ""},
			{"explain=yes&port=6061&portName=pprof", ""},
			{"explain=yes", ""},
		}
		for _, tc := range rows {
			t.Run(tc.query, func(t *testing.T) {
				h := explainHarness()
				rec := h.do(t, http.MethodGet, targetsPath+"?"+tc.query)
				h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
				if got := h.audits(t)[0]["port"]; got != tc.port {
					t.Errorf("audit port = %v, want %q", got, tc.port)
				}
			})
		}
	})
}

// TestTargetsBodies covers the plain body, the explain body, and the two filters on each.
func TestTargetsBodies(t *testing.T) {
	t.Run("explain=true carries the counts", func(t *testing.T) {
		h := explainHarness()
		rec := h.do(t, http.MethodGet, targetsPath+"?explain=true")
		want := strings.TrimSuffix(plainTargetsBody, "}\n") +
			`,"selectorMatched":6,"excluded":[{"reason":"pod_not_ready","count":2},{"reason":"port_name_not_declared","count":1}]}` + "\n"
		if rec.Body.String() != want {
			t.Errorf("body %q, want %q", rec.Body.String(), want)
		}
		decodeExplain(t, rec)
		if got := h.disc.explainCalls.Load(); got != 1 {
			t.Errorf("Explain calls = %d, want 1", got)
		}
		h.expectCounts(t, 0, 0)
		audit := h.expectAudit(t, http.StatusOK, "ok")
		if got := audit["explain"]; got != true {
			t.Errorf("audit explain = %v, want true", got)
		}
		h.expectMetric(t, metrics.EndpointTargets, "none")
	})

	for _, query := range []string{"?explain=false", ""} {
		t.Run("today's body under "+strconv.Quote(query), func(t *testing.T) {
			h := explainHarness()
			rec := h.do(t, http.MethodGet, targetsPath+query)
			if rec.Code != http.StatusOK || rec.Body.String() != plainTargetsBody {
				t.Errorf("status %d body %q, want 200 %q", rec.Code, rec.Body.String(), plainTargetsBody)
			}
			if got := h.disc.explainCalls.Load(); got != 0 {
				t.Errorf("Explain calls = %d, want 0", got)
			}
			h.expectCounts(t, 1, 0)
			audit := h.expectAudit(t, http.StatusOK, "ok")
			if _, ok := audit["explain"]; ok {
				t.Errorf("audit carries explain: %v", audit)
			}
			h.expectMetric(t, metrics.EndpointTargets, "none")
		})
	}

	t.Run("every Pod a target", func(t *testing.T) {
		h := explainHarness()
		h.disc.explanation.SelectorMatched = 3
		h.disc.explanation.Excluded = nil
		rec := h.do(t, http.MethodGet, targetsPath+"?explain=true")
		body := decodeExplain(t, rec)
		if body.SelectorMatched != 3 || !strings.Contains(rec.Body.String(), `"excluded":[]`) {
			t.Errorf("body %q, want selectorMatched 3 and excluded []", rec.Body.String())
		}
	})

	t.Run("selector matches no Pod", func(t *testing.T) {
		h := newHarness()
		rec := h.do(t, http.MethodGet, targetsPath+"?explain=true")
		want := `{"namespace":"payment","service":"payment-api","targets":[],"selectorMatched":0,"excluded":[]}` + "\n"
		if rec.Body.String() != want {
			t.Errorf("body %q, want %q", rec.Body.String(), want)
		}
		decodeExplain(t, rec)
	})

	filterRows := []struct {
		name, query string
		targets     []string
		excluded    []exclusionView
	}{
		{"version", "version=1.0", []string{"payment-api-1"}, []exclusionView{
			{k8s.ReasonPodNotReady, 2}, {k8s.ReasonPortNameNotDeclared, 1}, {k8s.ReasonVersionMismatch, 2},
		}},
		{"pod", "pod=payment-api-1", []string{"payment-api-1"}, []exclusionView{
			{k8s.ReasonPodNotReady, 2}, {k8s.ReasonPortNameNotDeclared, 1}, {k8s.ReasonPodNameMismatch, 2},
		}},
		{"both", "version=2.0&pod=nobody", []string{}, []exclusionView{
			{k8s.ReasonPodNotReady, 2}, {k8s.ReasonPortNameNotDeclared, 1},
			{k8s.ReasonVersionMismatch, 2}, {k8s.ReasonPodNameMismatch, 1},
		}},
		{"pod naming no target", "pod=nobody", []string{}, []exclusionView{
			{k8s.ReasonPodNotReady, 2}, {k8s.ReasonPortNameNotDeclared, 1}, {k8s.ReasonPodNameMismatch, 3},
		}},
		{"version matching no target", "version=0.0", []string{}, []exclusionView{
			{k8s.ReasonPodNotReady, 2}, {k8s.ReasonPortNameNotDeclared, 1}, {k8s.ReasonVersionMismatch, 3},
		}},
	}
	for _, tc := range filterRows {
		t.Run("explain with "+tc.name, func(t *testing.T) {
			h := explainHarness()
			rec := h.do(t, http.MethodGet, targetsPath+"?explain=true&"+tc.query)
			body := decodeExplain(t, rec)
			if got := podNames(body.Targets); !slices.Equal(got, tc.targets) {
				t.Errorf("targets = %v, want %v", got, tc.targets)
			}
			if !slices.Equal(body.Excluded, tc.excluded) {
				t.Errorf("excluded = %v, want %v", body.Excluded, tc.excluded)
			}
			if body.SelectorMatched != 6 {
				t.Errorf("selectorMatched = %d, want 6", body.SelectorMatched)
			}
			h.expectAudit(t, http.StatusOK, "ok")
		})
		t.Run("plain with "+tc.name, func(t *testing.T) {
			h := explainHarness()
			rec := h.do(t, http.MethodGet, targetsPath+"?"+tc.query)
			body := decodeExplain(t, rec)
			if got := podNames(body.Targets); !slices.Equal(got, tc.targets) {
				t.Errorf("targets = %v, want %v", got, tc.targets)
			}
			for _, field := range []string{"selectorMatched", "excluded"} {
				if _, ok := body.Raw[field]; ok {
					t.Errorf("plain body carries %q: %s", field, rec.Body.String())
				}
			}
			if strings.Contains(rec.Body.String(), `"targets":[]`) != (len(tc.targets) == 0) {
				t.Errorf("empty targets must encode as []: %s", rec.Body.String())
			}
			h.expectAudit(t, http.StatusOK, "ok")
		})
	}

	t.Run("both bodies carry the same targets array", func(t *testing.T) {
		plain := explainHarness().do(t, http.MethodGet, targetsPath)
		explained := explainHarness().do(t, http.MethodGet, targetsPath+"?explain=true")
		cut := func(body string) string {
			start := strings.Index(body, `"targets":`)
			end := strings.Index(body, `}]`)
			if start < 0 || end < 0 {
				t.Fatalf("no targets array in %q", body)
			}

			return body[start : end+2]
		}
		if cut(plain.Body.String()) != cut(explained.Body.String()) {
			t.Errorf("targets differ:\n%s\n%s", plain.Body.String(), explained.Body.String())
		}
	})
}

// TestTargetsNonDisclosure scans the raw response, every header, and the audit output for the sentinels a leak would print.
func TestTargetsNonDisclosure(t *testing.T) {
	sentinels := []string{droppedPod, droppedVersion, fixtureIP, strconv.Itoa(fixturePort)}
	for _, query := range []string{"&explain=true", "&explain=false", ""} {
		t.Run(strconv.Quote(query), func(t *testing.T) {
			h := explainHarness()
			rec := h.do(t, http.MethodGet, targetsPath+"?pod=payment-api-1"+query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
			}
			for _, leak := range sentinels {
				if bytes.Contains(rec.Body.Bytes(), []byte(leak)) {
					t.Errorf("body leaks %q: %s", leak, rec.Body.String())
				}
				for name, values := range rec.Header() {
					// The request id is random hex, or the caller's own value echoed back,
					// so it discloses nothing and a bare number can land inside it by chance.
					if http.CanonicalHeaderKey(name) == requestIDHeader {
						continue
					}
					for _, v := range values {
						if strings.Contains(name, leak) || strings.Contains(v, leak) {
							t.Errorf("header %s leaks %q: %q", name, leak, v)
						}
					}
				}
				if logs := h.logText(t); strings.Contains(logs, leak) {
					t.Errorf("audit output leaks %q: %s", leak, logs)
				}
			}
			audit := h.expectAudit(t, http.StatusOK, "ok")
			if audit["pod"] != "" {
				t.Errorf("audit pod = %v, want empty for a targets request", audit["pod"])
			}
			if _, ok := audit["version"]; ok {
				t.Errorf("audit carries version: %v", audit)
			}
		})
	}
}

// TestTargetsExplainSteps covers the step order around explain and the errors the seam can return.
func TestTargetsExplainSteps(t *testing.T) {
	denyNamespace := func(cfg *config.Config) {
		cfg.Realms["developer"] = config.Realm{Namespaces: []string{"billing"}, Services: []string{"*"}, Profiles: []string{"*"}}
	}
	expectNoDiscovery := func(t *testing.T, h *harness) {
		t.Helper()
		if got := h.disc.explainCalls.Load(); got != 0 {
			t.Errorf("Explain calls = %d, want 0", got)
		}
		h.expectCounts(t, 0, 0)
	}

	t.Run("realm denied, whether or not the Service exists", func(t *testing.T) {
		exists := explainHarness()
		exists.configure(denyNamespace)
		existsRec := exists.do(t, http.MethodGet, targetsPath+"?explain=true")
		exists.expectError(t, existsRec, http.StatusForbidden, "realm_denied")
		expectNoDiscovery(t, exists)

		missing := newHarness()
		missing.configure(denyNamespace)
		missing.disc.err = k8s.ErrServiceNotFound
		missing.disc.explainErr = k8s.ErrServiceNotFound
		rec := missing.do(t, http.MethodGet, targetsPath+"?explain=true")
		missing.expectError(t, rec, http.StatusForbidden, "realm_denied")
		expectNoDiscovery(t, missing)
		if rec.Body.String() != existsRec.Body.String() {
			t.Errorf("denial body differs by Service existence: %q vs %q", rec.Body.String(), existsRec.Body.String())
		}
	})

	t.Run("not ready", func(t *testing.T) {
		h := explainHarness()
		h.disc.synced = false
		rec := h.do(t, http.MethodGet, targetsPath+"?explain=true")
		h.expectError(t, rec, http.StatusServiceUnavailable, "not_ready")
		if strings.Contains(rec.Body.String(), "selectorMatched") {
			t.Errorf("not_ready wrote an explain body: %s", rec.Body.String())
		}
		expectNoDiscovery(t, h)
	})

	errorRows := []struct {
		err    error
		status int
		code   string
	}{
		{errors.New("lister failed"), http.StatusServiceUnavailable, "discovery_unavailable"},
		{k8s.ErrServiceNotFound, http.StatusNotFound, "service_not_found"},
		{k8s.ErrServiceSelectorless, http.StatusUnprocessableEntity, "service_selectorless"},
	}
	for _, tc := range errorRows {
		t.Run("Explain returns "+tc.code, func(t *testing.T) {
			h := explainHarness()
			h.disc.explainErr = tc.err
			rec := h.do(t, http.MethodGet, targetsPath+"?explain=true")
			h.expectError(t, rec, tc.status, tc.code)
			if got := h.disc.explainCalls.Load(); got != 1 {
				t.Errorf("Explain calls = %d, want 1", got)
			}
			h.expectCounts(t, 0, 0)
		})
		t.Run("Targets returns "+tc.code, func(t *testing.T) {
			h := explainHarness()
			h.disc.err = tc.err
			rec := h.do(t, http.MethodGet, targetsPath)
			h.expectError(t, rec, tc.status, tc.code)
			h.expectCounts(t, 1, 0)
		})
	}

	t.Run("explain=notabool writes no explain", func(t *testing.T) {
		h := explainHarness()
		rec := h.do(t, http.MethodGet, targetsPath+"?explain=notabool")
		h.expectError(t, rec, http.StatusBadRequest, "invalid_parameter")
		if _, ok := h.audits(t)[0]["explain"]; ok {
			t.Errorf("audit carries explain: %v", h.audits(t)[0])
		}
	})
}
