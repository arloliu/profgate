package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/bcrypt"
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
	"github.com/arloliu/profgate/internal/pgo"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	ownNS            = "profgate-system"
	fixtureNamespace = "payment"
	fixtureService   = "payment-api"
	fixturePod       = "payment-api-1"
	fixtureIP        = "10.0.0.5"
	secondPod        = "payment-api-2"
	secondIP         = "10.0.0.6"

	targetsPath     = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/targets"
	heapPath        = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/profiles/heap"
	collectionsPath = "/v1/namespaces/" + fixtureNamespace + "/services/" + fixtureService + "/collections"
	whoamiPath      = "/v1/whoami"
	namespacesPath  = "/v1/namespaces"

	// uiPath is where the console's shell is served, and what "/" redirects to.
	uiPath = "/ui/"

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

// recorder remembers every DiscoverySynced, NATSConnected, and TLS call and ignores the rest.
type recorder struct {
	metrics.Noop
	mu        sync.Mutex
	synced    []bool
	connected []bool
	reloads   []string
	expiry    time.Time
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

func (r *recorder) TLSReload(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloads = append(r.reloads, result)
}

func (r *recorder) TLSCertificateExpiry(notAfter time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expiry = notAfter
}

// reloadCalls returns the certificate reload results recorded so far.
func (r *recorder) reloadCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.reloads...)
}

// certificateExpiry returns the expiry last recorded.
func (r *recorder) certificateExpiry() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.expiry
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

// addSecondPod adds one more ready Pod to the fixture Service and lists it in
// the EndpointSlice, which is a discovery change only a running informer sees.
func addSecondPod(t *testing.T, cs *fake.Clientset) {
	t.Helper()

	const uid = types.UID("u2")
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixtureNamespace,
			Name:      secondPod,
			UID:       uid,
			Labels:    map[string]string{"app": "payment", "app.kubernetes.io/version": "1.0"},
		},
		Spec: corev1.PodSpec{NodeName: "worker-2"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			PodIPs:     []corev1.PodIP{{IP: secondIP}},
		},
	}
	if _, err := cs.CoreV1().Pods(fixtureNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s: %v", secondPod, err)
	}

	slice, err := cs.DiscoveryV1().EndpointSlices(fixtureNamespace).Get(ctx, "payment-api-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the EndpointSlice: %v", err)
	}
	slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
		Addresses:  []string{secondIP},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
		TargetRef: &corev1.ObjectReference{
			Kind: "Pod", Namespace: fixtureNamespace, Name: secondPod, UID: uid,
		},
	})
	if _, err := cs.DiscoveryV1().EndpointSlices(fixtureNamespace).Update(ctx, slice, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update the EndpointSlice: %v", err)
	}
}

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

// reserveAddr opens a loopback listener and keeps it open,
// so nothing else can take the port between here and the moment serve starts accepting on it.
// The listener is handed to serve through deps.listen, never closed and reopened.
// A port released to pick its number is a port another process can bind,
// or draw as an ephemeral source port,
// and serve would then exit on a bind error
// while the test waits out its readiness timeout with no clue why.
func reserveAddr(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	return l
}

// failingListener is a real listener whose Accept fails for good once the
// subtest trips it, standing in for the listener failure that ends the process.
// The goroutine each pending Accept leaves behind returns when the lifecycle
// closes the listener.
type failingListener struct {
	net.Listener
	fail chan struct{}
}

// errListenerFailed is what a tripped listener answers Accept with.
var errListenerFailed = errors.New("listener failed for good")

func (l *failingListener) Accept() (net.Conn, error) {
	type accepted struct {
		conn net.Conn
		err  error
	}
	next := make(chan accepted, 1)
	go func() {
		conn, err := l.Listener.Accept()
		next <- accepted{conn: conn, err: err}
	}()

	select {
	case <-l.fail:
		return nil, errListenerFailed
	case a := <-next:
		return a.conn, a.err
	}
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
func writeConfig(t *testing.T, listen, opsListen string, l limits, o gatewayOpts) string {
	t.Helper()

	tlsBlock := ""
	if o.tlsDir != "" {
		tlsBlock = fmt.Sprintf("  tls:\n    certFile: %q\n    keyFile: %q\n",
			filepath.Join(o.tlsDir, "tls.crt"), filepath.Join(o.tlsDir, "tls.key"))
	}
	authBlock := o.authBlock
	if authBlock == "" {
		authBlock = "auth:\n  mode: disabled\n  anonymousRealm: developer\n"
	}
	body := fmt.Sprintf(`server:
  listen: %q
  opsListen: %q
  drainDelay: %s
%sdiscovery:
  versionLabel: app.kubernetes.io/version
  pprof:
    port: 6060
limits:
  cpuSeconds: %d
  traceSeconds: %d
  maxConcurrentProfiles: %d
%srealms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
    pgo:
      read: true
      collect: true
      configure: true
`, listen, opsListen, o.drainDelay.String(), tlsBlock, l.cpu, l.trace, l.maxConcurrent, authBlock)
	if o.enabled {
		body += pgoBlock
	}
	if o.uiEnabled {
		body += "ui:\n  enabled: true\n"
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

// gatewayOpts is what a subtest varies about the gateway it starts:
// whether PGO collection is on, the seams the lifecycle rows drive it through
// -- the NATS preflight and the Collection worker -- and the drain delay,
// which every row but the one that proves it leaves at zero
// so a subtest waits for the behavior it is about and not for the window.
type gatewayOpts struct {
	enabled    bool
	preflight  *preflightStub
	worker     *stubWorker
	drainDelay time.Duration
	// failAPI, when set, is closed by the subtest to fail the API listener.
	failAPI chan struct{}
	// tlsDir, when set, holds tls.crt and tls.key and turns the API listener
	// into an HTTPS listener; tlsRefresh is how often they are read again.
	tlsDir     string
	tlsRefresh time.Duration
	// authBlock, when set, is the raw top-level auth block written in place
	// of the disabled one; authPoll is the users-file and cookie-key poll
	// interval, zero meaning the production 30 seconds.
	authBlock string
	authPoll  time.Duration
	// uiEnabled writes ui.enabled: true into the configuration.
	uiEnabled bool
}

// startGateway runs serve over cs with PGO off.
func startGateway(t *testing.T, cs *fake.Clientset, l limits) *gateway {
	t.Helper()

	return startGatewayWith(t, cs, l, gatewayOpts{})
}

// startGatewayWith runs serve over cs in a goroutine and returns once it is running.
// The subtest stops it through gw.stop;
// cleanup stops it anyway, releases the upstream, and releases the worker drain,
// so a failed assertion reports itself instead of timing out.
func startGatewayWith(t *testing.T, cs *fake.Clientset, l limits, o gatewayOpts) *gateway {
	t.Helper()

	apiListener, opsListener := reserveAddr(t), reserveAddr(t)
	gw := &gateway{
		apiAddr: apiListener.Addr().String(),
		opsAddr: opsListener.Addr().String(),
		stdout:  &syncBuffer{},
		stderr:  &syncBuffer{},
		up:      newFakeUpstream(),
		rec:     &recorder{},
		stop:    make(chan struct{}),
		exited:  make(chan int, 1),
	}
	cfgPath := writeConfig(t, gw.apiAddr, gw.opsAddr, l, o)
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
	deps.tlsRefresh = o.tlsRefresh
	deps.authPoll = o.authPoll
	if o.preflight != nil {
		deps.natsPreflight = o.preflight.fn()
	}
	if o.worker != nil {
		deps.pgoWorker = o.worker
	}
	deps.listen = func(_ context.Context, _, address string) (net.Listener, error) {
		switch address {
		case gw.apiAddr:
			if o.failAPI != nil {
				return &failingListener{Listener: apiListener, fail: o.failAPI}, nil
			}

			return apiListener, nil
		case gw.opsAddr:
			return opsListener, nil
		}

		return nil, fmt.Errorf("no listener reserved for %s", address)
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

// record returns the one record serve logged with this msg,
// failing the subtest when there is not exactly one.
func (gw *gateway) record(t *testing.T, msg string) map[string]any {
	t.Helper()

	var found []map[string]any
	for _, rec := range gw.records(t) {
		if rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d records with msg %q, want 1:\n%s", len(found), msg, gw.stdout.String())
	}

	return found[0]
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

// writeSelfSigned writes a fresh certificate and key for 127.0.0.1 into dir,
// replacing whatever was there, and returns a pool that trusts only it.
// A rotation is two calls: the second replaces the files under the running
// gateway the way a renewed Secret does.
func writeSelfSigned(t *testing.T, dir string) *x509.CertPool {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := cryptorand.Int(cryptorand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "profgate"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	write := func(name string, block *pem.Block) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("tls.crt", &pem.Block{Type: "CERTIFICATE", Bytes: der})
	write("tls.key", &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return pool
}

// httpsGet fetches path over TLS, trusting only pool.
// Keep-alives are off so every call completes a handshake of its own,
// which is what makes a rotation observable from the client side.
func httpsGet(addr, path string, pool *x509.CertPool) (int, error) {
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+addr+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, nil
}

func TestServe(t *testing.T) {
	// The one test that completes a real handshake against a bound port: it is
	// the listener serve builds that is under test, and an httptest server
	// cannot stand in for it, because StartTLS fills in Certificates of its
	// own, which suppresses the GetCertificate the rotation depends on for
	// every ClientHello without a server name.
	t.Run("api listener serves https and follows a rotation", func(t *testing.T) {
		dir := t.TempDir()
		first := writeSelfSigned(t, dir)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{tlsDir: dir, tlsRefresh: 10 * time.Millisecond})
		gw.waitReady(t, waitTimeout)

		status, err := httpsGet(gw.apiAddr, targetsPath, first)
		if err != nil || status != http.StatusOK {
			t.Fatalf("https GET %s = %d, %v; want 200 over TLS", targetsPath, status, err)
		}
		// The ops listener is deliberately not TLS: the readiness wait above
		// reached it over plain HTTP.
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusOK {
			t.Errorf("plain GET /readyz on the ops listener = %d, want 200: the ops listener stays plaintext", code)
		}

		_, err = httpsGet(gw.apiAddr, targetsPath, x509.NewCertPool())
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			t.Errorf("https GET with an empty trust store: error = %v, want an unknown-authority failure", err)
		}

		// The rotation: the files change under the running process, and no
		// restart happens.
		second := writeSelfSigned(t, dir)
		waitFor(t, waitTimeout, "the rotated certificate to be served", func() bool {
			status, err := httpsGet(gw.apiAddr, targetsPath, second)

			return err == nil && status == http.StatusOK
		})
		if _, err := httpsGet(gw.apiAddr, targetsPath, first); err == nil {
			t.Error("a client pinning the replaced certificate still succeeded; the swap did not happen")
		}

		if results := gw.rec.reloadCalls(); len(results) < 2 || results[0] != "applied" {
			t.Errorf("reload results = %v, want an applied first result and at least one more", results)
		}
		if gw.rec.certificateExpiry().IsZero() {
			t.Error("no certificate expiry was recorded, so a stalled rotation would be invisible")
		}
	})

	t.Run("certificate that cannot be parsed", func(t *testing.T) {
		dir := t.TempDir()
		writeSelfSigned(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("-----BEGIN EC PRIVATE KEY-----\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gw := startGatewayWith(t, fake.NewClientset(), defaultLimits(), gatewayOpts{tlsDir: dir})

		if code := gw.exitCode(t, waitTimeout); code != 1 {
			t.Fatalf("exit code = %d, want 1: a gateway asked for TLS that cannot serve a certificate has nothing to offer", code)
		}
	})

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

	t.Run("log level suppresses lower records", func(t *testing.T) {
		t.Setenv("PROFGATE_LOG_LEVEL", "error")
		gw := startGateway(t, fake.NewClientset(), defaultLimits())
		gw.waitReady(t, waitTimeout)
		for _, rec := range gw.records(t) {
			if rec["level"] == "WARN" && strings.Contains(fmt.Sprint(rec["msg"]), "authentication disabled") {
				t.Fatalf("WARN record survived server.logLevel error:\n%s", gw.stdout.String())
			}
		}
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
		// What the drain did is the one thing an operator reading the logs
		// after a rollout has to go on.
		if rec := gw.record(t, "drain complete"); rec["api"] != "completed" {
			t.Fatalf("drain complete record = %v, want api=completed: every in-flight request finished", rec)
		}
	})

	t.Run("drain delay holds the api listener open", func(t *testing.T) {
		const delay = 3 * time.Second
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{drainDelay: delay})
		gw.waitReady(t, waitTimeout)

		gw.stopOnce()
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/readyz", http.StatusServiceUnavailable)
		// The endpoint-removal window: readiness is already 503, and a request
		// the routing has not caught up with is still served rather than reset.
		if code, _ := mustGet(t, gw.apiAddr, targetsPath); code != http.StatusOK {
			t.Fatalf("targets during the drain delay = %d, want 200: the API listener closes after the delay, not with readiness", code)
		}
		waitFor(t, 2*delay, "the API listener refusing connections", func() bool { return refuses(gw.apiAddr) })
		if code := gw.exitCode(t, waitTimeout); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	})

	t.Run("discovery keeps resolving through the drain", func(t *testing.T) {
		const delay = 8 * time.Second
		cs := fake.NewClientset(fixtureObjects()...)
		gw := startGatewayWith(t, cs, defaultLimits(), gatewayOpts{drainDelay: delay})
		gw.waitReady(t, waitTimeout)

		gw.stopOnce()
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/readyz", http.StatusServiceUnavailable)
		addSecondPod(t, cs)
		// Discovery outlives the stop request because the drain still needs it:
		// an in-flight Collection re-resolves its targets every round, and a
		// cache frozen at SIGTERM would hand it Pods that have since gone.
		waitFor(t, delay-2*time.Second, "the draining gateway resolving the new Pod", func() bool {
			code, body, err := get(gw.apiAddr, targetsPath)

			return err == nil && code == http.StatusOK && strings.Contains(body, secondPod)
		})
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
		if rec := gw.record(t, "drain complete"); rec["api"] != "deadline_closed" {
			t.Fatalf("drain complete record = %v, want api=deadline_closed: the bound cut the request", rec)
		}
		if got := gw.record(t, "drain deadline passed; closing in-flight connections")["requests"]; got != float64(1) {
			t.Fatalf("the deadline record counted %v requests, want 1", got)
		}
	})

	t.Run("console off", func(t *testing.T) {
		gw := startGateway(t, fake.NewClientset(fixtureObjects()...), defaultLimits())
		gw.waitReady(t, waitTimeout)

		for _, path := range []string{uiPath, "/"} {
			code, body := mustGet(t, gw.apiAddr, path)
			if code != http.StatusNotFound || !strings.Contains(body, `"route_unknown"`) {
				t.Fatalf("GET %s without ui.enabled = %d %q, want 404 route_unknown", path, code, body)
			}
		}
		// The four listing routes exist whether or not the page does: a script
		// reads them from a gateway that serves no console at all.
		code, body := mustGet(t, gw.apiAddr, whoamiPath)
		if code != http.StatusOK {
			t.Fatalf("GET %s without ui.enabled = %d %q, want 200", whoamiPath, code, body)
		}
		var who struct {
			Principal string `json:"principal"`
		}
		if err := json.Unmarshal([]byte(body), &who); err != nil {
			t.Fatalf("whoami body %q is not JSON: %v", body, err)
		}
		if who.Principal != "anonymous" {
			t.Fatalf("whoami principal = %q, want anonymous", who.Principal)
		}
		if recordIndex(gw.records(t), "console enabled") >= 0 {
			t.Fatalf("the console mount was logged without ui.enabled:\n%s", gw.stdout.String())
		}
	})

	t.Run("console on", func(t *testing.T) {
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{uiEnabled: true})
		gw.waitReady(t, waitTimeout)

		shell := request(t, gw.apiAddr, uiPath, nil, nil)
		if shell.status != http.StatusOK || !strings.HasPrefix(shell.header.Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s = %d %q, want 200 text/html", uiPath, shell.status, shell.header.Get("Content-Type"))
		}
		// The policy is what keeps the page to its own origin; a shell served
		// without it is a page the gateway no longer bounds.
		if shell.header.Get("Content-Security-Policy") == "" {
			t.Fatalf("the shell carries no Content-Security-Policy header:\n%v", shell.header)
		}

		// The shell names the asset it loads, and the gateway serves it:
		// the two halves of the console reach the browser through one mount.
		script := shellScript(t, shell.body)
		asset := request(t, gw.apiAddr, script, nil, nil)
		if asset.status != http.StatusOK || !strings.HasPrefix(asset.header.Get("Content-Type"), "text/javascript") {
			t.Fatalf("GET %s = %d %q, want 200 text/javascript", script, asset.status, asset.header.Get("Content-Type"))
		}

		root := request(t, gw.apiAddr, "/", nil, nil)
		if root.status != http.StatusFound || root.header.Get("Location") != uiPath {
			t.Fatalf("GET / = %d Location %q, want 302 to %s", root.status, root.header.Get("Location"), uiPath)
		}
		if got := gw.record(t, "console enabled")["path"]; got != uiPath {
			t.Fatalf("console enabled record path = %v, want %s", got, uiPath)
		}
	})

	t.Run("console on the ops listener", func(t *testing.T) {
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{uiEnabled: true})
		gw.waitReady(t, waitTimeout)

		if code, _ := mustGet(t, gw.opsAddr, uiPath); code != http.StatusNotFound {
			t.Fatalf("GET %s on the ops listener = %d, want 404: the console lives on the API listener only", uiPath, code)
		}
	})

	t.Run("listing before sync", func(t *testing.T) {
		cs := fake.NewClientset(fixtureObjects()...)
		release := make(chan struct{})
		cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			<-release

			return false, nil, nil
		})
		gw := startGatewayWith(t, cs, defaultLimits(), gatewayOpts{uiEnabled: true})
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/healthz", http.StatusOK)

		// The page is bytes the gateway already holds, so it loads while
		// discovery is still coming up and asks again for what it could not read.
		code, body := mustGet(t, gw.apiAddr, namespacesPath)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("GET %s during the preflight = %d %q, want 503 not_ready", namespacesPath, code, body)
		}
		if code, _ := mustGet(t, gw.apiAddr, uiPath); code != http.StatusOK {
			t.Fatalf("GET %s during the preflight = %d, want 200", uiPath, code)
		}
		close(release)
		gw.waitReady(t, waitTimeout)
	})

	t.Run("oidc requires the browser flow", func(t *testing.T) {
		is := newTestIssuer(t)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{authBlock: oidcBlock(t, is, false), uiEnabled: true})

		if code := gw.exitCode(t, waitTimeout); code != 2 {
			t.Fatalf("exit code = %d, want 2: a console that cannot sign a browser in serves nobody", code)
		}
		if !strings.Contains(gw.stderr.String(), "ui.enabled requires auth.oidc.browser") {
			t.Fatalf("stderr = %q, want it to name the rule the configuration broke", gw.stderr.String())
		}
	})
}

// shellScript is the path the shell's module script names.
// It is read out of the shell rather than spelled here,
// so the fetch below asks for what the page would ask for.
func shellScript(t *testing.T, shell string) string {
	t.Helper()

	m := regexp.MustCompile(`<script type="module" src="([^"]+)"`).FindStringSubmatch(shell)
	if m == nil {
		t.Fatalf("the shell names no module script:\n%s", shell)
	}

	return m[1]
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

// waitCollectionID is the Collection a parked wait names.
// It is the identifier grammar the route table admits: twenty Crockford base32 characters.
const waitCollectionID = "abcdefghjkmnpqrstv00"

// oneRecordKV is a bucket holding one Collection record, which is what a
// request held open by a wait needs to exist.
// Nothing ever writes it, so the record never moves and the wait ends only on
// the drain or on its own deadline.
type oneRecordKV struct {
	emptyKV
	key   string
	value []byte
}

func newOneRecordKV(t *testing.T, id string) *oneRecordKV {
	t.Helper()

	value, err := json.Marshal(pgo.Record{
		ID:        id,
		Namespace: fixtureNamespace,
		Service:   fixtureService,
		Origin:    pgo.OriginSchedule,
		State:     pgo.StatePending,
		CreatedBy: "schedule",
	})
	if err != nil {
		t.Fatalf("encode the collection record: %v", err)
	}

	return &oneRecordKV{key: "job." + id, value: value}
}

// entry is the record as a watch and a read both deliver it.
func (k *oneRecordKV) entry() natskv.Entry {
	return natskv.Entry{Key: k.key, Value: k.value, Revision: 1, Generation: fakeGeneration}
}

func (k *oneRecordKV) Get(_ context.Context, key string) (natskv.Entry, error) {
	if key != k.key {
		return natskv.Entry{}, natskv.ErrKeyNotFound
	}

	return k.entry(), nil
}

func (k *oneRecordKV) Keys(_ context.Context, prefix string) ([]string, error) {
	if !strings.HasPrefix(k.key, prefix) {
		return nil, nil
	}

	return []string{k.key}, nil
}

func (k *oneRecordKV) Watch(ctx context.Context, prefix string) (<-chan natskv.Entry, error) {
	ch := make(chan natskv.Entry, 2)
	if strings.HasPrefix(k.key, prefix) {
		ch <- k.entry()
	}
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

// fakeGeneration is the one store generation every PGO serve row runs under.
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

// newFakeNATSHoldingOneRecord is the same connection with one Collection record in its job bucket.
func newFakeNATSHoldingOneRecord(t *testing.T, id string) *fakeNATS {
	t.Helper()

	f := newFakeNATS(true)
	f.stores.Jobs = newOneRecordKV(t, id)

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
	// hold, when set, keeps every attempt pending until it is closed.
	hold <-chan struct{}
}

func newPreflightStub(results ...preflightResult) *preflightStub {
	return &preflightStub{results: results}
}

func (p *preflightStub) fn() natsPreflightFunc {
	return func(ctx context.Context, opts natskv.Options, _ string, _ *slog.Logger) (natskv.Client, error) {
		if p.hold != nil {
			select {
			case <-p.hold:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
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
			gatewayOpts{enabled: false, preflight: pf})
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
			gatewayOpts{enabled: true, preflight: pf, worker: worker})

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
			gatewayOpts{enabled: true, preflight: pf, worker: newStubWorker()})

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
			gatewayOpts{enabled: true, preflight: pf, worker: newStubWorker()})

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
			gatewayOpts{enabled: true, preflight: pf, worker: newStubWorker()})

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
			gatewayOpts{enabled: true, preflight: pf, worker: newStubWorker()})

		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "NATSConnected(true)", func() bool { return len(gw.rec.connectedCalls()) > 0 })
		if got := gw.rec.connectedCalls(); len(got) != 1 || !got[0] {
			t.Fatalf("NATSConnected calls = %v, want exactly [true]", got)
		}
	})

	t.Run("the drain ends a request held open by a wait", func(t *testing.T) {
		const delay = 3 * time.Second
		collection := "/v1/collections/" + waitCollectionID
		worker := newStubWorker()
		pf := newPreflightStub(preflightResult{client: newFakeNATSHoldingOneRecord(t, waitCollectionID)})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{enabled: true, preflight: pf, worker: worker, drainDelay: delay})
		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "the collection route answering", func() bool {
			code, _, err := get(gw.apiAddr, collection)

			return err == nil && code == http.StatusOK
		})

		answered := make(chan int, 1)
		go func() {
			code, _, _ := get(gw.apiAddr, collection+"?wait=60s")
			answered <- code
		}()
		// The record never moves, so nothing but the drain or the minute the
		// wait asked for can end this request.
		waitFor(t, waitTimeout, "the collection route answering beside the held request", func() bool {
			code, _, err := get(gw.apiAddr, collection)

			return err == nil && code == http.StatusOK
		})
		select {
		case code := <-answered:
			t.Fatalf("the wait answered %d before the drain began", code)
		default:
		}

		gw.stopOnce()
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/readyz", http.StatusServiceUnavailable)

		// The signal closes with readiness and before the endpoint-removal
		// window, so the answer arrives inside the window rather than at the
		// minute the client asked for.
		select {
		case code := <-answered:
			if code != http.StatusOK {
				t.Errorf("the drained wait answered %d, want 200 with the record it last read", code)
			}
		case <-time.After(delay):
			t.Fatalf("a request held open by a wait outlived the %s drain delay", delay)
		}
		worker.releaseDrain()
		if code := gw.exitCode(t, delay+waitTimeout); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	})

	t.Run("shutdown stops claiming and drains the two waits separately", func(t *testing.T) {
		worker := newStubWorker()
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{enabled: true, preflight: pf, worker: worker})
		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "the worker starting", func() bool { return worker.runCount() == 1 })

		// One interactive profile is held open, so the API drain cannot finish
		// while the worker drain is observed.
		go func() { _, _, _ = getCtx(context.Background(), gw.apiAddr, heapPath) }()
		waitFor(t, waitTimeout, "the profile request reaching the upstream", func() bool { return gw.up.calls.Load() == 1 })

		gw.stopOnce()
		waitFor(t, waitTimeout, "the worker drain starting", func() bool { return closed(worker.entered) })
		waitFor(t, waitTimeout, "the worker stopping its claiming", func() bool { return closed(worker.runEnded) })
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
		rec := gw.record(t, "drain complete")
		if rec["api"] != "completed" || rec["pgo"] != "drained" {
			t.Fatalf("drain complete record = %v, want api=completed pgo=drained", rec)
		}
	})

	t.Run("a failed listener skips the endpoint window and drains the same", func(t *testing.T) {
		worker := newStubWorker()
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		fail := make(chan struct{})
		// The drain delay is six times the wait for the exit below: a
		// listener that has failed receives nothing the endpoint window
		// protects, so the fatal path does not spend the grace period on it.
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{enabled: true, preflight: pf, worker: worker, failAPI: fail, drainDelay: 30 * time.Second})
		gw.waitReady(t, waitTimeout)
		waitFor(t, waitTimeout, "the worker starting", func() bool { return worker.runCount() == 1 })

		close(fail)
		// The Collection drain runs here as it does on any other exit:
		// it is bounded by the lease each owner last renewed,
		// and what ends the process makes no difference to that bound.
		waitFor(t, waitTimeout, "the worker drain starting", func() bool { return closed(worker.entered) })
		worker.releaseDrain()
		if code := gw.exitCode(t, waitTimeout); code != 1 {
			t.Fatalf("exit code = %d, want 1: a failed listener is a crash", code)
		}
		rec := gw.record(t, "drain complete")
		if rec["pgo"] != "drained" {
			t.Fatalf("drain complete record = %v, want pgo=drained", rec)
		}
	})
}

// TestWaitSynced covers the one wait whose progress the lifecycle loop does
// not log itself: a slow informer sync says so from waitSynced, at the
// injected cadence, and a fast one stays silent.
func TestWaitSynced(t *testing.T) {
	const reportEvery = 30 * time.Millisecond
	const progressMsg = "still waiting for informer caches to sync"

	parseRecords := func(t *testing.T, buf *syncBuffer) []map[string]any {
		t.Helper()

		var out []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line %q is not JSON: %v", line, err)
			}
			out = append(out, rec)
		}

		return out
	}

	t.Run("a slow sync reports progress until it lands", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var buf syncBuffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		var synced atomic.Bool
		syncedCh := make(chan struct{}, 1)
		go waitSynced(ctx, synced.Load, syncedCh, reportEvery, logger)

		// Three records prove the reporting repeats at the cadence rather
		// than firing once; a one-shot regression would stop after the first.
		waitFor(t, waitTimeout, "three progress records", func() bool {
			return strings.Count(buf.String(), progressMsg) >= 3
		})
		synced.Store(true)
		select {
		case <-syncedCh:
		case <-time.After(waitTimeout):
			t.Fatal("waitSynced did not report the sync after HasSynced turned true")
		}
		records := parseRecords(t, &buf)
		if len(records) < 3 {
			t.Fatalf("got %d progress records, want at least 3", len(records))
		}
		var prev time.Duration
		for _, rec := range records {
			if rec["msg"] != progressMsg {
				t.Fatalf("unexpected record %v, want only %q", rec, progressMsg)
			}
			if rec["level"] != "WARN" {
				t.Fatalf("progress record level = %v, want WARN", rec["level"])
			}
			raw, ok := rec["elapsed"].(string)
			if !ok || raw == "" {
				t.Fatalf("progress record elapsed = %v, want a duration string", rec["elapsed"])
			}
			// elapsed is rounded to whole seconds, so at this cadence
			// successive records may repeat a value; going backwards would
			// mean the wait lost track of its start time.
			elapsed, err := time.ParseDuration(raw)
			if err != nil {
				t.Fatalf("progress record elapsed %q is not a duration: %v", raw, err)
			}
			if elapsed < prev {
				t.Fatalf("progress record elapsed %v went backwards from %v", elapsed, prev)
			}
			prev = elapsed
		}
	})

	t.Run("cancellation while unsynced exits without a sync report", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var buf syncBuffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		syncedCh := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			waitSynced(ctx, func() bool { return false }, syncedCh, reportEvery, logger)
		}()

		// A first progress record pins the cancel to mid-wait, after the
		// loop is running, rather than racing the goroutine's startup.
		waitFor(t, waitTimeout, "a progress record", func() bool {
			return strings.Contains(buf.String(), progressMsg)
		})
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Fatal("waitSynced did not return after its context was cancelled")
		}
		select {
		case <-syncedCh:
			t.Fatal("waitSynced sent on syncedCh after cancellation while unsynced")
		default:
		}
	})

	t.Run("a sync that lands before the first report logs nothing", func(t *testing.T) {
		var buf syncBuffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		syncedCh := make(chan struct{}, 1)
		waitSynced(context.Background(), func() bool { return true }, syncedCh, reportEvery, logger)

		select {
		case <-syncedCh:
		default:
			t.Fatal("waitSynced returned without sending on syncedCh")
		}
		if got := strings.TrimSpace(buf.String()); got != "" {
			t.Fatalf("fast sync logged %q, want nothing", got)
		}
	})
}

// testIssuer is an OpenID Connect issuer on an httptest TLS listener: the
// discovery document, a key set that a subtest can rotate, and a gate that
// holds discovery until the subtest releases it.
type testIssuer struct {
	srv *httptest.Server
	mu  sync.Mutex
	// discoveryStatus is what discovery answers; anything but 200 fails it.
	discoveryStatus int
	// hold, when set, keeps every discovery request pending until closed.
	hold  chan struct{}
	keys  map[string]*rsa.PrivateKey // by kid, published in the key set
	order []string                   // kids in publication order
}

const (
	testAudience  = "profgate"
	testSubject   = "alice"
	wellKnownPath = "/.well-known/openid-configuration"
)

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	is := &testIssuer{discoveryStatus: http.StatusOK, keys: map[string]*rsa.PrivateKey{}}
	is.rotate(t, "k1")
	is.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		hold, status := is.hold, is.discoveryStatus
		is.mu.Unlock()
		switch r.URL.Path {
		case wellKnownPath:
			if hold != nil {
				<-hold
			}
			if status != http.StatusOK {
				http.Error(w, "down", status)

				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 is.srv.URL,
				"jwks_uri":               is.srv.URL + "/keys",
				"authorization_endpoint": is.srv.URL + "/auth",
				"token_endpoint":         is.srv.URL + "/token",
			})
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(is.keySet())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(is.srv.Close)

	return is
}

// rotate publishes a fresh key under kid in place of every earlier one.
func (is *testIssuer) rotate(t *testing.T, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.keys = map[string]*rsa.PrivateKey{kid: key}
	is.order = []string{kid}
}

func (is *testIssuer) keySet() jose.JSONWebKeySet {
	is.mu.Lock()
	defer is.mu.Unlock()
	var set jose.JSONWebKeySet
	for _, kid := range is.order {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &is.keys[kid].PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
	}

	return set
}

// caFile writes the issuer's certificate where auth.oidc.caFile can name it.
func (is *testIssuer) caFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "issuer-ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: is.srv.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// bearer mints a valid ID token for testSubject under kid.
func (is *testIssuer) bearer(t *testing.T, kid string) string {
	t.Helper()
	is.mu.Lock()
	key := is.keys[kid]
	is.mu.Unlock()
	if key == nil {
		t.Fatalf("no key under kid %q", kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: &jose.JSONWebKey{Key: key, KeyID: kid}}, nil)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Now()
	payload, err := json.Marshal(map[string]any{
		"iss": is.srv.URL,
		"sub": testSubject,
		"aud": testAudience,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("CompactSerialize: %v", err)
	}

	return "Bearer " + token
}

// oidcBlock is the auth block for oidc mode against is, mapping testSubject
// to the developer realm; browser adds the relying-party block.
func oidcBlock(t *testing.T, is *testIssuer, browser bool) string {
	t.Helper()
	block := fmt.Sprintf(`auth:
  mode: oidc
  oidc:
    issuer: %q
    audience: %s
    caFile: %q
    discoveryTimeout: 3s
    jwksRefreshMin: 1s
    mapping:
      users:
        - name: %s
          realm: developer
`, is.srv.URL, testAudience, is.caFile(t), testSubject)
	if browser {
		block += fmt.Sprintf(`    browser:
      clientID: %s
      redirectURL: "https://profgate.example/auth/callback"
      scopes: [openid]
      cookieKeyFile: %q
`, testAudience, cookieKeyFile(t))
	}

	return block
}

// cookieKeyFile writes one 32-byte key, base64 on one line.
func cookieKeyFile(t *testing.T) string {
	t.Helper()
	var key [32]byte
	if _, err := cryptorand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cookie.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// basicHash is one bcrypt hash of basicPassword at the lowest accepted cost,
// generated once because bcrypt is slow by design.
var basicHash = sync.OnceValue(func() string {
	hash, err := bcrypt.GenerateFromPassword([]byte(basicPassword), 10)
	if err != nil {
		panic(err)
	}

	return string(hash)
})

const basicPassword = "correct horse"

// basicBlock is the auth block for basic mode with one inline user, alice,
// in the developer realm.
func basicBlock(allowPlaintext bool) string {
	return fmt.Sprintf(`auth:
  mode: basic
  basic:
    allowPlaintext: %t
    users:
      - name: alice
        passwordHash: %q
        realm: developer
`, allowPlaintext, basicHash())
}

// usersFileYAML is a users file naming the given users, each with basicHash.
func usersFileYAML(names ...string) string {
	body := "users:\n"
	for _, name := range names {
		body += fmt.Sprintf("  - name: %s\n    passwordHash: %q\n    realm: developer\n", name, basicHash())
	}

	return body
}

func basicCredential(name string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(name+":"+basicPassword))
}

// reply is what one request came back with.
type reply struct {
	status int
	header http.Header
	body   string
}

// request runs one GET against addr, over TLS when pool is set, with header
// and without following redirects, so a 302 is observed as one.
func request(t *testing.T, addr, path string, pool *x509.CertPool, header http.Header) reply {
	t.Helper()
	scheme := "http"
	transport := &http.Transport{DisableKeepAlives: true}
	if pool != nil {
		scheme = "https"
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

// recordIndex returns the position of the first record whose msg starts with
// prefix, or -1.
func recordIndex(records []map[string]any, prefix string) int {
	for i, rec := range records {
		if strings.HasPrefix(fmt.Sprint(rec["msg"]), prefix) {
			return i
		}
	}

	return -1
}

// assertNoDisabledWarning fails when the disabled-mode warning was logged:
// a gateway that authenticates must not claim it does not.
func assertNoDisabledWarning(t *testing.T, gw *gateway) {
	t.Helper()
	if recordIndex(gw.records(t), "authentication disabled") >= 0 {
		t.Fatalf("the disabled-mode warning was logged under an authenticating mode:\n%s", gw.stdout.String())
	}
}

func TestServeAuth(t *testing.T) {
	t.Run("basic plaintext warns", func(t *testing.T) {
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{authBlock: basicBlock(true)})
		gw.waitReady(t, waitTimeout)
		rec := gw.record(t, "basic authentication over plaintext HTTP; passwords cross the network in the clear")
		if rec["level"] != "WARN" {
			t.Fatalf("plaintext record level = %v, want WARN", rec["level"])
		}
		assertNoDisabledWarning(t, gw)
	})

	t.Run("basic serves", func(t *testing.T) {
		dir := t.TempDir()
		pool := writeSelfSigned(t, dir)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{tlsDir: dir, authBlock: basicBlock(false)})
		gw.waitReady(t, waitTimeout)

		anon := request(t, gw.apiAddr, targetsPath, pool, nil)
		if anon.status != http.StatusUnauthorized || anon.header.Get("WWW-Authenticate") != `Basic realm="profgate"` {
			t.Fatalf("targets without a credential = %d %q, want 401 with the Basic challenge", anon.status, anon.header.Get("WWW-Authenticate"))
		}
		if !strings.Contains(anon.body, `"unauthenticated"`) {
			t.Fatalf("401 body = %q, want code unauthenticated", anon.body)
		}
		got := request(t, gw.apiAddr, targetsPath, pool, http.Header{"Authorization": {basicCredential("alice")}})
		if got.status != http.StatusOK {
			t.Fatalf("targets with the credential = %d %q, want 200", got.status, got.body)
		}
		if recordIndex(gw.records(t), "basic authentication over plaintext") >= 0 {
			t.Fatal("the plaintext warning was logged over TLS")
		}
		assertNoDisabledWarning(t, gw)
	})

	t.Run("oidc discovering", func(t *testing.T) {
		is := newTestIssuer(t)
		is.hold = make(chan struct{})
		cs := fake.NewClientset(fixtureObjects()...)
		gw := startGatewayWith(t, cs, defaultLimits(), gatewayOpts{authBlock: oidcBlock(t, is, false)})

		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/healthz", http.StatusOK)
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz while discovering = %d, want 503", code)
		}
		code, body := mustGet(t, gw.apiAddr, targetsPath)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("targets while discovering = %d %q, want 503 not_ready", code, body)
		}
		if actions := cs.Actions(); len(actions) != 0 {
			t.Fatalf("the Kubernetes preflight ran before discovery: %d actions", len(actions))
		}
		close(is.hold)
		gw.waitReady(t, waitTimeout)

		records := gw.records(t)
		discovered := recordIndex(records, "issuer discovered")
		passed := recordIndex(records, "preflight passed")
		if discovered < 0 || passed < 0 || discovered > passed {
			t.Fatalf("issuer discovered at %d, preflight passed at %d; want discovery first:\n%s", discovered, passed, gw.stdout.String())
		}
		assertNoDisabledWarning(t, gw)
	})

	t.Run("oidc discovery timeout", func(t *testing.T) {
		is := newTestIssuer(t)
		is.discoveryStatus = http.StatusInternalServerError
		cs := fake.NewClientset(fixtureObjects()...)
		gw := startGatewayWith(t, cs, defaultLimits(), gatewayOpts{authBlock: oidcBlock(t, is, false)})

		// Discovery spends its whole timeout before the gateway begins to exit.
		// That cost and the exit's own budget are waited for separately,
		// rather than sharing one number the retries have already spent most of.
		waitFor(t, 3*time.Second+waitTimeout, "the issuer retries running out", func() bool {
			return recordIndex(gw.records(t), "issuer discovery failed") >= 0
		})
		if code := gw.exitCode(t, waitTimeout); code != 1 {
			t.Fatalf("exit code = %d, want 1: a gateway that cannot reach its issuer cannot authenticate anyone", code)
		}
		gw.record(t, "issuer discovery failed")
		if actions := cs.Actions(); len(actions) != 0 {
			t.Fatalf("the Kubernetes preflight ran without discovery: %d actions", len(actions))
		}
		if recordIndex(gw.records(t), "issuer discovery attempt") < 0 {
			t.Fatalf("no retry was logged before giving up:\n%s", gw.stdout.String())
		}
	})

	t.Run("oidc bearer", func(t *testing.T) {
		is := newTestIssuer(t)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{authBlock: oidcBlock(t, is, false)})
		gw.waitReady(t, waitTimeout)

		anon := request(t, gw.apiAddr, targetsPath, nil, nil)
		if anon.status != http.StatusUnauthorized || anon.header.Get("WWW-Authenticate") != `Bearer realm="profgate"` {
			t.Fatalf("targets without a credential = %d %q, want 401 with the Bearer challenge", anon.status, anon.header.Get("WWW-Authenticate"))
		}
		got := request(t, gw.apiAddr, targetsPath, nil, http.Header{"Authorization": {is.bearer(t, "k1")}})
		if got.status != http.StatusOK {
			t.Fatalf("targets with a bearer token = %d %q, want 200", got.status, got.body)
		}

		// The issuer rotates: a token under the new kid verifies once the
		// on-demand refresh has fetched the new set, without a restart.
		is.rotate(t, "k2")
		rotated := is.bearer(t, "k2")
		waitFor(t, 3*time.Second, "a token under the rotated key verifying", func() bool {
			return request(t, gw.apiAddr, targetsPath, nil, http.Header{"Authorization": {rotated}}).status == http.StatusOK
		})
	})

	t.Run("oidc readiness order", func(t *testing.T) {
		is := newTestIssuer(t)
		is.hold = make(chan struct{})
		natsHold := make(chan struct{})
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		pf.hold = natsHold
		cs := fake.NewClientset(fixtureObjects()...)
		gw := startGatewayWith(t, cs, defaultLimits(),
			gatewayOpts{enabled: true, preflight: pf, worker: newStubWorker(), authBlock: oidcBlock(t, is, false)})

		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/healthz", http.StatusOK)
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz before discovery = %d, want 503", code)
		}
		if actions := cs.Actions(); len(actions) != 0 {
			t.Fatalf("the Kubernetes preflight ran before discovery: %d actions", len(actions))
		}
		close(is.hold)
		waitFor(t, waitTimeout, "the informers syncing", func() bool {
			return recordIndex(gw.records(t), "discovery synced") >= 0
		})
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz with discovery, preflight, and sync done but NATS pending = %d, want 503", code)
		}
		close(natsHold)
		gw.waitReady(t, waitTimeout)
	})

	t.Run("/auth/ waits for the preflight", func(t *testing.T) {
		is := newTestIssuer(t)
		dir := t.TempDir()
		pool := writeSelfSigned(t, dir)
		cs := fake.NewClientset(fixtureObjects()...)
		release := make(chan struct{})
		cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			<-release

			return false, nil, nil
		})
		pf := newPreflightStub(preflightResult{client: newFakeNATS(true)})
		gw := startGatewayWith(t, cs, defaultLimits(),
			gatewayOpts{enabled: true, preflight: pf, worker: newStubWorker(), tlsDir: dir, authBlock: oidcBlock(t, is, true)})

		waitFor(t, waitTimeout, "issuer discovery", func() bool {
			return recordIndex(gw.records(t), "issuer discovered") >= 0
		})
		if code, _ := mustGet(t, gw.opsAddr, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz during the preflight = %d, want 503", code)
		}
		for _, path := range []string{"/auth/login", targetsPath} {
			got := request(t, gw.apiAddr, path, pool, nil)
			if got.status != http.StatusServiceUnavailable || !strings.Contains(got.body, `"not_ready"`) {
				t.Fatalf("GET %s during the preflight = %d %q, want 503 not_ready", path, got.status, got.body)
			}
		}
		close(release)
		gw.waitReady(t, waitTimeout)
		if got := request(t, gw.apiAddr, "/auth/login", pool, nil); got.status != http.StatusFound {
			t.Fatalf("GET /auth/login once ready = %d %q, want 302 to the issuer", got.status, got.body)
		}
		if got := request(t, gw.apiAddr, targetsPath, pool, nil); got.status != http.StatusUnauthorized {
			t.Fatalf("GET targets once ready = %d %q, want 401", got.status, got.body)
		}
	})

	t.Run("/auth/ through the drain delay", func(t *testing.T) {
		const delay = 3 * time.Second
		is := newTestIssuer(t)
		dir := t.TempDir()
		pool := writeSelfSigned(t, dir)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{tlsDir: dir, authBlock: oidcBlock(t, is, true), drainDelay: delay})
		gw.waitReady(t, waitTimeout)

		gw.stopOnce()
		gw.waitStatus(t, waitTimeout, gw.opsAddr, "/readyz", http.StatusServiceUnavailable)
		// The endpoint-removal window: readiness is already 503, and the
		// /auth/ routes keep answering the way /v1 does, until the listener
		// closes after the delay.
		if got := request(t, gw.apiAddr, "/auth/login", pool, nil); got.status != http.StatusFound {
			t.Fatalf("GET /auth/login during the drain delay = %d %q, want 302", got.status, got.body)
		}
		if code := gw.exitCode(t, 2*delay); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if !refuses(gw.apiAddr) {
			t.Fatal("the API listener still accepts connections after exit")
		}
	})

	t.Run("users file polled", func(t *testing.T) {
		usersPath := filepath.Join(t.TempDir(), "users.yaml")
		if err := os.WriteFile(usersPath, []byte(usersFileYAML("alice")), 0o600); err != nil {
			t.Fatal(err)
		}
		block := fmt.Sprintf("auth:\n  mode: basic\n  basic:\n    allowPlaintext: true\n    usersFile: %q\n", usersPath)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{authBlock: block, authPoll: 100 * time.Millisecond})
		gw.waitReady(t, waitTimeout)

		if got := request(t, gw.apiAddr, targetsPath, nil, http.Header{"Authorization": {basicCredential("bob")}}); got.status != http.StatusUnauthorized {
			t.Fatalf("targets as bob before the rewrite = %d, want 401", got.status)
		}
		if err := os.WriteFile(usersPath, []byte(usersFileYAML("alice", "bob")), 0o600); err != nil {
			t.Fatal(err)
		}
		waitFor(t, waitTimeout, "bob being admitted", func() bool {
			return request(t, gw.apiAddr, targetsPath, nil, http.Header{"Authorization": {basicCredential("bob")}}).status == http.StatusOK
		})
		select {
		case <-gw.exited:
			t.Fatal("serve exited; the users file must be re-read by the running process")
		default:
		}
		if n := strings.Count(gw.stdout.String(), `"msg":"listening"`); n != 1 {
			t.Fatalf("listening records = %d, want 1: no restart", n)
		}
		gw.record(t, "file reloaded")
	})

	t.Run("cookie key fails startup", func(t *testing.T) {
		is := newTestIssuer(t)
		dir := t.TempDir()
		writeSelfSigned(t, dir)
		block := oidcBlock(t, is, true)
		block = strings.Replace(block, "cookieKeyFile: ", "cookieKeyFile: \"/nonexistent/cookie.key\" # ", 1)
		gw := startGatewayWith(t, fake.NewClientset(fixtureObjects()...), defaultLimits(),
			gatewayOpts{tlsDir: dir, authBlock: block})

		if code := gw.exitCode(t, waitTimeout); code != 2 {
			t.Fatalf("exit code = %d, want 2: an unreadable cookie key file is a configuration error", code)
		}
		if !strings.Contains(gw.stderr.String(), "auth.oidc.browser.cookieKeyFile") {
			t.Fatalf("stderr = %q, want it to name auth.oidc.browser.cookieKeyFile", gw.stderr.String())
		}
	})
}
