package natskv

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	c, err := connect(ctx, connectConfig{
		url:            f.url(fixtureClientUser),
		connectTimeout: callTimeout,
		reconnectWait:  fixtureReconnectWait,
		name:           "natskv-test",
	}, testLogger())
	if err != nil {
		f.t.Fatalf("seam connect: %v", err)
	}
	f.t.Cleanup(c.close)
	return c
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
