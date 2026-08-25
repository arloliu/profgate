package main

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	ownNS            = "profgate-system"
	fixtureNamespace = "payment"
	fixtureService   = "payment-api"
	fixturePod       = "payment-api-1"
	fixtureIP        = "10.0.0.5"

	targetsPath     = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/targets"
	heapPath        = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/profiles/heap"
	collectionsPath = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/collections"

	// pollInterval is how often the waiters re-check their condition.
	pollInterval = 10 * time.Millisecond
	// waitTimeout bounds every wait that does not involve the preflight backoff.
	waitTimeout = 5 * time.Second
	// backoffWaitTimeout bounds a wait across two preflight retries (1s then 2s of backoff).
	backoffWaitTimeout = 15 * time.Second
)

// syncBuffer is a bytes.Buffer safe for the logger and the test to share.
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

// fakeUpstream answers every request with a committed 200 after release is closed.
// It ignores the request context on purpose:
// the drain rows prove the lifecycle's own bound ends the process, not the request budget.
type fakeUpstream struct {
	release chan struct{}
	calls   atomic.Int32
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{release: make(chan struct{})}
}

func (f *fakeUpstream) Do(_ context.Context, w http.ResponseWriter, _ proxy.Request) proxy.Outcome {
	f.calls.Add(1)
	<-f.release
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("profile-bytes"))

	return proxy.Outcome{Code: "ok", Status: http.StatusOK, Committed: true}
}

// recorder remembers every DiscoverySynced and NATSConnected call and ignores the rest.
type recorder struct {
	metrics.Noop
	mu        sync.Mutex
	synced    []bool
	connected []bool
}

func (r *recorder) DiscoverySynced(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synced = append(r.synced, v)
}

func (r *recorder) syncedCalls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]bool(nil), r.synced...)
}

func (r *recorder) NATSConnected(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = append(r.connected, up)
}

func (r *recorder) connectedCalls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]bool(nil), r.connected...)
}

// fixtureObjects is one Service with one ready Pod behind one EndpointSlice,
// so a profile request resolves, confirms, and reaches the upstream.
func fixtureObjects() []runtime.Object {
	const uid = types.UID("u1")

	return []runtime.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: fixtureService},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "payment"}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fixtureNamespace,
				Name:      fixturePod,
				UID:       uid,
				Labels:    map[string]string{"app": "payment", "app.kubernetes.io/version": "1.0"},
			},
			Spec: corev1.PodSpec{NodeName: "worker-1"},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				PodIPs:     []corev1.PodIP{{IP: fixtureIP}},
			},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fixtureNamespace,
				Name:      "payment-api-abc",
				Labels:    map[string]string{discoveryv1.LabelServiceName: fixtureService},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{fixtureIP},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod", Namespace: fixtureNamespace, Name: fixturePod, UID: uid,
				},
			}},
		},
	}
}

func ptr[T any](v T) *T { return &v }

// forbidden is the 403 the API server returns for a missing RBAC tuple.
func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
}

// denyWatch answers every watch on resource with a 403.
func denyWatch(cs *fake.Clientset, resource string) {
	cs.PrependWatchReactor(resource, func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, forbidden(resource)
	})
}

// freeAddr reserves and releases a loopback port, returning its host:port.
func freeAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	return addr
}

// limits are the duration and concurrency caps a subtest writes into its configuration.
type limits struct{ cpu, trace, maxConcurrent int }

func defaultLimits() limits { return limits{cpu: 60, trace: 60, maxConcurrent: 16} }

// pgoBlock is the top-level configuration a gateway with PGO collection on carries.
// The URL is never dialled: every serve row reaches NATS through the preflight seam.
const pgoBlock = `nats:
  url: "nats://127.0.0.1:4222"
pgo:
  enabled: true
`

// writeConfig writes a valid configuration listening on the two addresses.
// The realm always carries every PGO flag,
// so a route that answers 501 or 503 answers it for the reason under test and not for the realm.
func writeConfig(t *testing.T, listen, opsListen string, l limits, enablePGO bool) string {
	t.Helper()

	body := fmt.Sprintf(`server:
  listen: %q
  opsListen: %q
discovery:
  versionLabel: app.kubernetes.io/version
  pprof:
    port: 6060
limits:
  cpuSeconds: %d
  traceSeconds: %d
  maxConcurrentProfiles: %d
auth:
  mode: disabled
  anonymousRealm: developer
realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
    pgo:
      read: true
      collect: true
      configure: true
`, listen, opsListen, l.cpu, l.trace, l.maxConcurrent)
	if enablePGO {
		body += pgoBlock
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// namespaceFile writes the projected namespace file a subtest's runtime reads.
func namespaceFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(path, []byte(ownNS+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// gateway is one running serve call and everything a subtest needs to observe it.
type gateway struct {
	apiAddr string
	opsAddr string
	stdout  *syncBuffer
	stderr  *syncBuffer
	up      *fakeUpstream
	rec     *recorder
	stop    chan struct{}
	exited  chan int
}

// pgoOpts turns PGO collection on for one gateway
// and carries the seams the lifecycle rows drive it through: the NATS preflight and the Collection worker.
type pgoOpts struct {
	enabled   bool
	preflight *preflightStub
	worker    *stubWorker
}

// startGateway runs serve over cs with PGO off.
func startGateway(t *testing.T, cs *fake.Clientset, l limits) *gateway {
	t.Helper()

	return startGatewayWith(t, cs, l, pgoOpts{})
}

// startGatewayWith runs serve over cs in a goroutine and returns once it is running.
// The subtest stops it through gw.stop;
// cleanup stops it anyway, releases the upstream, and releases the worker drain,
// so a failed assertion reports itself instead of timing out.
func startGatewayWith(t *testing.T, cs *fake.Clientset, l limits, o pgoOpts) *gateway {
	t.Helper()

	gw := &gateway{
		apiAddr: freeAddr(t),
		opsAddr: freeAddr(t),
		stdout:  &syncBuffer{},
		stderr:  &syncBuffer{},
		up:      newFakeUpstream(),
		rec:     &recorder{},
		stop:    make(chan struct{}),
		exited:  make(chan int, 1),
	}
	cfgPath := writeConfig(t, gw.apiAddr, gw.opsAddr, l, o.enabled)
	registry := prometheus.NewRegistry()
	deps := serveDeps{
		namespaceFile: namespaceFile(t),
		runtime: func(opts k8s.Options) (k8s.Runtime, error) {
			return k8s.NewRuntimeWithClientset(cs, opts), nil
		},
		upstream: gw.up,
		sampler:  proxy.New(proxy.Options{}),
		registry: registry,
		recorder: gw.rec,
		stop:     gw.stop,
	}
	if o.preflight != nil {
		deps.natsPreflight = o.preflight.fn()
	}
	if o.worker != nil {
		deps.pgoWorker = o.worker
	}
	go func() { gw.exited <- serve(context.Background(), cfgPath, deps, gw.stdout, gw.stderr) }()
	t.Cleanup(func() {
		gw.stopOnce()
		gw.releaseOnce()
		if o.worker != nil {
			o.worker.releaseDrain()
		}
		select {
		case <-gw.exited:
		case <-time.After(time.Minute):
			t.Errorf("serve did not exit after stop; stdout:\n%s\nstderr:\n%s", gw.stdout.String(), gw.stderr.String())
		}
	})

	return gw
}

func (gw *gateway) stopOnce() {
	select {
	case <-gw.stop:
	default:
		close(gw.stop)
	}
}

func (gw *gateway) releaseOnce() {
	select {
	case <-gw.up.release:
	default:
		close(gw.up.release)
	}
}

// exitCode waits for serve to return.
func (gw *gateway) exitCode(t *testing.T, within time.Duration) int {
	t.Helper()

	select {
	case code := <-gw.exited:
		gw.exited <- code

		return code
	case <-time.After(within):
		t.Fatalf("serve did not exit within %s; stdout:\n%s", within, gw.stdout.String())

		return -1
	}
}

// records parses every JSON line serve wrote to stdout.
func (gw *gateway) records(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(gw.stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stdout line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}

	return out
}

// get runs one GET against addr and returns the status and body; a dial failure is returned as err.
func get(addr, path string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()

	return getCtx(ctx, addr, path)
}

// getCtx is get under the caller's context, for requests that outlive waitTimeout.
func getCtx(ctx context.Context, addr, path string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}

	return resp.StatusCode, string(body), nil
}

// mustGet is get for a listener that must be up.
func mustGet(t *testing.T, addr, path string) (int, string) {
	t.Helper()

	code, body, err := get(addr, path)
	if err != nil {
		t.Fatalf("GET %s%s: %v", addr, path, err)
	}

	return code, body
}

// refuses reports whether nothing accepts connections on addr.
func refuses(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()

		return false
	}

	return true
}

// waitFor polls pred until it holds or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, pred func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, timeout)
		}
		time.Sleep(pollInterval)
	}
}

// waitStatus polls addr+path until it answers want.
func (gw *gateway) waitStatus(t *testing.T, timeout time.Duration, addr, path string, want int) {
	t.Helper()

	waitFor(t, timeout, fmt.Sprintf("%s%s answering %d", addr, path, want), func() bool {
		code, _, err := get(addr, path)

		return err == nil && code == want
	})
}

// waitReady waits until the ops listener reports ready.
func (gw *gateway) waitReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	gw.waitStatus(t, timeout, gw.opsAddr, "/readyz", http.StatusOK)
}

func TestServe(t *testing.T) {
	t.Run("warning on disabled auth", func(t *testing.T) {
		gw := startGateway(t, fake.NewClientset(), defaultLimits())
		gw.waitReady(t, waitTimeout)
		for _, rec := range gw.records(t) {
			if rec["level"] == "WARN" && strings.Contains(fmt.Sprint(rec["msg"]), "authentication disabled") {
				return
			}
		}
		t.Fatalf("no WARN record about disabled authentication in stdout:\n%s", gw.stdout.String())
	})

	t.Run("logger is JSON on stdout", func(t *testing.T) {
		gw := startGateway(t, fake.NewClientset(), defaultLimits())
		gw.waitReady(t, waitTimeout)
		mustGet(t, gw.apiAddr, targetsPath)
		gw.stopOnce()
		gw.exitCode(t, waitTimeout)
		if got := gw.records(t); len(got) == 0 {
			t.Fatal("stdout holds no records")
		}
		if got := gw.stderr.String(); got != "" {
			t.Fatalf("stderr = %q, want nothing: operational output goes to stdout as JSON", got)
		}
	})

	t.Run("preflight forbidden", func(t *testing.T) {
		cs := fake.NewClientset()
		denyWatch(cs, "pods")
		gw := startGateway(t, cs, defaultLimits())
		if code := gw.exitCode(t, waitTimeout); code != 1 {
			t.Fatalf("exit code = %d, want 1: an under-privileged ClusterRole is a crash", code)
		}
		for _, rec := range gw.records(t) {
			if rec["resource"] == "pods" && rec["verb"] == "watch" {
				return
			}
		}
		t.Fatalf("no record naming resource=pods verb=watch in stdout:\n%s", gw.stdout.String())
	})

	t.Run("preflight transient then ok", func(t *testing.T) {
		cs := fake.NewClientset()
		var lists atomic.Int32
		cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			if lists.Add(1) <= 2 {
				return true, nil, errors.New("api server hiccup")
			}

			return false, nil, nil
		})
		gw := startGateway(t, cs, defaultLimits())
		gw.waitReady(t, backoffWaitTimeout)
		var attempts int
		for _, rec := range gw.records(t) {
			if rec["msg"] == "preflight attempt" {
				if _, ok := rec["error"]; !ok {
					t.Fatalf("preflight attempt record lacks error: %v", rec)
				}
				attempts++
			}
		}
		if attempts != 2 {
			t.Fatalf("preflight attempt records = %d, want 2:\n%s", attempts, gw.stdout.String())
		}
	})

	t.Run("responses during preflight", func(t *testing.T) {
		cs := fake.NewClientset()
		release := make(chan struct{})
		cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			<-release

			return false, nil, nil
		})
		gw := startGateway(t, cs, defaultLimits())
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/healthz", http.StatusOK)
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz during preflight = %d, want 503", code)
		}
		code, body := mustGet(t, gw.apiAddr, targetsPath)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("targets during preflight = %d %q, want 503 not_ready", code, body)
		}
		close(release)
		gw.waitReady(t, waitTimeout)
	})

	t.Run("preflight forbidden closes listeners", func(t *testing.T) {
		cs := fake.NewClientset()
		denyWatch(cs, "pods")
		gw := startGateway(t, cs, defaultLimits())
		gw.exitCode(t, waitTimeout)
		for _, addr := range []string{gw.apiAddr, gw.opsAddr} {
			if !refuses(addr) {
				t.Fatalf("%s still accepts connections after exit", addr)
			}
		}
	})

	t.Run("synced gauge", func(t *testing.T) {
		gw := startGateway(t, fake.NewClientset(), defaultLimits())
		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "DiscoverySynced(true)", func() bool { return len(gw.rec.syncedCalls()) > 0 })
		if got := gw.rec.syncedCalls(); len(got) != 1 || !got[0] {
			t.Fatalf("DiscoverySynced calls = %v, want exactly [true]", got)
		}
	})

	t.Run("drain", func(t *testing.T) {
		gw := startGateway(t, fake.NewClientset(fixtureObjects()...), defaultLimits())
		gw.waitReady(t, waitTimeout)
		done := make(chan error, 1)
		go func() {
			code, _, err := get(gw.apiAddr, heapPath)
			if err == nil && code != http.StatusOK {
				err = fmt.Errorf("profile status = %d, want 200", code)
			}
			done <- err
		}()
		waitFor(t, waitTimeout, "the profile request reaching the upstream", func() bool { return gw.up.calls.Load() == 1 })

		gw.stopOnce()
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/readyz", http.StatusServiceUnavailable)
		waitFor(t, waitTimeout, "the API listener refusing connections", func() bool { return refuses(gw.apiAddr) })
		select {
		case <-gw.exited:
			t.Fatal("serve exited while a profile request was still in flight")
		default:
		}

		gw.releaseOnce()
		if code := gw.exitCode(t, waitTimeout); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if err := <-done; err != nil {
			t.Fatalf("the in-flight profile did not finish cleanly: %v", err)
		}
	})

	t.Run("drain bound", func(t *testing.T) {
		gw := startGateway(t, fake.NewClientset(fixtureObjects()...), limits{cpu: 1, trace: 1, maxConcurrent: 1})
		gw.waitReady(t, waitTimeout)
		go func() { _, _, _ = getCtx(context.Background(), gw.apiAddr, heapPath) }()
		waitFor(t, waitTimeout, "the profile request reaching the upstream", func() bool { return gw.up.calls.Load() == 1 })

		start := time.Now()
		gw.stopOnce()
		code := gw.exitCode(t, 40*time.Second)
		elapsed := time.Since(start)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if elapsed < 30*time.Second || elapsed > 35*time.Second {
			t.Fatalf("serve exited after %s, want max(cpu,trace)+30s = 31s: the drain waits for the bound and then closes", elapsed)
		}
	})
}

// emptyKV is a bucket holding nothing.
// The PGO serve rows read store state, they never write it,
// so every mutation is the unavailability a caller would see with the bucket unreachable.
type emptyKV struct{}

func (emptyKV) Get(context.Context, string) (natskv.Entry, error) {
	return natskv.Entry{}, natskv.ErrKeyNotFound
}

func (emptyKV) Create(context.Context, string, []byte) (uint64, error) {
	return 0, natskv.ErrUnavailable
}

func (emptyKV) Update(context.Context, string, []byte, uint64) (uint64, error) {
	return 0, natskv.ErrUnavailable
}

func (emptyKV) Delete(context.Context, string, uint64) error { return natskv.ErrUnavailable }

func (emptyKV) Keys(context.Context, string) ([]string, error) { return nil, nil }

// Watch delivers the replay marker of an empty prefix and then nothing until the caller's context ends,
// which is what closes the channel.
func (emptyKV) Watch(ctx context.Context, _ string) (<-chan natskv.Entry, error) {
	ch := make(chan natskv.Entry, 1)
	ch <- natskv.Entry{Synced: true, Generation: fakeGeneration}
	go func() {
		<-ctx.Done()
		close(ch)
	}()

	return ch, nil
}

// emptyObjects is an Object Store holding nothing.
type emptyObjects struct{}

func (emptyObjects) Put(context.Context, string, io.Reader) error { return natskv.ErrUnavailable }

func (emptyObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, natskv.ErrObjectNotFound
}

func (emptyObjects) Delete(context.Context, string) error { return natskv.ErrUnavailable }

func (emptyObjects) List(context.Context) ([]natskv.ObjectInfo, error) { return nil, nil }

// fakeGeneration is the one connection generation every PGO serve row runs under.
const fakeGeneration = uint64(1)

// fakeNATS is the connection the preflight seam hands back:
// empty buckets, and a replay barrier the subtest opens when it chooses,
// so a row can hold the gateway between "preflight passed" and "the watches have replayed".
type fakeNATS struct {
	synced atomic.Bool
	stores natskv.Stores
}

func newFakeNATS(synced bool) *fakeNATS {
	f := &fakeNATS{stores: natskv.Stores{Config: emptyKV{}, Jobs: emptyKV{}, Artifacts: emptyObjects{}}}
	f.synced.Store(synced)

	return f
}

func (f *fakeNATS) Connected() bool    { return true }
func (f *fakeNATS) Generation() uint64 { return fakeGeneration }
func (f *fakeNATS) Synced(uint64) bool { return f.synced.Load() }

func (f *fakeNATS) View(gen uint64) (natskv.Stores, error) {
	if gen != fakeGeneration {
		return natskv.Stores{}, natskv.ErrUnavailable
	}

	return f.stores, nil
}

// preflightResult is what one preflight attempt answers.
type preflightResult struct {
	client natskv.Client
	err    error
}

// preflightStub answers the NATS preflight with a scripted sequence, repeating its last result,
// and counts the attempts the lifecycle made.
type preflightStub struct {
	mu      sync.Mutex
	calls   int
	results []preflightResult
}

func newPreflightStub(results ...preflightResult) *preflightStub {
	return &preflightStub{results: results}
}

func (p *preflightStub) fn() natsPreflightFunc {
	return func(_ context.Context, opts natskv.Options, _ string, _ *slog.Logger) (natskv.Client, error) {
		p.mu.Lock()
		res := p.results[min(p.calls, len(p.results)-1)]
		p.calls++
		p.mu.Unlock()

		if res.err != nil {
			return nil, res.err
		}
		// natskv.Preflight reports the connection before it probes;
		// without that call the connected gauge would read zero until the first reconnect,
		// so the stub owes the caller the same report.
		if opts.OnConnectionChange != nil {
			opts.OnConnectionChange(true)
		}

		return res.client, nil
	}
}

func (p *preflightStub) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

// stubWorker stands in for the Collection worker
// so the shutdown rows can see which context each of the two waits was given.
// The real worker's per-Collection drain bound is proved where it lives, in internal/pgo;
// what serve owes it is a context of its own.
type stubWorker struct {
	ran      chan struct{}
	runEnded chan struct{}
	entered  chan struct{}
	release  chan struct{}

	mu            sync.Mutex
	runs          int
	drainDone     bool  // the drain context had a Done channel
	drainErr      error // the drain context's error when Drain was entered
	drainDeadline bool  // the drain context carried a deadline
}

func newStubWorker() *stubWorker {
	return &stubWorker{
		ran:      make(chan struct{}),
		runEnded: make(chan struct{}),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
}

// Run claims until the lifecycle stops claiming.
func (s *stubWorker) Run(ctx context.Context) {
	s.mu.Lock()
	s.runs++
	first := s.runs == 1
	s.mu.Unlock()
	if first {
		close(s.ran)
	}
	<-ctx.Done()
	close(s.runEnded)
}

// Drain records the context it was handed and blocks until the subtest releases it,
// standing in for a Collection that is still merging.
func (s *stubWorker) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.drainDone = ctx.Done() != nil
	s.drainErr = ctx.Err()
	_, s.drainDeadline = ctx.Deadline()
	s.mu.Unlock()
	close(s.entered)

	select {
	case <-s.release:
	case <-ctx.Done():
	}

	return nil
}

func (s *stubWorker) runCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.runs
}

func (s *stubWorker) drainContext() (done bool, deadline bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.drainDone, s.drainDeadline, s.drainErr
}

func (s *stubWorker) releaseDrain() {
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

// closed reports whether ch has been closed, without blocking on it.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// pgoCode returns the status and error code a PGO route answered with.
func pgoCode(t *testing.T, addr, path string) (int, string) {
	t.Helper()

	status, body := mustGet(t, addr, path)
	var parsed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return status, body
	}

	return status, parsed.Code
}

func TestServePGO(t *testing.T) {
	t.Run("disabled reaches no NATS and answers 501", func(t *testing.T) {
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: false, preflight: pf})
		gw.waitReady(t, waitTimeout)

		if status, code := pgoCode(t, gw.apiAddr, collectionsPath); status != http.StatusNotImplemented || code != "pgo_disabled" {
			t.Fatalf("collections with pgo off = %d %q, want 501 pgo_disabled", status, code)
		}
		if got := pf.callCount(); got != 0 {
			t.Fatalf("NATS preflight attempts = %d, want 0: pgo.enabled false constructs nothing NATS-related", got)
		}
	})

	t.Run("nats down leaves readyz 503 and the interactive routes serving", func(t *testing.T) {
		pf := newPreflightStub(preflightResult{err: fmt.Errorf("dial nats: %w", natskv.ErrUnavailable)})
		worker := newStubWorker()
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: true, preflight: pf, worker: worker})

		waitFor(t, waitTimeout, "the targets route answering 200", func() bool {
			code, _, err := get(gw.apiAddr, targetsPath)

			return err == nil && code == http.StatusOK
		})
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz with NATS down = %d, want 503: readyz requires the NATS preflight to have passed", code)
		}
		if status, code := pgoCode(t, gw.apiAddr, collectionsPath); status != http.StatusServiceUnavailable || code != "pgo_unavailable" {
			t.Fatalf("collections with NATS down = %d %q, want 503 pgo_unavailable", status, code)
		}
		if got := worker.runCount(); got != 0 {
			t.Fatalf("worker started %d times before the preflight passed, want 0", got)
		}
	})

	t.Run("a connection failure is retried until it passes", func(t *testing.T) {
		down := preflightResult{err: fmt.Errorf("dial nats: %w", natskv.ErrUnavailable)}
		pf := newPreflightStub(down, down, preflightResult{client: newFakeNATS(true)})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: true, preflight: pf, worker: newStubWorker()})

		gw.waitReady(t, backoffWaitTimeout)
		var attempts int
		for _, rec := range gw.records(t) {
			if rec["msg"] == "nats preflight attempt" {
				attempts++
			}
		}
		if attempts != 2 {
			t.Fatalf("nats preflight attempt records = %d, want 2:\n%s", attempts, gw.stdout.String())
		}
	})

	t.Run("a contract violation ends startup naming the bucket and field", func(t *testing.T) {
		violation := errors.New("nats preflight: bucket PROFGATE_JOBS: field TTL is 1h0m0s, the contract requires no TTL")
		pf := newPreflightStub(preflightResult{err: violation})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: true, preflight: pf, worker: newStubWorker()})

		if code := gw.exitCode(t, waitTimeout); code != 1 {
			t.Fatalf("exit code = %d, want 1: a bucket outside the contract is a crash", code)
		}
		if got := pf.callCount(); got != 1 {
			t.Fatalf("NATS preflight attempts = %d, want 1: only a connection failure is retried", got)
		}
		var named bool
		for _, rec := range gw.records(t) {
			text := fmt.Sprint(rec["error"])
			if strings.Contains(text, "PROFGATE_JOBS") && strings.Contains(text, "field TTL") {
				named = true
			}
		}
		if !named {
			t.Fatalf("no record naming the bucket and the field in stdout:\n%s", gw.stdout.String())
		}
	})

	t.Run("readyz does not wait for the replay barrier", func(t *testing.T) {
		nats := newFakeNATS(false)
		pf := newPreflightStub(preflightResult{client: nats})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: true, preflight: pf, worker: newStubWorker()})

		gw.waitReady(t, waitTimeout)
		if status, code := pgoCode(t, gw.apiAddr, collectionsPath); status != http.StatusServiceUnavailable || code != "pgo_unavailable" {
			t.Fatalf("collections while the watches replay = %d %q, want 503 pgo_unavailable", status, code)
		}
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusOK {
			t.Fatalf("/readyz while the watches replay = %d, want 200: a replaying replica still serves interactive requests", code)
		}

		nats.synced.Store(true)
		waitFor(t, waitTimeout, "the collections route passing the barrier", func() bool {
			code, _, err := get(gw.apiAddr, collectionsPath)

			return err == nil && code == http.StatusOK
		})
	})

	t.Run("the connected gauge reports the initial connection", func(t *testing.T) {
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: true, preflight: pf, worker: newStubWorker()})

		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "NATSConnected(true)", func() bool { return len(gw.rec.connectedCalls()) > 0 })
		if got := gw.rec.connectedCalls(); len(got) != 1 || !got[0] {
			t.Fatalf("NATSConnected calls = %v, want exactly [true]", got)
		}
	})

	t.Run("shutdown stops claiming and drains the two waits separately", func(t *testing.T) {
		worker := newStubWorker()
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			pgoOpts{enabled: true, preflight: pf, worker: worker})
		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "the worker starting", func() bool { return worker.runCount() == 1 })

		// One interactive profile is held open, so the API drain cannot finish
		// while the worker drain is observed.
		go func() { _, _, _ = getCtx(context.Background(), gw.apiAddr, heapPath) }()
		waitFor(t, waitTimeout, "the profile request reaching the upstream", func() bool { return gw.up.calls.Load() == 1 })

		gw.stopOnce()
		waitFor(t, waitTimeout, "the worker drain starting", func() bool { return closed(worker.entered) })
		if !closed(worker.runEnded) {
			t.Fatal("the worker was still claiming after the stop request: SIGTERM cancels the claiming context at once")
		}
		done, deadline, err := worker.drainContext()
		if done || deadline || err != nil {
			t.Fatalf("Drain context: done=%v deadline=%v err=%v, want a context of its own that nothing cuts off: "+
				"a Collection deadline can far exceed the interactive drain bound", done, deadline, err)
		}

		// The API drain finishes on its own bound while the worker drain holds.
		gw.releaseOnce()
		waitFor(t, waitTimeout, "the API listener refusing connections", func() bool { return refuses(gw.apiAddr) })
		select {
		case <-gw.exited:
			t.Fatal("serve exited while the worker was still draining")
		default:
		}

		worker.releaseDrain()
		if code := gw.exitCode(t, waitTimeout); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	})
}
