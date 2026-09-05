package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// confirmBlocks holds Confirm open until its context ends, and returns that context's error.
	confirmBlocks bool
	onTargets     func()

	// catalog and catalogErr answer Catalog; catalogNamespaces records the namespace argument of every call.
	catalog    []k8s.ServiceRef
	catalogErr error

	// explanation and explainErr answer Explain; explainSelections records the port selection of every call.
	explanation k8s.Explanation
	explainErr  error

	targetsCalls atomic.Int32
	confirmCalls atomic.Int32
	catalogCalls atomic.Int32
	explainCalls atomic.Int32

	mu                sync.Mutex
	confirmDeadline   time.Time
	confirmTarget     k8s.Target
	selections        []k8s.PortSelection // the port selection of every Targets call, in order
	explainSelections []k8s.PortSelection // the port selection of every Explain call, in order
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
	if f.confirmBlocks {
		<-ctx.Done()

		return ctx.Err()
	}

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

func (f *fakeDiscovery) Explain(_ context.Context, _, _ string, sel k8s.PortSelection) (k8s.Explanation, error) {
	f.explainCalls.Add(1)
	f.mu.Lock()
	f.explainSelections = append(f.explainSelections, sel)
	f.mu.Unlock()
	if f.explainErr != nil {
		return k8s.Explanation{}, f.explainErr
	}

	return f.explanation, nil
}

// explainSelectionsSeen is the port selection of every Explain call, in order.
func (f *fakeDiscovery) explainSelectionsSeen() []k8s.PortSelection {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]k8s.PortSelection(nil), f.explainSelections...)
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

func (r *recorder) PGOSyncedFrom(func() bool) {}

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

// cutServer serves handler over a real socket whose request contexts descend from one drain context,
// the shape cmd/profgate gives the API listener.
// The returned function is the cut: it cancels that context with ErrDrainExpired and leaves every socket open,
// which is the interval between the drain bound ending and the process closing the connections.
func cutServer(t *testing.T, handler http.Handler) (*httptest.Server, func()) {
	t.Helper()

	drainCtx, cancel := context.WithCancelCause(context.Background())
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.BaseContext = func(net.Listener) context.Context { return drainCtx }
	srv.Start()
	t.Cleanup(func() {
		cancel(nil)
		srv.Close()
	})

	return srv, func() { cancel(ErrDrainExpired) }
}

// dialRequest opens a raw connection to srv and sends one GET with no body,
// so the test holds the socket itself rather than through a client that would read, retry, or close it.
// Every read and write on the connection is bounded, so a request nothing ends fails the test rather than hangs it.
func dialRequest(t *testing.T, srv *httptest.Server, target string) net.Conn {
	t.Helper()

	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(heldOpenTimeout)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}
	if _, err := io.WriteString(conn, "GET "+target+" HTTP/1.1\r\nHost: gateway\r\n\r\n"); err != nil {
		t.Fatalf("write request error = %v", err)
	}

	return conn
}

// expectNothingRead asserts the connection delivers no byte before the server closes it:
// a request the drain cut ends with nothing written, not with an envelope and not with an empty 200.
func expectNothingRead(t *testing.T, conn net.Conn) {
	t.Helper()

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("Read() = %d bytes %q, error %v; want 0 bytes and io.EOF: nothing is written to a request the drain cut", n, buf[:n], err)
	}
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
	// drain is the signal a request held open by a wait ends on;
	// it closes only where a test drains the replica.
	drain chan struct{}
	// beforeAllowlist, when set, runs between the realm check and the allowlist check.
	beforeAllowlist func()
}

func newHarness(targets ...k8s.Target) *harness {
	h := &harness{
		disc:  &fakeDiscovery{targets: targets, synced: true},
		up:    newFakeUpstream(),
		rec:   &recorder{},
		logs:  &syncBuffer{},
		drain: make(chan struct{}),
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
		Drain:      h.drain,
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

	return h.doHeaders(t, method, target, nil)
}

// doHeaders is do with the request headers a row sends.
func (h *harness) doHeaders(t *testing.T, method, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	rec := httptest.NewRecorder()
	h.handler().ServeHTTP(rec, req)
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
			// The request identifier is 32 random hexadecimal characters when the gateway mints one,
			// and the client's own text when it does not,
			// so it names nothing of the cluster and a chance substring is not a leak.
			// Its own rules are asserted in requestid_test.go.
			if name == requestIDHeader {
				continue
			}
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
	// Every envelope the suite reads passes through here or through detailsOf,
	// so between them a code no registry constant names is caught wherever it is written.
	if !slices.Contains(EnvelopeCodes(), body.Code) {
		t.Errorf("body %q carries code %q, which is not in the registry", rec.Body.String(), body.Code)
	}

	return body.Code, body.Error
}

// expectError checks a gateway-generated error: status, code, JSON type, no target headers,
// and that the audit record and the single Recorder.Request call carry the same code.
func (h *harness) expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	h.expectErrorBody(t, rec, status, code)
	h.expectAudit(t, status, code)
}

// expectUnnarratedError is expectError for a route that writes no audit record:
// it carries no principal and names nothing a realm bounds.
func (h *harness) expectUnnarratedError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	h.expectErrorBody(t, rec, status, code)
	h.expectNoAudit(t)
}

// expectErrorBody holds every rule a gateway error obeys but the audit record.
func (h *harness) expectErrorBody(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
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
	detailsOf(t, rec, code)
	for name := range rec.Header() {
		if strings.HasPrefix(name, "X-Pprof-Target-") {
			t.Errorf("gateway error carries %s", name)
		}
	}
	h.expectMetricCode(t, code)
}

// codesWithDetails are the envelope codes Errors and pgo.md Ceilings give a
// details vocabulary; every other code carries no details key at all.
var codesWithDetails = []string{"invalid_parameter", "limit_exceeded", "port_not_allowed"}

// detailsOf decodes the details array of an error envelope and holds the rules every error body obeys:
// a code the registry holds,
// at least one item for a code with a vocabulary,
// no key at all for a code without one,
// never null and never empty,
// every item carrying a code,
// and no item naming a Pod, an address, or a resolved port.
func detailsOf(t *testing.T, rec *httptest.ResponseRecorder, code string) []errorDetail {
	t.Helper()

	body := rec.Body.String()
	if strings.Contains(body, `"details":null`) || strings.Contains(body, `"details":[]`) {
		t.Errorf("body %q carries an empty details", body)
	}

	var envelope struct {
		Code    string        `json:"code"`
		Details []errorDetail `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body %q is not a JSON envelope: %v", body, err)
	}
	// expectDetails reaches this without errorBodyOf, so the registry check runs here too.
	if !slices.Contains(EnvelopeCodes(), envelope.Code) {
		t.Errorf("body %q carries code %q, which is not in the registry", body, envelope.Code)
	}
	if want := slices.Contains(codesWithDetails, code); (len(envelope.Details) > 0) != want {
		t.Errorf("body %q: details appear for %v and for no other code", body, codesWithDetails)
	}
	for _, item := range envelope.Details {
		if item.Code == "" {
			t.Errorf("details item %+v carries no code", item)
		}
		for _, leak := range []string{fixturePod, fixtureIP, strconv.Itoa(fixturePort)} {
			if strings.Contains(item.Field, leak) || strings.Contains(item.Message, leak) {
				t.Errorf("details item %+v names %q", item, leak)
			}
		}
	}

	return envelope.Details
}

// expectDetails checks the whole details array of one refusal, item by item.
func expectDetails(t *testing.T, rec *httptest.ResponseRecorder, code string, want []errorDetail) {
	t.Helper()

	got := detailsOf(t, rec, code)
	if len(got) != len(want) {
		t.Fatalf("details = %+v, want %d item(s): %+v", got, len(want), want)
	}
	for i, w := range want {
		if got[i].Field != w.Field || got[i].Code != w.Code {
			t.Errorf("details[%d] = %+v, want field %q code %q", i, got[i], w.Field, w.Code)
		}
		if got[i].Message == "" {
			t.Errorf("details[%d] = %+v, want a message", i, got[i])
		}
	}
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
	for _, key := range []string{"requestId", "principal", "namespace", "service", "pod", "profile", "seconds", "port", "status", "code", "duration_ms"} {
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

// logRecords parses every record the logger wrote under one message,
// which is how a test reads a record that is not the audit line.
func (h *harness) logRecords(t *testing.T, msg string) []map[string]any {
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
		if rec["msg"] == msg {
			records = append(records, rec)
		}
	}

	return records
}

// logText is every captured log line flattened without its time and requestId fields.
// slog writes the timestamp, and the request id is random hex or the caller's own,
// so neither can carry what a fixture holds.
// Leaving them in lets a sentinel that is a bare number match a nanosecond timestamp or a hex identifier,
// reporting a leak that did not happen.
func (h *harness) logText(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		delete(rec, "time")
		delete(rec, "requestId")
		out, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("re-encode %v: %v", rec, err)
		}
		b.Write(out)
		b.WriteByte('\n')
	}

	return b.String()
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
	idemKeyPrefix     = "idem."
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
	// beforeCall, when set, runs at the start of every operation,
	// outside the bucket's lock,
	// so a test can hold one write open while another request runs to completion.
	beforeCall func(op, key string)
	// holdsGets makes every Get close getStarted and then wait for its context to end,
	// answering with that context's error,
	// so a test can cut a request that is inside the store call rather than one that has not reached it.
	holdsGets  bool
	getStarted chan struct{}
	startOnce  sync.Once

	updates atomic.Int32
	creates atomic.Int32
	// calls counts the reads and writes a caller makes through the seam.
	// Keys and Watch are outside it:
	// those are the caches' own replay,
	// which runs in the background whether or not a handler reaches the store.
	calls atomic.Int32
}

func newFakeKV(gen func() uint64) *fakeKV {
	return &fakeKV{gen: gen, entries: make(map[string]natskv.Entry)}
}

// hold runs the test's intervention for one operation, outside the lock.
func (k *fakeKV) hold(op, key string) {
	k.mu.Lock()
	before := k.beforeCall
	k.mu.Unlock()
	if before != nil {
		before(op, key)
	}
}

// setBefore installs the intervention every later operation runs first.
func (k *fakeKV) setBefore(fn func(op, key string)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.beforeCall = fn
}

func (k *fakeKV) Get(ctx context.Context, key string) (natskv.Entry, error) {
	k.calls.Add(1)
	k.hold("get", key)
	k.mu.Lock()
	getErr, after := k.getErr, k.afterGet
	holds, started := k.holdsGets, k.getStarted
	e, ok := k.entries[key]
	k.mu.Unlock()

	if holds {
		k.startOnce.Do(func() { close(started) })
		<-ctx.Done()

		return natskv.Entry{}, ctx.Err()
	}
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
	k.calls.Add(1)
	k.creates.Add(1)
	k.hold("create", key)
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
	k.calls.Add(1)
	k.updates.Add(1)
	k.hold("update", key)
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
	k.calls.Add(1)
	k.hold("delete", key)
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

// reopenWatches replays every open watch under the current generation:
// the keys the bucket holds, stamped with that generation, then the marker that ends a replay.
// It is what the seam delivers after it re-opens a watch the generation moved out from under,
// which is what makes a watched cache a rebuild rather than a patch.
func (k *fakeKV) reopenWatches() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, w := range k.watches {
		for _, key := range k.keysLocked(w.prefix) {
			e := k.entries[key]
			e.Generation = k.gen()
			k.deliverToLocked(w, e)
		}
		k.deliverToLocked(w, natskv.Entry{Synced: true, Generation: k.gen()})
	}
}

// deliverToLocked hands one entry to one watch, holding it back on a frozen one.
func (k *fakeKV) deliverToLocked(w *fakeWatch, e natskv.Entry) {
	if w.frozen {
		w.pending = append(w.pending, e)

		return
	}
	w.ch <- e
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

// remove deletes one key outside the seam, standing in for the sweeper.
func (k *fakeKV) remove(t *testing.T, key string) {
	t.Helper()

	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.entries[key]; !ok {
		t.Fatalf("remove %s: no such key", key)
	}
	delete(k.entries, key)
	k.revision++
	k.deliverLocked(natskv.Entry{Key: key, Revision: k.revision, Generation: k.gen()})
}

// setGetErr installs what every later read answers,
// so a test can take the store away from a request that is already in flight.
func (k *fakeKV) setGetErr(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.getErr = err
}

// blockGets makes every later Get signal its entry on the returned channel
// and then wait for its context to end, the way a store call held across the drain cut does.
func (k *fakeKV) blockGets() <-chan struct{} {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.holdsGets = true
	k.getStarted = make(chan struct{})

	return k.getStarted
}

// watchCount is how many watches are open on the bucket, which is the caches'
// own and nothing a request added.
func (k *fakeKV) watchCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return len(k.watches)
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
	// afterOpen, when set, runs after a reader has been handed out,
	// outside the store's lock,
	// so a test can take the object away from a request that already holds its bytes.
	afterOpen func(name string)

	// opens counts the readers handed out,
	// which is how many times a request asked the store for an object.
	opens atomic.Int32
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
	o.opens.Add(1)
	o.mu.Lock()
	if o.openErr != nil {
		err := o.openErr
		o.mu.Unlock()

		return nil, err
	}
	body, ok := o.objects[name]
	if !ok {
		o.mu.Unlock()

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
	opened, after := o.opened, o.afterOpen
	o.mu.Unlock()

	// The object may go now, which is what makes an open the confirmation:
	// the reader already holds the bytes, and a second open would not find them.
	if after != nil {
		after(name)
	}

	return opened, nil
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

// fakePGOClock is the clock the publisher, the on-demand bucket,
// and the wait of a Collection read run on,
// so no test waits on wall-clock time.
// Moving it fires every timer whose time has come,
// which is how a test drives a wait to its deadline.
type fakePGOClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakePGOTimer
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
	for _, t := range c.timers {
		if t.active && !t.deadline.After(c.now) {
			t.active = false
			select {
			case t.ch <- c.now:
			default:
			}
		}
	}
}

func (c *fakePGOClock) NewTimer(d time.Duration) pgo.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakePGOTimer{c: c, ch: make(chan time.Time, 1), deadline: c.now.Add(d), active: true}
	c.timers = append(c.timers, t)
	if d <= 0 {
		t.active = false
		t.ch <- c.now
	}

	return t
}

func (c *fakePGOClock) NewTicker(time.Duration) pgo.Ticker { panic("no PGO route uses a ticker") }

// timerCount is how many timers this clock has handed out,
// which is what a test waits for before it moves the clock:
// a timer taken after the move would never fire.
func (c *fakePGOClock) timerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.timers)
}

// fakePGOTimer fires when the clock reaches its deadline.
type fakePGOTimer struct {
	c        *fakePGOClock
	ch       chan time.Time
	deadline time.Time
	active   bool
}

func (t *fakePGOTimer) C() <-chan time.Time { return t.ch }

func (t *fakePGOTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.deadline = t.c.now.Add(d)
	t.active = true

	return was
}

func (t *fakePGOTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false

	return was
}

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
		MaxSampleBytes:       16777216,
		MaxMergedBytes:       33554432,
		MaxTargetsPerRound:   32,
		MaxActiveCollections: 1,
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
		Artifact: config.PGOArtifactDefaults{Retention: 24 * time.Hour},
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

// live is what the harness's caches show for one Service, under the generation the connection is on.
// The generation is read fresh on every call,
// so a predicate polling to convergence follows a move rather than holding the generation it started under.
// Both results are returned because a caller waiting for a Service to go quiet needs them apart:
// a cache that has not replayed under that generation answers no, which is not the same answer as not live,
// and folding the two would end such a wait on a cache that has said nothing.
func (p *pgoHarness) live(namespace, service string) (live, ok bool) {
	return p.caches.Live(p.nats.Generation(), namespace, service)
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
		views, _, _ := p.caches.Collections(p.nats.Generation(), rec.Namespace, rec.Service, pgo.CollectionQuery{})
		for _, v := range views {
			if v.ID == rec.ID {
				return true
			}
		}

		return false
	})

	return rec
}

// dropRecord deletes one Collection record from the authoritative bucket,
// standing in for the sweeper, and waits for the cache to lose it.
func (p *pgoHarness) dropRecord(t *testing.T, id string) {
	t.Helper()

	p.nats.jobs.remove(t, jobKeyPrefix+id)
	p.waitCache(t, "collection "+id+" to go", func() bool { return p.cachedState(id) == "" })
}

// seedActive writes the active key of one Service, which is what a live
// Collection holds.
func (p *pgoHarness) seedActive(t *testing.T, namespace, service, id string) {
	t.Helper()

	p.nats.jobs.put(t, activeKeyPrefix+namespace+"."+service,
		map[string]any{"id": id, "createdAt": pgoFixtureNow})
	p.waitCache(t, "the active key of "+service, func() bool {
		live, ok := p.live(namespace, service)

		return ok && live
	})
}

// seedOverride writes one Service's stored policy override and returns its revision.
func (p *pgoHarness) seedOverride(t *testing.T, override *pgo.PolicyOverride) uint64 {
	t.Helper()

	revision := p.nats.config.put(t, overrideKeyPrefix+fixtureNamespace+"."+fixtureService,
		pgo.StoredOverride{Policy: override, UpdatedBy: "anonymous", UpdatedAt: pgoFixtureNow})
	p.waitCache(t, "the policy override", func() bool {
		_, rev, _ := p.caches.Override(p.nats.Generation(), fixtureNamespace, fixtureService)

		return rev == revision
	})

	return revision
}

// cachedState is the state the watched job cache holds for one Collection,
// and the empty state for one it does not hold.
func (p *pgoHarness) cachedState(id string) pgo.State {
	views, _, _ := p.caches.Collections(p.nats.Generation(), fixtureNamespace, fixtureService, pgo.CollectionQuery{})
	for _, v := range views {
		if v.ID == id {
			return v.State
		}
	}

	return ""
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

// heldOpenTimeout is how long a test gives a held-open request to answer,
// once what it waits for has happened.
// Nothing in these tests waits on wall-clock time,
// so reaching it is a failure rather than a slow machine.
const heldOpenTimeout = 5 * time.Second

// generationHook is the seam a test moves the store generation through, mid-request.
// The PGO step takes one session before the principal is resolved,
// so an authenticator that runs first is inside a request that has bound its generation and has not yet read a cache.
type generationHook struct {
	next auth.Authenticator
	once sync.Once
	gap  func()
}

func (g *generationHook) Authenticate(
	ctx context.Context, r *http.Request, cfg *config.Config,
) (auth.Principal, error) {
	g.once.Do(g.gap)

	return g.next.Authenticate(ctx, r, cfg)
}

// moveGenerationDuringRequest arranges the gap a session's generation exists for.
// On the next request, once its session is taken and before it reads a cache,
// the connection moves the store generation and gap rebuilds the caches under the new one.
// Every later request runs with the caches as that rebuild left them.
func (p *pgoHarness) moveGenerationDuringRequest(t *testing.T, gap func(*testing.T)) {
	t.Helper()

	var next auth.Authenticator = auth.Disabled{}
	if p.auth != nil {
		next = p.auth
	}
	p.auth = &generationHook{next: next, gap: func() {
		p.disconnect()
		gap(t)
	}}
}

// rebuildJobCaches deletes the named keys from the job bucket and rebuilds the caches over it,
// which is what a replay under a moved generation leaves behind when the gap took those keys away.
// The failed Collection written last is the sentinel the wait rests on:
// its arrival proves the delete, the replay, and the marker ahead of it were all applied.
// A failed record is no candidate for the latest walk and leaves the Service not live,
// so it changes nothing any route under test decides.
func (p *pgoHarness) rebuildJobCaches(t *testing.T, keys ...string) {
	t.Helper()

	for _, key := range keys {
		p.nats.jobs.remove(t, key)
	}
	p.nats.jobs.reopenWatches()
	sentinel := p.newRecord(pgo.StateFailed)
	p.nats.jobs.put(t, jobKeyPrefix+sentinel.ID, sentinel)
	p.waitCache(t, "the rebuilt job caches", func() bool { return p.cachedState(sentinel.ID) != "" })
}

// rebuildOverrideCache deletes the fixture Service's stored override and rebuilds the cache over the config bucket.
// Another Service's override, written last, is the sentinel the wait rests on.
// The job bucket is untouched, so a request whose override read is guarded is refused there
// and one whose is not reaches the checks that follow it.
func (p *pgoHarness) rebuildOverrideCache(t *testing.T) {
	t.Helper()

	p.nats.config.remove(t, overrideKeyPrefix+fixtureNamespace+"."+fixtureService)
	p.nats.config.reopenWatches()
	p.nats.config.put(t, overrideKeyPrefix+"other.other-api",
		pgo.StoredOverride{Policy: &pgo.PolicyOverride{}, UpdatedBy: "anonymous", UpdatedAt: pgoFixtureNow})
	p.waitCache(t, "the rebuilt override cache", func() bool {
		_, revision, _ := p.caches.Override(p.nats.Generation(), "other", "other-api")

		return revision != 0
	})
}

// disconnect is the connection going down as the process sees it:
// the generation moves, and the broadcast a held-open request selects on moves with it.
// The harness plays both reports the seam makes, because it stands in for the seam rather than for a caller of it.
func (p *pgoHarness) disconnect() {
	p.nats.disconnect()
	p.runtime.MoveGeneration()
}

// startDrain is the replica beginning to drain, the moment /readyz turns 503.
func (p *pgoHarness) startDrain() { close(p.drain) }

// held is one request the handler has not answered yet:
// the recorder it answers into, the client's own cancellation,
// and the channel closed when the handler returns.
type held struct {
	rec    *httptest.ResponseRecorder
	cancel context.CancelFunc
	done   chan struct{}
}

// hold runs one PGO request on a goroutine of its own
// and hands back what is still in flight,
// so a test can act while the request is parked.
func (p *pgoHarness) hold(t *testing.T, target string) *held {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	h := &held{rec: httptest.NewRecorder(), cancel: cancel, done: make(chan struct{})}
	handler := p.handler()
	go func() {
		defer close(h.done)
		handler.ServeHTTP(h.rec, req)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(heldOpenTimeout):
			t.Errorf("a request was still held open %s after its client left", heldOpenTimeout)
		}
	})

	return h
}

// answered reports whether the handler has returned.
func (h *held) answered() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// join waits for the answer and applies the assertions every PGO answer makes.
func (h *held) join(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	select {
	case <-h.done:
	case <-time.After(heldOpenTimeout):
		t.Fatalf("the request was still held open after %s", heldOpenTimeout)
	}
	assertNoLeak(t, h.rec)

	return h.rec
}

// waitTimer blocks until the held request has taken its deadline from the
// clock; moving the clock before that would leave the timer never firing.
func (p *pgoHarness) waitTimer(t *testing.T) {
	t.Helper()

	p.waitCache(t, "the wait's deadline timer", func() bool { return p.clock.timerCount() > 0 })
}

// keyed is what a create that binds itself to one Collection sends:
// the JSON media type the route requires and the idempotency key.
func keyed(key string) http.Header {
	return http.Header{"Content-Type": []string{jsonMediaType}, idempotencyKeyHeader: []string{key}}
}

// receipt reads one idempotency receipt straight from the authoritative bucket.
func (k *fakeKV) receipt(t *testing.T, key string) pgo.Receipt {
	t.Helper()

	e, err := k.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("receipt %s: %v", key, err)
	}
	var r pgo.Receipt
	if err := json.Unmarshal(e.Value, &r); err != nil {
		t.Fatalf("receipt %s is not readable: %v", key, err)
	}

	return r
}

// expectCode checks the status and the envelope code of one answer.
// It is for a test that makes more than one request,
// which is a test that cannot assert a single audit record.
func expectCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got, _ := errorBodyOf(t, rec); got != code {
		t.Errorf("code = %q, want %q (body %q)", got, code, rec.Body.String())
	}
	detailsOf(t, rec, code)
}

// ifMatch is the header one conditional policy write carries.
func ifMatch(etag string) http.Header { return http.Header{"If-Match": []string{etag}} }

// jsonType is the media type the two write routes require.
func jsonType() http.Header { return http.Header{"Content-Type": []string{"application/json"}} }

// clientHeaders is what an ordinary client sends for one method:
// a POST declares the JSON media type the two write routes require,
// and every other method declares no media type at all.
func clientHeaders(method string) http.Header {
	if method == http.MethodPost {
		return jsonType()
	}

	return nil
}

// storeCalls is how many reads and writes the two buckets have been asked for.
// A snapshot taken once a harness is up moves only when a handler reaches the store,
// so a refusal that is meant to write nothing leaves it where it was.
func (p *pgoHarness) storeCalls() int32 {
	return p.nats.config.calls.Load() + p.nats.jobs.calls.Load()
}

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
	detailsOf(t, rec, code)
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
