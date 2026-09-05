package natskv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fixtureTimeout bounds every wait a fixture performs.
const fixtureTimeout = 10 * time.Second

// fixtureReconnectWait keeps reconnect-after-restart tests fast.
const fixtureReconnectWait = 50 * time.Millisecond

const (
	fixtureAdminUser  = "admin"
	fixtureClientUser = "client"
	fixturePassword   = "s3cret"
)

// fixture is one in-process JetStream server with the three buckets
// provisioned, a connected seam client, and a raw admin connection.
type fixture struct {
	t     *testing.T
	opts  *server.Options
	srv   *server.Server
	admin *nats.Conn
	js    jetstream.JetStream
	c     *client
}

// startFixture is startServerFixture plus a connected seam client.
func startFixture(t *testing.T, mutate ...func(*server.Options)) *fixture {
	t.Helper()
	f := startServerFixture(t, mutate...)
	f.c = f.connectClient()
	return f
}

// startServerFixture starts a nats-server on a random port with JetStream on
// t.TempDir(), provisions the three buckets per the contract, and connects
// the raw admin connection; no seam client is connected, so a test can run
// Preflight itself or reshape the buckets first.
// mutate hooks adjust the server options before the first start.
// A permission test sets server.Options.Users through withUsers: with users
// declared, every connection carries its credentials in the URL, and the
// seam runs as the restricted fixtureClientUser while the fixture's
// provisioning runs as the unrestricted fixtureAdminUser.
func startServerFixture(t *testing.T, mutate ...func(*server.Options)) *fixture {
	t.Helper()

	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	for _, m := range mutate {
		m(opts)
	}

	f := &fixture{t: t, opts: opts}
	f.srv = runServer(t, opts)
	// Pin the resolved port so a restart comes back at the same address.
	tcp, ok := f.srv.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("server address is %T, not *net.TCPAddr", f.srv.Addr())
	}
	opts.Port = tcp.Port

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()

	admin, err := nats.Connect(f.url(fixtureAdminUser),
		nats.Name("natskv-test-admin"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(fixtureReconnectWait),
	)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	f.admin = admin

	f.js, err = jetstream.New(admin)
	if err != nil {
		t.Fatalf("admin jetstream: %v", err)
	}
	for _, bucket := range []string{configBucket, jobsBucket} {
		_, err = f.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  bucket,
			History: 1,
			Storage: jetstream.FileStorage,
		})
		if err != nil {
			t.Fatalf("create bucket %s: %v", bucket, err)
		}
	}
	_, err = f.js.CreateObjectStore(ctx, jetstream.ObjectStoreConfig{
		Bucket:  artifactsBucket,
		Storage: jetstream.FileStorage,
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", artifactsBucket, err)
	}

	t.Cleanup(func() {
		f.admin.Close()
		f.srv.Shutdown()
		f.srv.WaitForShutdown()
	})

	return f
}

// connectClient connects one seam client as the (possibly restricted)
// fixtureClientUser and registers its cleanup.
func (f *fixture) connectClient() *client {
	f.t.Helper()
	return f.connectClientLogged(testLogger())
}

// connectClientLogged is connectClient with a logger the test chose,
// so a test can count the records the seam writes.
func (f *fixture) connectClientLogged(log *slog.Logger) *client {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	c, err := connect(ctx, connectConfig{
		url:            f.url(fixtureClientUser),
		connectTimeout: callTimeout,
		reconnectWait:  fixtureReconnectWait,
		name:           "natskv-test",
	}, log)
	if err != nil {
		f.t.Fatalf("seam connect: %v", err)
	}
	f.t.Cleanup(c.close)
	return c
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// logCapture keeps every record a client wrote, so a test can count the records of one message.
type logCapture struct {
	mu      sync.Mutex
	records []string
}

// captureLogger returns a logger that keeps its records, beside testLogger, which discards them.
func captureLogger() (*slog.Logger, *logCapture) {
	c := &logCapture{}
	return slog.New(&captureHandler{c: c}), c
}

// count returns how many records carried the message msg.
func (c *logCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.records {
		if m == msg {
			n++
		}
	}
	return n
}

type captureHandler struct {
	c *logCapture
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	h.c.records = append(h.c.records, r.Message)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// watcherTap keeps the live watcher of each prefix,
// so a test can stop one and cut its subscription while the connection stays up.
type watcherTap struct {
	mu       sync.Mutex
	watchers map[string]jetstream.KeyWatcher
}

func newWatcherTap() *watcherTap {
	return &watcherTap{watchers: map[string]jetstream.KeyWatcher{}}
}

// record is the client's testWatchOpened hook.
func (w *watcherTap) record(prefix string, kw jetstream.KeyWatcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watchers[prefix] = kw
}

// cut stops the watcher currently open on prefix, which closes its subscription.
func (w *watcherTap) cut(t *testing.T, prefix string) {
	t.Helper()
	w.mu.Lock()
	kw := w.watchers[prefix]
	w.mu.Unlock()
	if kw == nil {
		t.Fatalf("no watcher was opened on %q", prefix)
	}
	if err := kw.Stop(); err != nil {
		t.Fatalf("stop the watcher on %q: %v", prefix, err)
	}
}

// stop stops whatever watcher is open on prefix now, and reports whether there was one to stop.
// It is cut without the test's failure:
// a race that cuts two watchers at once runs off the test goroutine,
// and a watcher whose subscription is already gone answers an error that decides nothing.
func (w *watcherTap) stop(prefix string) bool {
	w.mu.Lock()
	kw := w.watchers[prefix]
	w.mu.Unlock()
	if kw == nil {
		return false
	}
	//nolint:errcheck // stopping a watcher whose subscription is already gone is what a racing cut finds
	_ = kw.Stop()

	return true
}

// withUsers runs the server with an unrestricted admin user and a client
// user carrying the given permissions.
func withUsers(clientPerms *server.Permissions) func(*server.Options) {
	return func(o *server.Options) {
		o.Users = []*server.User{
			{Username: fixtureAdminUser, Password: fixturePassword},
			{Username: fixtureClientUser, Password: fixturePassword, Permissions: clientPerms},
		}
	}
}

// fragmentPermissions is the publish and subscribe allowlist of the spec's
// NATS account fragment, one subject per entry.
func fragmentPermissions() *server.Permissions {
	streams := []string{"KV_PROFGATE_CONFIG", "KV_PROFGATE_JOBS", "OBJ_PROFGATE_ARTIFACTS"}
	publish := []string{"$JS.API.INFO"}
	for _, s := range streams {
		publish = append(publish,
			"$JS.API.STREAM.INFO."+s,
			"$JS.API.CONSUMER.CREATE."+s+".>",
			"$JS.API.CONSUMER.DELETE."+s+".>",
			"$JS.API.CONSUMER.INFO."+s+".>",
			"$JS.API.CONSUMER.MSG.NEXT."+s+".>",
			"$JS.API.DIRECT.GET."+s+".>",
			"$JS.API.STREAM.MSG.GET."+s,
		)
	}
	publish = append(publish,
		"$JS.API.STREAM.PURGE.OBJ_PROFGATE_ARTIFACTS",
		"$KV.PROFGATE_CONFIG.>", "$KV.PROFGATE_JOBS.>", "$O.PROFGATE_ARTIFACTS.>",
	)
	subscribe := []string{"_INBOX.>", "$KV.PROFGATE_CONFIG.>", "$KV.PROFGATE_JOBS.>", "$O.PROFGATE_ARTIFACTS.>"}
	return &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: publish},
		Subscribe: &server.SubjectPermission{Allow: subscribe},
	}
}

// fragmentWithout returns the fragment's permissions minus one exact
// subject taken out of the named list ("publish" or "subscribe").
func fragmentWithout(t *testing.T, list, subject string) *server.Permissions {
	t.Helper()
	perms := fragmentPermissions()
	target := perms.Publish
	if list == "subscribe" {
		target = perms.Subscribe
	}
	kept := make([]string, 0, len(target.Allow))
	for _, s := range target.Allow {
		if s != subject {
			kept = append(kept, s)
		}
	}
	if len(kept) != len(target.Allow)-1 {
		t.Fatalf("subject %q is not in the %s list", subject, list)
	}
	target.Allow = kept
	return perms
}

// url builds the client URL, carrying credentials when the server runs users.
func (f *fixture) url(user string) string {
	if len(f.opts.Users) == 0 {
		return fmt.Sprintf("nats://127.0.0.1:%d", f.opts.Port)
	}
	return fmt.Sprintf("nats://%s:%s@127.0.0.1:%d", user, fixturePassword, f.opts.Port)
}

// stopServer shuts the server down and waits for it to be gone;
// the store directory and port stay reserved for restartServer.
func (f *fixture) stopServer() {
	f.t.Helper()
	f.srv.Shutdown()
	f.srv.WaitForShutdown()
}

// restartServer brings the server back in place: same store directory, same port.
func (f *fixture) restartServer() {
	f.t.Helper()
	f.srv = runServer(f.t, f.opts)
}

// view returns the stores bound to the current generation.
func (f *fixture) view() Stores {
	f.t.Helper()
	stores, err := f.c.View(f.c.Generation())
	if err != nil {
		f.t.Fatalf("view: %v", err)
	}
	return stores
}

// artifactConsumers counts the consumers the artifact stream holds,
// which is how many object reads have not released theirs.
func (f *fixture) artifactConsumers(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	stream, err := f.js.Stream(ctx, "OBJ_"+artifactsBucket)
	if err != nil {
		t.Fatalf("artifact stream: %v", err)
	}
	n := 0
	names := stream.ConsumerNames(ctx)
	for range names.Name() {
		n++
	}
	if err := names.Err(); err != nil {
		t.Fatalf("consumer names: %v", err)
	}
	return n
}

// putBytes stores n bytes of a known pattern under name through the admin connection
// and returns them.
func (f *fixture) putBytes(t *testing.T, name string, n int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	obs, err := f.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		t.Fatalf("admin object store: %v", err)
	}
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	if _, err := obs.Put(ctx, jetstream.ObjectMeta{Name: name}, bytes.NewReader(data)); err != nil {
		t.Fatalf("admin put %s: %v", name, err)
	}
	return data
}

// chunkSize is nats.go's default object chunk, which every put here uses.
const chunkSize = 128 << 10

// rewriteDigest republishes the object's metadata with the digest of other bytes,
// so the chunks stored under it no longer sum to what the metadata claims.
func (f *fixture) rewriteDigest(t *testing.T, name string, other []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	stream, err := f.js.Stream(ctx, "OBJ_"+artifactsBucket)
	if err != nil {
		t.Fatalf("artifact stream: %v", err)
	}
	subject := "$O." + artifactsBucket + ".M." + base64.URLEncoding.EncodeToString([]byte(name))
	last, err := stream.GetLastMsgForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("last meta message of %s: %v", name, err)
	}
	var info jetstream.ObjectInfo
	if err := json.Unmarshal(last.Data, &info); err != nil {
		t.Fatalf("meta message of %s: %v", name, err)
	}
	sum := sha256.Sum256(other)
	info.Digest = "SHA-256=" + base64.URLEncoding.EncodeToString(sum[:])
	body, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	msg := nats.NewMsg(last.Subject)
	msg.Header.Set(jetstream.MsgRollup, jetstream.MsgRollupSubject)
	msg.Data = body
	if _, err := f.js.PublishMsg(ctx, msg); err != nil {
		t.Fatalf("republish meta of %s: %v", name, err)
	}
}

// chunkNUID returns the identifier the chunk subject of name carries.
func (f *fixture) chunkNUID(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	obs, err := f.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		t.Fatalf("admin object store: %v", err)
	}
	info, err := obs.GetInfo(ctx, name)
	if err != nil {
		t.Fatalf("admin get info %s: %v", name, err)
	}
	return info.NUID
}

// stall is a read held between two chunks of an object.
type stall struct {
	// written is closed once the chunk before the held one has been written into the pipe,
	// which is once the caller has drained it.
	written chan struct{}
	release chan struct{}
	once    sync.Once
}

// free releases the held fetch; it is safe to call more than once.
func (s *stall) free() {
	s.once.Do(func() { close(s.release) })
}

// stalledAt stores a four-chunk object,
// and arranges for the seam's read of it to block before the fetch of chunk chunk, counting from one,
// so a reader that has drained chunk chunk-1 has a Read pending on a pump that has not fetched.
// The test releases the fetch with free; the cleanup releases it for a test that never does.
func stalledAt(t *testing.T, f *fixture, chunk int) (string, []byte, *stall) {
	t.Helper()
	const name = "stalled.pprof"
	data := f.putBytes(t, name, 512<<10)
	s := &stall{written: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(s.free)
	f.c.testBeforeFetch = func(_ string, n int) {
		if n == chunk {
			<-s.release
		}
	}
	f.c.testChunkWritten = func(_ string, n int) {
		if n == chunk-1 {
			close(s.written)
		}
	}
	return name, data, s
}

func runServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	srv, err := server.NewServer(opts.Clone())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(fixtureTimeout) {
		t.Fatalf("server not ready within %s", fixtureTimeout)
	}
	return srv
}

// waitFor polls pred until it is true or the deadline passes.
func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(fixtureTimeout)
	for {
		if pred() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, fixtureTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// nextEntry reads one entry from a watch channel or fails the test.
func nextEntry(t *testing.T, ch <-chan Entry) Entry {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatalf("watch channel closed while an entry was expected")
		}
		return e
	case <-time.After(fixtureTimeout):
		t.Fatalf("no watch entry within %s", fixtureTimeout)
		return Entry{}
	}
}

// drainToMarker reads a watch channel through its replay marker.
func drainToMarker(t *testing.T, ch <-chan Entry) {
	t.Helper()
	e := nextEntry(t, ch)
	for !e.Synced {
		e = nextEntry(t, ch)
	}
}
