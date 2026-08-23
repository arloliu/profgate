package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	fixtureNamespace = "payment"
	fixtureService   = "payment-api"
	fixturePod       = "payment-api-1"
	fixtureIP        = "10.0.0.5"
	fixturePort      = 6060
	fixtureNode      = "worker-1"
	fixtureVersion   = "1.0"
	fixtureUID       = "u1"

	targetsPath = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/targets"
	profilePath = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/profiles/"
)

// baseTarget is the one backend most rows resolve to; its address must never appear in a response.
func baseTarget() k8s.Target {
	return k8s.Target{
		Namespace: fixtureNamespace, Service: fixtureService, Pod: fixturePod, Node: fixtureNode,
		PodIP: fixtureIP, Port: fixturePort, Version: fixtureVersion, UID: fixtureUID,
	}
}

// namedTarget is baseTarget under another Pod name and version.
func namedTarget(pod, version string) k8s.Target {
	t := baseTarget()
	t.Pod = pod
	t.Version = version
	t.UID = "uid-" + pod

	return t
}

// testConfig is a valid wide-open configuration with the spec's default limits.
func testConfig() *config.Config {
	return &config.Config{
		Limits: config.LimitsConfig{CPUSeconds: 60, TraceSeconds: 60, MaxConcurrentProfiles: 16},
		Auth:   config.AuthConfig{Mode: "disabled", AnonymousRealm: "developer"},
		Realms: map[string]config.Realm{
			"developer": {Namespaces: []string{"*"}, Services: []string{"*"}, Profiles: []string{"*"}},
		},
	}
}

// fakeDiscovery is a Discovery whose answers the test sets up front.
type fakeDiscovery struct {
	targets    []k8s.Target
	err        error
	synced     bool
	confirmErr error
	onTargets  func()

	targetsCalls atomic.Int32
	confirmCalls atomic.Int32

	mu              sync.Mutex
	confirmDeadline time.Time
	confirmTarget   k8s.Target
}

func (f *fakeDiscovery) Targets(context.Context, string, string) ([]k8s.Target, error) {
	f.targetsCalls.Add(1)
	if f.onTargets != nil {
		f.onTargets()
	}
	if f.err != nil {
		return nil, f.err
	}

	return f.targets, nil
}

func (f *fakeDiscovery) HasSynced() bool { return f.synced }

func (f *fakeDiscovery) Confirm(ctx context.Context, t k8s.Target) error {
	f.confirmCalls.Add(1)
	f.mu.Lock()
	f.confirmDeadline, _ = ctx.Deadline()
	f.confirmTarget = t
	f.mu.Unlock()

	return f.confirmErr
}

func (f *fakeDiscovery) confirmed() (time.Time, k8s.Target) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.confirmDeadline, f.confirmTarget
}

// noConfirm is the mutation the trap rows guard against: a Discovery whose Confirm never refuses.
type noConfirm struct{ k8s.Discovery }

func (noConfirm) Confirm(context.Context, k8s.Target) error { return nil }

// fakeUpstream records what reaches it and answers with a configured Outcome.
type fakeUpstream struct {
	outcome proxy.Outcome
	// release, when set, blocks Do until it is closed or the context ends.
	release <-chan struct{}

	calls atomic.Int32
	mu    sync.Mutex
	reqs  []proxy.Request
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{outcome: proxy.Outcome{Code: "ok", Status: http.StatusOK, Committed: true}}
}

func (f *fakeUpstream) Do(ctx context.Context, w http.ResponseWriter, req proxy.Request) proxy.Outcome {
	f.calls.Add(1)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
		}
	}
	if f.outcome.Committed {
		for name, value := range req.TargetHeaders {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(f.outcome.Status)
		_, _ = w.Write([]byte("profile-bytes"))
	}

	return f.outcome
}

func (f *fakeUpstream) requests() []proxy.Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]proxy.Request(nil), f.reqs...)
}

// requestCall is one Recorder.Request observation.
type requestCall struct {
	endpoint metrics.Endpoint
	profile  string
	code     string
}

// recorder is a Recorder that remembers every call.
type recorder struct {
	mu       sync.Mutex
	requests []requestCall
	confirms []string
	inFlight int
	peak     int
}

func (r *recorder) Request(endpoint metrics.Endpoint, profile, code string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, requestCall{endpoint, profile, code})
}

func (r *recorder) Confirm(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.confirms = append(r.confirms, result)
}

func (r *recorder) ProfilesInFlight(delta int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight += delta
	r.peak = max(r.peak, r.inFlight)
}

func (r *recorder) DiscoverySynced(bool) {}

func (r *recorder) snapshot() ([]requestCall, []string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]requestCall(nil), r.requests...), append([]string(nil), r.confirms...), r.inFlight
}

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// trap is an httptest.Server that counts every connection it receives; rows that must not dial
// use its address as the target's PodIP and Port.
type trap struct {
	server *httptest.Server
	hits   atomic.Int32
	host   string
	port   int32
}

func newTrap(t *testing.T, handler http.HandlerFunc) *trap {
	t.Helper()

	tr := &trap{}
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("trap-body"))
		}
	}
	tr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(tr.server.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(tr.server.URL, "http://"))
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", tr.server.URL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q) error = %v", portStr, err)
	}
	tr.host = host
	tr.port = int32(port) //nolint:gosec // an ephemeral port fits int32

	return tr
}

// target is baseTarget with the trap as its address.
func (tr *trap) target() k8s.Target {
	t := baseTarget()
	t.PodIP = tr.host
	t.Port = tr.port

	return t
}

// harness wires one handler over the fakes; every subtest builds its own.
type harness struct {
	disc      *fakeDiscovery
	up        *fakeUpstream
	rec       *recorder
	logs      *syncBuffer
	cfg       atomic.Pointer[config.Config]
	choose    func(int) int
	discovery k8s.Discovery // overrides disc when set
	upstream  Upstream      // overrides up when set
}

func newHarness(targets ...k8s.Target) *harness {
	h := &harness{
		disc: &fakeDiscovery{targets: targets, synced: true},
		up:   newFakeUpstream(),
		rec:  &recorder{},
		logs: &syncBuffer{},
	}
	h.cfg.Store(testConfig())

	return h
}

// configure mutates the stored configuration before the handler is built.
func (h *harness) configure(fn func(*config.Config)) {
	cfg := h.cfg.Load()
	fn(cfg)
	h.cfg.Store(cfg)
}

func (h *harness) handler() http.Handler {
	var discovery k8s.Discovery = h.disc
	if h.discovery != nil {
		discovery = h.discovery
	}
	var upstream Upstream = h.up
	if h.upstream != nil {
		upstream = h.upstream
	}

	return New(Deps{
		Discovery: discovery,
		Upstream:  upstream,
		Config:    &h.cfg,
		Recorder:  h.rec,
		Logger:    slog.New(slog.NewJSONHandler(h.logs, nil)),
		Choose:    h.choose,
	})
}

// do runs one request through a fresh handler and applies the assertions every row shares:
// no response byte names the fixture address, and every response is no-store.
func (h *harness) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, nil))
	assertNoLeak(t, rec)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	return rec
}

// assertNoLeak fails when any header or body byte names the fixture Pod's address.
func assertNoLeak(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	for _, leak := range []string{fixtureIP, strconv.Itoa(fixturePort)} {
		for name, values := range rec.Header() {
			for _, v := range values {
				if strings.Contains(v, leak) {
					t.Errorf("header %s leaks %q: %q", name, leak, v)
				}
			}
		}
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("body leaks %q: %q", leak, rec.Body.String())
		}
	}
}

// errorBodyOf decodes a gateway error envelope.
func errorBodyOf(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()

	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not a JSON envelope: %v", rec.Body.String(), err)
	}

	return body.Code, body.Error
}

// expectError checks a gateway-generated error: status, code, JSON type, no target headers,
// and that the audit record and the single Recorder.Request call carry the same code.
func (h *harness) expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got, _ := errorBodyOf(t, rec); got != code {
		t.Errorf("code = %q, want %q (body %q)", got, code, rec.Body.String())
	}
	for name := range rec.Header() {
		if strings.HasPrefix(name, "X-Pprof-Target-") {
			t.Errorf("gateway error carries %s", name)
		}
	}
	h.expectAudit(t, status, code)
	h.expectMetricCode(t, code)
}

// expectAudit checks that exactly one audit record was written with the spec's keys,
// the given status and code, and no Pod address.
func (h *harness) expectAudit(t *testing.T, status int, code string) map[string]any {
	t.Helper()

	records := h.audits(t)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want 1: %s", len(records), h.logs.String())
	}
	rec := records[0]
	for _, key := range []string{"principal", "namespace", "service", "pod", "profile", "seconds", "status", "code", "duration_ms"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("audit record lacks %q: %v", key, rec)
		}
	}
	if got, _ := rec["status"].(float64); int(got) != status {
		t.Errorf("audit status = %v, want %d", rec["status"], status)
	}
	if got := rec["code"]; got != code {
		t.Errorf("audit code = %v, want %q", got, code)
	}
	if logs := h.logs.String(); strings.Contains(logs, fixtureIP) {
		t.Errorf("audit log names the Pod address: %s", logs)
	}

	return rec
}

// audits parses every "request" record the logger wrote.
func (h *harness) audits(t *testing.T) []map[string]any {
	t.Helper()

	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec["msg"] == "request" {
			records = append(records, rec)
		}
	}

	return records
}

// expectMetricCode checks that Recorder.Request was called exactly once with code.
func (h *harness) expectMetricCode(t *testing.T, code string) requestCall {
	t.Helper()

	requests, _, _ := h.rec.snapshot()
	if len(requests) != 1 {
		t.Fatalf("Recorder.Request calls = %d, want 1: %v", len(requests), requests)
	}
	if requests[0].code != code {
		t.Errorf("Recorder.Request code = %q, want %q", requests[0].code, code)
	}

	return requests[0]
}

// expectMetric checks the endpoint and profile labels of the single Recorder.Request call.
func (h *harness) expectMetric(t *testing.T, endpoint metrics.Endpoint, profile string) {
	t.Helper()

	requests, _, _ := h.rec.snapshot()
	if len(requests) != 1 {
		t.Fatalf("Recorder.Request calls = %d, want 1: %v", len(requests), requests)
	}
	if requests[0].endpoint != endpoint || requests[0].profile != profile {
		t.Errorf("Recorder.Request labels = (%q,%q), want (%q,%q)", requests[0].endpoint, requests[0].profile, endpoint, profile)
	}
}

// expectCounts checks how many times discovery and the upstream were reached.
func (h *harness) expectCounts(t *testing.T, targets, do int32) {
	t.Helper()

	if got := h.disc.targetsCalls.Load(); got != targets {
		t.Errorf("Targets calls = %d, want %d", got, targets)
	}
	if got := h.up.calls.Load(); got != do {
		t.Errorf("Upstream.Do calls = %d, want %d", got, do)
	}
}
