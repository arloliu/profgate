package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/pgo"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	fixtureNamespace = "payment"
	fixtureService   = "payment-api"
	fixturePod       = "payment-api-1"
	fixtureIP        = "10.0.0.5"
	// fixturePort is the number the fixture Pod exposes, which no response may
	// carry. It differs from the configured default in testConfig, which
	// /v1/limits reports and a refusal may echo, so assertNoLeak catches only
	// the resolved number.
	fixturePort    = 6070
	fixtureNode    = "worker-1"
	fixtureVersion = "1.0"
	fixtureUID     = "u1"

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
		// A numeric default and no allowedSelections entry: every harness
		// starts default-deny and states the entries it needs.
		Discovery: config.DiscoveryConfig{Pprof: config.PprofConfig{Port: 6060}},
		Limits:    config.LimitsConfig{CPUSeconds: 60, TraceSeconds: 60, MaxConcurrentProfiles: 16},
		Auth:      config.AuthConfig{Mode: "disabled", AnonymousRealm: "developer"},
		Realms: map[string]config.Realm{
			"developer": {Namespaces: []string{"*"}, Services: []string{"*"}, Profiles: []string{"*"}},
		},
	}
}

// oidcConfig is an oidc block whose issuer, audience, and token type are set,
// mapping one user to the developer realm, with the cli and browser blocks the row asks for.
func oidcConfig(cli *config.OIDCCLI, browser *config.OIDCBrowser) *config.OIDCConfig {
	return &config.OIDCConfig{
		Issuer: "https://issuer.example", Audience: "profgate", TokenType: "id",
		Mapping: config.OIDCMapping{Users: []config.OIDCMappingEntry{{Name: "alice", Realm: "developer"}}},
		Browser: browser, CLI: cli,
	}
}

// fakeDiscovery is a Discovery whose answers the test sets up front.
type fakeDiscovery struct {
	targets    []k8s.Target
	err        error
	synced     bool
	confirmErr error
	onTargets  func()

	// catalog and catalogErr answer Catalog; catalogNamespaces records the namespace argument of every call.
	catalog    []k8s.ServiceRef
	catalogErr error

	targetsCalls atomic.Int32
	confirmCalls atomic.Int32
	catalogCalls atomic.Int32

	mu                sync.Mutex
	confirmDeadline   time.Time
	confirmTarget     k8s.Target
	selections        []k8s.PortSelection // the port selection of every Targets call, in order
	catalogNamespaces []string            // the namespace argument of every Catalog call, in order
	// byName, when set, answers a portName selection from the map (nil for an
	// absent name) instead of targets, the way a name resolves per Pod.
	byName map[string][]k8s.Target
}

func (f *fakeDiscovery) Targets(_ context.Context, _, _ string, sel k8s.PortSelection) ([]k8s.Target, error) {
	f.targetsCalls.Add(1)
	f.mu.Lock()
	f.selections = append(f.selections, sel)
	f.mu.Unlock()
	if f.onTargets != nil {
		f.onTargets()
	}
	if f.err != nil {
		return nil, f.err
	}
	if sel.PortName != "" && f.byName != nil {
		return f.byName[sel.PortName], nil
	}

	return f.targets, nil
}

// selectionsSeen is the port selection of every Targets call, in order.
func (f *fakeDiscovery) selectionsSeen() []k8s.PortSelection {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]k8s.PortSelection(nil), f.selections...)
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

func (f *fakeDiscovery) Catalog(_ context.Context, namespace string) ([]k8s.ServiceRef, error) {
	f.catalogCalls.Add(1)
	f.mu.Lock()
	f.catalogNamespaces = append(f.catalogNamespaces, namespace)
	f.mu.Unlock()
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
	if namespace == "" {
		return f.catalog, nil
	}
	// A namespace argument narrows the answer, as the seam's Catalog does.
	refs := make([]k8s.ServiceRef, 0, len(f.catalog))
	for _, ref := range f.catalog {
		if ref.Namespace == namespace {
			refs = append(refs, ref)
		}
	}

	return refs, nil
}

func (f *fakeDiscovery) Explain(context.Context, string, string, k8s.PortSelection) (k8s.Explanation, error) {
	return k8s.Explanation{}, nil
}

// catalogNamespacesSeen is the namespace argument of every Catalog call, in order.
func (f *fakeDiscovery) catalogNamespacesSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.catalogNamespaces...)
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

// authFailureCall is one Recorder.AuthFailure call.
type authFailureCall struct {
	mode   string
	reason string
}

// recorder is a Recorder that remembers every call.
type recorder struct {
	mu           sync.Mutex
	requests     []requestCall
	confirms     []string
	collections  []string
	authFailures []authFailureCall
	inFlight     int
	peak         int
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

func (r *recorder) Collection(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collections = append(r.collections, result)
}

// collectionRows is every Collection result recorded, in order.
func (r *recorder) collectionRows() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.collections...)
}

func (r *recorder) CollectionSample(string) {}

func (r *recorder) CollectionDuration(time.Duration) {}

func (r *recorder) ScheduleSlot(string) {}

func (r *recorder) SweeperDelete(string) {}

func (r *recorder) CollectionsActive(int) {}

func (r *recorder) NATSConnected(bool) {}

func (r *recorder) TLSReload(string) {}

func (r *recorder) TLSCertificateExpiry(time.Time) {}

func (r *recorder) AuthFailure(mode, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authFailures = append(r.authFailures, authFailureCall{mode, reason})
}

// authFailureRows is every AuthFailure call recorded, in order.
func (r *recorder) authFailureRows() []authFailureCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]authFailureCall(nil), r.authFailures...)
}

func (r *recorder) AuthSessionIssued() {}

func (r *recorder) JWKSRefresh(string) {}

func (r *recorder) JWKSKeys(int) {}

func (r *recorder) JWKSFetched(time.Time) {}

func (r *recorder) AuthFileReload(string, string) {}

func (r *recorder) CookieKeys([]metrics.CookieKey) {}

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

// trap is an httptest.Server that counts every connection it receives;
// rows that must not dial use its address as the target's PodIP and Port.
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
	discovery k8s.Discovery      // overrides disc when set
	upstream  Upstream           // overrides up when set
	gate      *admit.Gate        // overrides the per-handler gate when set
	runtime   *pgo.Runtime       // nil leaves the handler an unbound one
	auth      auth.Authenticator // nil leaves the handler on auth.Disabled
	routes    auth.AuthRoutes    // nil leaves the /auth/ routes unknown
	console   http.Handler       // nil leaves /ui/ and / unknown
	ready     func() bool        // nil leaves readiness on disc.synced
	// beforeAllowlist, when set, runs between the realm check and the allowlist check.
	beforeAllowlist func()
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

	gate := h.gate
	if gate == nil {
		gate = admit.New(h.cfg.Load().Limits.MaxConcurrentProfiles)
	}

	handler := New(Deps{
		Discovery:  discovery,
		Upstream:   upstream,
		Config:     &h.cfg,
		Recorder:   h.rec,
		Gate:       gate,
		PGO:        h.runtime,
		Auth:       h.auth,
		AuthRoutes: h.routes,
		Console:    h.console,
		Ready:      h.ready,
		Logger:     h.logger(),
		Choose:     h.choose,
	})
	if h.beforeAllowlist != nil {
		setBeforeAllowlist(handler, h.beforeAllowlist)
	}

	return handler
}

// logger is the one logger the handler, the caches, the publisher, and the
// runtime write to, so a test reads every record of a request from one place.
func (h *harness) logger() *slog.Logger { return slog.New(slog.NewJSONHandler(h.logs, nil)) }

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
	// details is omitted, never null or empty, and only port_not_allowed carries it.
	body := rec.Body.String()
	if strings.Contains(body, `"details":null`) || strings.Contains(body, `"details":[]`) {
		t.Errorf("body %q carries an empty details", body)
	}
	if strings.Contains(body, `"details"`) != (code == "port_not_allowed") {
		t.Errorf("body %q: details must appear for port_not_allowed and no other code", body)
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
	for _, key := range []string{"principal", "namespace", "service", "pod", "profile", "seconds", "port", "status", "code", "duration_ms"} {
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

// The key layout of internal/pgo.
// PGO assertions are made against the authoritative bucket rather than through
// a replica's cache, so the tests address its keys directly.
const (
	jobKeyPrefix      = "job."
	activeKeyPrefix   = "active."
	slotKeyPrefix     = "schedule."
	overrideKeyPrefix = "service."
)

// watchBuffer is how many entries one fake watch holds before a send would block.
// It is a failure deadline and never a throttle: no test writes anywhere near
// this many keys.
const watchBuffer = 1024

// fakeWatch is one open watch over a prefix.
type fakeWatch struct {
	prefix string
	ch     chan natskv.Entry
	// pending holds what a frozen watch has not delivered.
	pending []natskv.Entry
	frozen  bool
}

// fakeKV is one in-memory bucket with the revision semantics the seam
// promises: Create loses to an existing key, Update and Delete lose to a
// revision that has moved, and every write is delivered to the open watches.
// It is the authoritative bucket a PGO assertion is made against.
type fakeKV struct {
	gen func() uint64

	mu       sync.Mutex
	revision uint64
	entries  map[string]natskv.Entry
	watches  []*fakeWatch

	// The failures a test drives a handler through.
	getErr    error
	createErr error
	updateErr error
	// updateMismatch makes every Update lose, which is a record moving faster
	// than a handler can read it.
	updateMismatch bool
	// afterGet, when set, runs after a successful Get, so a test can move a key
	// between a handler's read and the conditional write it makes at that revision.
	afterGet func()

	updates atomic.Int32
	creates atomic.Int32
}

func newFakeKV(gen func() uint64) *fakeKV {
	return &fakeKV{gen: gen, entries: make(map[string]natskv.Entry)}
}

func (k *fakeKV) Get(_ context.Context, key string) (natskv.Entry, error) {
	k.mu.Lock()
	getErr, after := k.getErr, k.afterGet
	e, ok := k.entries[key]
	k.mu.Unlock()

	if getErr != nil {
		return natskv.Entry{}, getErr
	}
	if !ok {
		return natskv.Entry{}, fmt.Errorf("get %q: %w", key, natskv.ErrKeyNotFound)
	}
	// The key may move now, which is what makes the caller's revision the one
	// its conditional write loses on.
	if after != nil {
		after()
	}

	return e, nil
}

func (k *fakeKV) Create(_ context.Context, key string, value []byte) (uint64, error) {
	k.creates.Add(1)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.createErr != nil {
		return 0, k.createErr
	}
	if _, ok := k.entries[key]; ok {
		return 0, fmt.Errorf("create %q: %w", key, natskv.ErrKeyExists)
	}

	return k.storeLocked(key, value), nil
}

func (k *fakeKV) Update(_ context.Context, key string, value []byte, expected uint64) (uint64, error) {
	k.updates.Add(1)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.updateErr != nil {
		return 0, k.updateErr
	}
	e, ok := k.entries[key]
	if k.updateMismatch || !ok || e.Revision != expected {
		return 0, fmt.Errorf("update %q at revision %d: %w", key, expected, natskv.ErrRevisionMismatch)
	}

	return k.storeLocked(key, value), nil
}

func (k *fakeKV) Delete(_ context.Context, key string, expected uint64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.entries[key]
	if !ok || e.Revision != expected {
		return fmt.Errorf("delete %q at revision %d: %w", key, expected, natskv.ErrRevisionMismatch)
	}
	delete(k.entries, key)
	k.revision++
	k.deliverLocked(natskv.Entry{Key: key, Revision: k.revision, Generation: k.gen()})

	return nil
}

func (k *fakeKV) Keys(_ context.Context, prefix string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.keysLocked(prefix), nil
}

func (k *fakeKV) Watch(ctx context.Context, prefix string) (<-chan natskv.Entry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	w := &fakeWatch{prefix: prefix, ch: make(chan natskv.Entry, watchBuffer)}
	k.watches = append(k.watches, w)
	for _, key := range k.keysLocked(prefix) {
		w.ch <- k.entries[key]
	}
	// The end of the initial replay, which is what closes the barrier's other half.
	w.ch <- natskv.Entry{Synced: true, Generation: k.gen()}

	go func() {
		<-ctx.Done()
		k.mu.Lock()
		defer k.mu.Unlock()
		close(w.ch)
		for i, open := range k.watches {
			if open == w {
				k.watches = append(k.watches[:i], k.watches[i+1:]...)

				return
			}
		}
	}()

	return w.ch, nil
}

// storeLocked writes one value at the next revision and delivers it.
func (k *fakeKV) storeLocked(key string, value []byte) uint64 {
	k.revision++
	e := natskv.Entry{
		Key:        key,
		Value:      append([]byte(nil), value...),
		Revision:   k.revision,
		Created:    time.Now(),
		Generation: k.gen(),
	}
	k.entries[key] = e
	k.deliverLocked(e)

	return k.revision
}

// deliverLocked hands one entry to every watch of a matching prefix, holding it
// back on a frozen one.
func (k *fakeKV) deliverLocked(e natskv.Entry) {
	for _, w := range k.watches {
		if !strings.HasPrefix(e.Key, w.prefix) {
			continue
		}
		if w.frozen {
			w.pending = append(w.pending, e)

			continue
		}
		w.ch <- e
	}
}

// keysLocked lists the live keys under prefix, in a stable order.
func (k *fakeKV) keysLocked(prefix string) []string {
	out := make([]string, 0, len(k.entries))
	for key := range k.entries {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	slices.Sort(out)

	return out
}

// freeze holds delivery of one prefix's later writes, leaving the cache stale
// while the authoritative bucket moves on.
func (k *fakeKV) freeze(prefix string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, w := range k.watches {
		if w.prefix == prefix {
			w.frozen = true
		}
	}
}

// countKeys is how many keys the authoritative bucket holds under prefix.
func (k *fakeKV) countKeys(prefix string) int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return len(k.keysLocked(prefix))
}

// put writes one value outside the seam, standing in for another replica.
func (k *fakeKV) put(t *testing.T, key string, value any) uint64 {
	t.Helper()

	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.storeLocked(key, body)
}

// record reads one Collection record straight from the authoritative bucket.
func (k *fakeKV) record(t *testing.T, id string) pgo.Record {
	t.Helper()

	e, err := k.Get(context.Background(), jobKeyPrefix+id)
	if err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	var rec pgo.Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		t.Fatalf("record %s is not readable: %v", id, err)
	}

	return rec
}

// fakeObjects is an in-memory Object Store.
type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	// openErr, when set, is what Get answers before it hands back a reader.
	openErr error
	// readErr, when set, is what a reader fails with after failAfter bytes.
	readErr   error
	failAfter int
	// onRead runs once, on the first read, so a test can act mid-stream.
	onRead func()
	// gate, when set, parks every read after the first until it is closed or
	// the read's context ends, so the server cannot outrun the test.
	gate chan struct{}
	// opened is the last reader handed out, so a test can see how far the
	// stream got and whether it was released.
	opened *fakeReader
}

func newFakeObjects() *fakeObjects { return &fakeObjects{objects: make(map[string][]byte)} }

func (o *fakeObjects) Put(_ context.Context, name string, r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[name] = body

	return nil
}

func (o *fakeObjects) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.openErr != nil {
		return nil, o.openErr
	}
	body, ok := o.objects[name]
	if !ok {
		return nil, fmt.Errorf("get %q: %w", name, natskv.ErrObjectNotFound)
	}

	o.opened = &fakeReader{
		ctx:       ctx,
		body:      body,
		readErr:   o.readErr,
		failAfter: o.failAfter,
		onRead:    o.onRead,
		gate:      o.gate,
	}

	return o.opened, nil
}

// reader is the last reader the store handed out.
func (o *fakeObjects) reader() *fakeReader {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.opened
}

func (o *fakeObjects) Delete(_ context.Context, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.objects, name)

	return nil
}

func (o *fakeObjects) List(context.Context) ([]natskv.ObjectInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]natskv.ObjectInfo, 0, len(o.objects))
	for name, body := range o.objects {
		out = append(out, natskv.ObjectInfo{Name: name, Size: uint64(len(body))})
	}

	return out, nil
}

// put stores one object.
func (o *fakeObjects) put(name string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[name] = body
}

// fakeReader streams one object, optionally failing part-way and optionally
// letting the test act on the first read.
type fakeReader struct {
	ctx       context.Context
	body      []byte
	offset    int
	readErr   error
	failAfter int
	onRead    func()
	gate      chan struct{}
	once      sync.Once
	reads     atomic.Int32
	closed    atomic.Bool
}

func (r *fakeReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		if r.onRead != nil {
			r.onRead()
		}
	})
	if r.gate != nil && r.reads.Load() > 0 {
		select {
		case <-r.gate:
		case <-r.ctx.Done():
		}
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.readErr != nil && r.offset >= r.failAfter {
		return 0, r.readErr
	}
	if r.offset >= len(r.body) {
		return 0, io.EOF
	}
	// One chunk per read, so a failure or a cancellation lands mid-stream.
	n := copy(p, r.body[r.offset:min(len(r.body), r.offset+chunkBytes)])
	r.offset += n
	r.reads.Add(1)

	return n, nil
}

func (r *fakeReader) Close() error {
	r.closed.Store(true)

	return nil
}

// chunkBytes is how much one fakeReader read returns, so a stream a test
// interrupts has somewhere to be interrupted.
const chunkBytes = 8

// fakeNATS is the connection the PGO routes see: three in-memory buckets, a
// generation, and the two flags the replay barrier is made of.
type fakeNATS struct {
	config    *fakeKV
	jobs      *fakeKV
	artifacts *fakeObjects

	mu        sync.Mutex
	gen       uint64
	connected bool
	synced    bool
}

func newFakeNATS() *fakeNATS {
	f := &fakeNATS{artifacts: newFakeObjects(), gen: 1, connected: true, synced: true}
	f.config = newFakeKV(f.Generation)
	f.jobs = newFakeKV(f.Generation)

	return f
}

func (f *fakeNATS) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.connected
}

func (f *fakeNATS) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.gen
}

func (f *fakeNATS) Synced(gen uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.synced && gen == f.gen
}

func (f *fakeNATS) View(gen uint64) (natskv.Stores, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if gen != f.gen {
		return natskv.Stores{}, fmt.Errorf("view of generation %d: %w", gen, natskv.ErrUnavailable)
	}

	return natskv.Stores{Config: f.config, Jobs: f.jobs, Artifacts: f.artifacts}, nil
}

// disconnect is the connection going down: the generation moves in the
// disconnected callback, before the connection is usable again.
func (f *fakeNATS) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = false
	f.gen++
}

// holdReplay leaves the seam's own watches unsynced, which is a connection
// whose watches have not finished replaying.
func (f *fakeNATS) holdReplay() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = false
}

// fakePGOClock is the clock the publisher and the on-demand bucket run on.
type fakePGOClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakePGOClock() *fakePGOClock { return &fakePGOClock{now: pgoFixtureNow} }

// pgoFixtureNow is the instant every PGO fixture starts at: 2026-08-24T00:00:00Z.
var pgoFixtureNow = time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

func (c *fakePGOClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakePGOClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakePGOClock) NewTimer(time.Duration) pgo.Timer   { panic("no PGO route uses a timer") }
func (c *fakePGOClock) NewTicker(time.Duration) pgo.Ticker { panic("no PGO route uses a ticker") }

// PGO route paths over the fixture Service and one Collection.
const (
	pgoPath         = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/pgo"
	collectionsPath = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/collections"
)

// fixtureInstance is the gateway instance every PGO fixture runs as:
// a Pod name plus a per-process suffix, which a realm that may read PGO state
// may know.
const fixtureInstance = "profgate-7f88fdf79-xabcd/q2w3e4r5"

// pgoHarness is a harness with the PGO machinery behind it: an in-memory
// connection, the real watched caches over it, the real publisher, and a bound
// runtime.
// The caches are the real ones so a test can freeze a watch and leave the
// handler deciding from a cache the authoritative bucket has moved past.
type pgoHarness struct {
	*harness
	nats   *fakeNATS
	caches *pgo.Caches
	pub    *pgo.Publisher
	clock  *fakePGOClock
	limits config.PGOLimits
}

// pgoOpts shapes one PGO harness.
type pgoOpts struct {
	limits config.PGOLimits
	// realm replaces the wide-open realm every flag is set on.
	realm *config.Realm
	// configAPI replaces the default "enabled".
	configAPI string
}

// testPGOLimits is the shipped ceilings with the fields one test needs changed.
func testPGOLimits(mutate ...func(*config.PGOLimits)) config.PGOLimits {
	limits := config.PGOLimits{
		MaxDuration:          60 * time.Second,
		MaxRounds:            5,
		MaxParallel:          4,
		MinEvery:             15 * time.Minute,
		MaxEvery:             24 * time.Hour,
		MaxRetention:         24 * time.Hour,
		MaxSampleBytes:       33554432,
		MaxMergedBytes:       67108864,
		MaxTargetsPerRound:   32,
		MaxActiveCollections: 2,
		OnDemandPerMinute:    10,
		MaxLiveCollections:   64,
	}
	for _, m := range mutate {
		m(&limits)
	}

	return limits
}

// testPGODefaults is the operator's default policy every override layers onto.
func testPGODefaults() config.PGODefaults {
	return config.PGODefaults{
		Schedule: config.PGOScheduleDefaults{Every: 6 * time.Hour, Jitter: 10 * time.Minute},
		Sampling: config.PGOSamplingDefaults{
			Duration:      30 * time.Second,
			Rounds:        2,
			RoundInterval: 30 * time.Second,
			Replicas:      "all",
			MaxParallel:   4,
		},
		Target:   config.PGOTargetDefaults{VersionPolicy: "strict"},
		Artifact: config.PGOArtifactDefaults{Retention: 2 * time.Hour},
	}
}

// wideRealm is a realm that allows every namespace, Service, profile, and PGO action.
func wideRealm() config.Realm {
	return config.Realm{
		Namespaces: []string{"*"},
		Services:   []string{"*"},
		Profiles:   []string{"*"},
		PGO:        config.RealmPGO{Read: true, Collect: true, Configure: true},
	}
}

// newPGOHarness wires one gateway replica's PGO machinery over an in-memory
// connection and binds the runtime, as cmd/profgate does once its preflight
// has passed.
func newPGOHarness(t *testing.T, o pgoOpts, targets ...k8s.Target) *pgoHarness {
	t.Helper()

	if o.limits == (config.PGOLimits{}) {
		o.limits = testPGOLimits()
	}
	if o.configAPI == "" {
		o.configAPI = "enabled"
	}
	realm := wideRealm()
	if o.realm != nil {
		realm = *o.realm
	}

	if len(targets) == 0 {
		targets = []k8s.Target{baseTarget()}
	}
	h := newHarness(targets...)
	h.configure(func(cfg *config.Config) {
		cfg.PGO.Enabled = true
		cfg.PGO.ConfigAPI = o.configAPI
		cfg.PGO.Limits = o.limits
		cfg.PGO.Defaults = testPGODefaults()
		cfg.Realms["developer"] = realm
	})

	defaults, err := pgo.DefaultPolicy(testPGODefaults())
	if err != nil {
		t.Fatalf("DefaultPolicy: %v", err)
	}

	nats := newFakeNATS()
	clock := newFakePGOClock()
	caches := pgo.NewCaches(h.logger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := caches.Run(ctx, nats); err != nil {
			h.logger().Error("caches stopped", "error", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	pub := pgo.NewPublisher(caches, clock, o.limits.MaxLiveCollections, fixtureInstance, h.logger())
	runtime := pgo.NewRuntime()
	runtime.Bind(pgo.Bundle{
		Client:    nats,
		Caches:    caches,
		Publisher: pub,
		Bucket:    pgo.NewTokenBucket(o.limits.OnDemandPerMinute, clock),
		Defaults:  defaults,
		Limits:    o.limits,
		Clock:     clock,
		Recorder:  h.rec,
		Instance:  fixtureInstance,
		Log:       h.logger(),
	})
	h.runtime = runtime

	p := &pgoHarness{harness: h, nats: nats, caches: caches, pub: pub, clock: clock, limits: o.limits}
	p.waitCache(t, "the initial replay", func() bool { return caches.Synced(nats.Generation()) })

	return p
}

// waitCache blocks until pred holds, so a test never drives a handler against
// a cache that has not yet seen its own setup.
func (p *pgoHarness) waitCache(t *testing.T, what string, pred func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// newRecord is a Collection record in the given state, with whatever a test
// needs changed.
func (p *pgoHarness) newRecord(state pgo.State, mutate ...func(*pgo.Record)) pgo.Record {
	defaults, err := pgo.DefaultPolicy(testPGODefaults())
	if err != nil {
		panic(err)
	}
	rec := pgo.Record{
		ID:        newFixtureID(),
		Namespace: fixtureNamespace,
		Service:   fixtureService,
		Origin:    pgo.OriginSchedule,
		Policy:    defaults,
		State:     state,
		ClaimBy:   pgoFixtureNow.Add(time.Hour),
		CreatedBy: "schedule",
		CreatedAt: pgoFixtureNow,
	}
	for _, m := range mutate {
		m(&rec)
	}

	return rec
}

// seedRecord writes one Collection record into the authoritative bucket,
// standing in for what a scheduler or another replica published, and waits for
// the cache to deliver it.
func (p *pgoHarness) seedRecord(t *testing.T, rec pgo.Record) pgo.Record {
	t.Helper()

	p.nats.jobs.put(t, jobKeyPrefix+rec.ID, rec)
	p.waitCache(t, "collection "+rec.ID, func() bool {
		for _, v := range p.caches.Collections(rec.Namespace, rec.Service) {
			if v.ID == rec.ID {
				return true
			}
		}

		return false
	})

	return rec
}

// seedActive writes the active key of one Service, which is what a live
// Collection holds.
func (p *pgoHarness) seedActive(t *testing.T, namespace, service, id string) {
	t.Helper()

	p.nats.jobs.put(t, activeKeyPrefix+namespace+"."+service,
		map[string]any{"id": id, "createdAt": pgoFixtureNow})
	p.waitCache(t, "the active key of "+service, func() bool { return p.caches.Live(namespace, service) })
}

// seedOverride writes one Service's stored policy override and returns its revision.
func (p *pgoHarness) seedOverride(t *testing.T, override *pgo.PolicyOverride) uint64 {
	t.Helper()

	revision := p.nats.config.put(t, overrideKeyPrefix+fixtureNamespace+"."+fixtureService,
		pgo.StoredOverride{Policy: override, UpdatedBy: "anonymous", UpdatedAt: pgoFixtureNow})
	p.waitCache(t, "the policy override", func() bool {
		_, rev := p.caches.Override(fixtureNamespace, fixtureService)

		return rev == revision
	})

	return revision
}

// newFixtureID is a Collection identifier of the grammar the routes accept.
func newFixtureID() string {
	fixtureIDs.Add(1)

	return "abcdefghjkmnpqrstv" + string(idAlphabetFixture[fixtureIDs.Load()%32]) +
		string(idAlphabetFixture[(fixtureIDs.Load()/32)%32])
}

// idAlphabetFixture is the Crockford base32 alphabet the identifier grammar uses.
const idAlphabetFixture = "0123456789abcdefghjkmnpqrstvwxyz"

// fixtureIDs makes every fixture identifier distinct within a run.
var fixtureIDs atomic.Int64

// collectionPath is the path of one Collection, with an optional suffix.
func collectionPath(id, suffix string) string { return "/v1/collections/" + id + suffix }

// doPGO runs one PGO request through a fresh handler, with the shared
// assertions every row makes: nothing names the fixture Pod's address, and the
// response is no-store.
func (p *pgoHarness) doPGO(t *testing.T, method, target, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, reader)
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	rec := httptest.NewRecorder()
	p.handler().ServeHTTP(rec, req)
	assertNoLeak(t, rec)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	return rec
}

// ifMatch is the header one conditional policy write carries.
func ifMatch(etag string) http.Header { return http.Header{"If-Match": []string{etag}} }

// expectPGOError checks a PGO failure: status, the code the body carries, and
// the code the audit record and the metrics row carry.
// The two differ for the outcomes that have a code of their own but no status
// of their own, which is why this does not reuse the interactive assertion.
func (h *harness) expectPGOError(
	t *testing.T, rec *httptest.ResponseRecorder, status int, code, auditCode string,
) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got, _ := errorBodyOf(t, rec); got != code {
		t.Errorf("code = %q, want %q (body %q)", got, code, rec.Body.String())
	}
	h.expectPGOAudit(t, status, auditCode)
	h.expectMetricCode(t, auditCode)
}

// expectPGOAudit checks that exactly one audit record was written with the
// spec's PGO keys, the given status and code, and no Pod address.
func (h *harness) expectPGOAudit(t *testing.T, status int, code string) map[string]any {
	t.Helper()

	records := h.audits(t)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want 1: %s", len(records), h.logs.String())
	}
	rec := records[0]
	for _, key := range []string{"principal", "namespace", "service", "collection", "method", "status", "code", "duration_ms"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("audit record lacks %q: %v", key, rec)
		}
	}
	if _, ok := rec["pod"]; ok {
		t.Errorf("a pgo audit record carries a pod: %v", rec)
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

// transitions parses every "collection transition" record the logger wrote.
func (h *harness) transitions(t *testing.T) []map[string]any {
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
		if rec["msg"] == "collection transition" {
			records = append(records, rec)
		}
	}

	return records
}
