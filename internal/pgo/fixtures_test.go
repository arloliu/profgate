package pgo

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/proxy"
)

// fixtureTimeout bounds every wait a fixture performs.
// It is a failure deadline and never a sleep, so it is set well clear of the
// slowest path any test drives: a NATS reconnect after the in-process server
// is restarted, on a machine running the rest of the suite at the same time.
const fixtureTimeout = 30 * time.Second

// The three buckets, named as internal/natskv opens them.
const (
	configBucket    = "PROFGATE_CONFIG"
	jobsBucket      = "PROFGATE_JOBS"
	artifactsBucket = "PROFGATE_ARTIFACTS"
)

// slotBase is the slot the spec pins a key and a hash input to:
// 2026-08-24T00:00:00Z, Unix second 1787529600.
var slotBase = time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

// pgoFixture is one in-process JetStream server with the three buckets
// provisioned and an admin connection for authoritative reads and writes.
// Counting assertions go through it, never through a replica's caches.
type pgoFixture struct {
	t      *testing.T
	opts   *server.Options
	srv    *server.Server
	admin  *nats.Conn
	js     jetstream.JetStream
	jobs   jetstream.KeyValue
	config jetstream.KeyValue
}

// startPGO starts a nats-server on a random port with JetStream on
// t.TempDir() and provisions the three buckets per the contract.
func startPGO(t *testing.T) *pgoFixture {
	t.Helper()

	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	f := &pgoFixture{t: t, opts: opts}
	f.srv = runServer(t, opts)
	// Pin the resolved port so a restart comes back at the same address.
	tcp, ok := f.srv.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("server address is %T, not *net.TCPAddr", f.srv.Addr())
	}
	opts.Port = tcp.Port

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()

	admin, err := nats.Connect(f.url(), nats.Name("pgo-test-admin"), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	f.admin = admin
	if f.js, err = jetstream.New(admin); err != nil {
		t.Fatalf("admin jetstream: %v", err)
	}
	if f.config, err = f.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: configBucket, History: 1, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create bucket %s: %v", configBucket, err)
	}
	if f.jobs, err = f.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: jobsBucket, History: 1, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create bucket %s: %v", jobsBucket, err)
	}
	if _, err = f.js.CreateObjectStore(ctx, jetstream.ObjectStoreConfig{
		Bucket: artifactsBucket, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create bucket %s: %v", artifactsBucket, err)
	}

	t.Cleanup(func() {
		f.admin.Close()
		f.srv.Shutdown()
		f.srv.WaitForShutdown()
	})

	return f
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

func (f *pgoFixture) url() string { return fmt.Sprintf("nats://127.0.0.1:%d", f.opts.Port) }

// stopServer shuts the server down; the store directory and port stay
// reserved for restartServer, so a client's reconnect finds the same bucket.
func (f *pgoFixture) stopServer() {
	f.t.Helper()
	f.srv.Shutdown()
	f.srv.WaitForShutdown()
}

// restartServer brings the server back in place: same store directory, same port.
func (f *pgoFixture) restartServer() {
	f.t.Helper()
	f.srv = runServer(f.t, f.opts)
}

// keys lists the authoritative keys under prefix.
func (f *pgoFixture) keys(bucket jetstream.KeyValue, prefix string) []string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	all, err := bucket.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("list keys: %v", err)
	}
	out := make([]string, 0, len(all))
	for _, k := range all {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}

	return out
}

// jobKeys is every job.<id> key in the authoritative bucket.
func (f *pgoFixture) jobKeys() []string { return f.keys(f.jobs, jobPrefix) }

// records reads every Collection record in the authoritative bucket.
func (f *pgoFixture) records() []Record {
	f.t.Helper()
	var out []Record
	for _, key := range f.jobKeys() {
		var rec Record
		f.getJSON(f.jobs, key, &rec)
		out = append(out, rec)
	}

	return out
}

// nonterminalRecords is every record the bucket holds in a state a Collection
// can still leave.
func (f *pgoFixture) nonterminalRecords() []Record {
	f.t.Helper()
	var out []Record
	for _, rec := range f.records() {
		if !terminal(rec.State) {
			out = append(out, rec)
		}
	}

	return out
}

// liveServices is the authoritative live-Collection set: every Service with an
// active key or a record in a state a Collection can still leave.
func (f *pgoFixture) liveServices() map[serviceRef]struct{} {
	f.t.Helper()
	live := make(map[serviceRef]struct{})
	for _, key := range f.keys(f.jobs, activePrefix) {
		ns, svc, ok := splitServiceKey(activePrefix, key)
		if !ok {
			continue
		}
		live[serviceRef{Namespace: ns, Service: svc}] = struct{}{}
	}
	for _, rec := range f.nonterminalRecords() {
		live[serviceRef{Namespace: rec.Namespace, Service: rec.Service}] = struct{}{}
	}

	return live
}

// getJSON reads one key and decodes it, failing the test when it is absent.
func (f *pgoFixture) getJSON(bucket jetstream.KeyValue, key string, into any) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	e, err := bucket.Get(ctx, key)
	if err != nil {
		f.t.Fatalf("get %s: %v", key, err)
	}
	if err := json.Unmarshal(e.Value(), into); err != nil {
		f.t.Fatalf("decode %s: %v", key, err)
	}
}

// putJSON writes one key authoritatively, standing in for another replica or
// for what a creator that died left behind.
func (f *pgoFixture) putJSON(bucket jetstream.KeyValue, key string, value any) uint64 {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	b, err := json.Marshal(value)
	if err != nil {
		f.t.Fatalf("encode %s: %v", key, err)
	}
	rev, err := bucket.Put(ctx, key, b)
	if err != nil {
		f.t.Fatalf("put %s: %v", key, err)
	}

	return rev
}

// deleteKey removes one key authoritatively.
func (f *pgoFixture) deleteKey(bucket jetstream.KeyValue, key string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if err := bucket.Delete(ctx, key); err != nil {
		f.t.Fatalf("delete %s: %v", key, err)
	}
}

// seedRecord writes a Collection record straight into the bucket, standing in
// for another replica's publication or for what a creator that died left.
func (f *pgoFixture) seedRecord(ns, svc string, state State, mutate ...func(*Record)) string {
	f.t.Helper()
	id := newID()
	rec := Record{
		ID:        id,
		Namespace: ns,
		Service:   svc,
		Origin:    OriginSchedule,
		Policy:    schedulerDefaults(f.t),
		State:     state,
		ClaimBy:   slotBase.Add(time.Hour),
		CreatedBy: createdBySchedule,
		CreatedAt: slotBase,
	}
	rec.SnapshotHash = SnapshotHash(rec.Policy)
	for _, m := range mutate {
		m(&rec)
	}
	f.putJSON(f.jobs, jobKey(rec.ID), rec)

	return rec.ID
}

// seedClaimable writes a record a worker may claim, plus the active key that
// names it, and applies whatever a test needs changed.
func (f *pgoFixture) seedClaimable(ns, svc string, mutate ...func(*Record)) string {
	f.t.Helper()
	id := newID()
	rec := Record{
		ID:        id,
		Namespace: ns,
		Service:   svc,
		Origin:    OriginSchedule,
		Policy:    schedulerDefaults(f.t),
		State:     StatePending,
		ClaimBy:   slotBase.Add(time.Hour),
		CreatedBy: createdBySchedule,
		CreatedAt: slotBase,
	}
	for _, m := range mutate {
		m(&rec)
	}
	f.putJSON(f.jobs, jobKey(rec.ID), rec)
	f.putJSON(f.jobs, activeKey(ns, svc), activeValue{ID: rec.ID, CreatedAt: slotBase})

	return rec.ID
}

// seedCompleted writes what a Collection leaves behind once it has finished:
// a completed record naming its artifact, and the artifact itself.
// The record finishes at slotBase and its artifact is retained for an hour
// unless a test says otherwise.
func (f *pgoFixture) seedCompleted(r *replica, ns, svc string, mutate ...func(*Record)) Record {
	f.t.Helper()
	id := newID()
	finished := slotBase
	expires := slotBase.Add(time.Hour)
	rec := Record{
		ID:         id,
		Namespace:  ns,
		Service:    svc,
		Origin:     OriginSchedule,
		Policy:     schedulerDefaults(f.t),
		State:      StateCompleted,
		Attempt:    1,
		Artifact:   &ArtifactRef{Object: fmt.Sprintf("%s-1.pprof", id), Bytes: 7},
		CreatedBy:  createdBySchedule,
		CreatedAt:  slotBase,
		FinishedAt: &finished,
		ExpiresAt:  &expires,
	}
	for _, m := range mutate {
		m(&rec)
	}
	f.putJSON(f.jobs, jobKey(rec.ID), rec)
	if rec.Artifact != nil {
		f.putObject(r, rec.Artifact.Object)
	}

	return rec
}

// objectNames lists what the artifact bucket holds, in a stable order.
func (f *pgoFixture) objectNames(r *replica) []string {
	f.t.Helper()
	stores, err := r.client.View(r.client.Generation())
	if err != nil {
		f.t.Fatalf("view: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	objects, err := stores.Artifacts.List(ctx)
	if err != nil {
		f.t.Fatalf("list objects: %v", err)
	}
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		out = append(out, o.Name)
	}
	sort.Strings(out)

	return out
}

// objectModTime is the server's timestamp on one object, which is what an age
// threshold over the artifact bucket is measured from.
func (f *pgoFixture) objectModTime(r *replica, name string) time.Time {
	f.t.Helper()
	stores, err := r.client.View(r.client.Generation())
	if err != nil {
		f.t.Fatalf("view: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	objects, err := stores.Artifacts.List(ctx)
	if err != nil {
		f.t.Fatalf("list objects: %v", err)
	}
	for _, o := range objects {
		if o.Name == name {
			return o.ModTime.UTC()
		}
	}
	f.t.Fatalf("the bucket holds no object named %s", name)

	return time.Time{}
}

// keyCreated is the server's timestamp on one key's current revision, which is
// what an age threshold over a KV bucket is measured from.
func (f *pgoFixture) keyCreated(bucket jetstream.KeyValue, key string) time.Time {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	e, err := bucket.Get(ctx, key)
	if err != nil {
		f.t.Fatalf("get %s: %v", key, err)
	}

	return e.Created().UTC()
}

// hasKey reports whether one key is in the authoritative bucket.
func (f *pgoFixture) hasKey(bucket jetstream.KeyValue, key string) bool {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	_, err := bucket.Get(ctx, key)

	return err == nil
}

// record reads one Collection record fresh from the authoritative bucket.
func (f *pgoFixture) record(id string) Record {
	f.t.Helper()
	var rec Record
	f.getJSON(f.jobs, jobKey(id), &rec)

	return rec
}

// seedLiveCollection writes a record in state plus the active key naming it,
// which is what a Service with a live Collection looks like in the bucket.
func (f *pgoFixture) seedLiveCollection(ns, svc string, state State, mutate ...func(*Record)) string {
	f.t.Helper()
	id := f.seedRecord(ns, svc, state, mutate...)
	f.putJSON(f.jobs, activeKey(ns, svc), activeValue{ID: id, CreatedAt: slotBase})

	return id
}

// seedReceipt writes what one idempotency key created, standing in for a
// publication another replica made or for one a sweep has not reached.
func (f *pgoFixture) seedReceipt(principal, ns, svc, key string, r Receipt) string {
	f.t.Helper()
	name := ReceiptKey(principal, ns, svc, key)
	f.putJSON(f.jobs, name, r)

	return name
}

// receipt reads one receipt fresh from the authoritative bucket.
func (f *pgoFixture) receipt(key string) Receipt {
	f.t.Helper()
	var r Receipt
	f.getJSON(f.jobs, key, &r)

	return r
}

// receiptKeys is every idempotency receipt in the bucket.
func (f *pgoFixture) receiptKeys() []string { return f.keys(f.jobs, receiptPrefix) }

// failRecord flips one record to failed and releases its active key, which is
// what the worker scan commits for a Collection it gives up on.
func (f *pgoFixture) failRecord(id, reason string) {
	f.t.Helper()
	var rec Record
	f.getJSON(f.jobs, jobKey(id), &rec)
	rec.State = StateFailed
	rec.Reason = reason
	finished := slotBase
	rec.FinishedAt = &finished
	f.putJSON(f.jobs, jobKey(id), rec)
	f.releaseActiveFor(rec.Namespace, rec.Service, id)
}

// finishCollection completes whatever live Collection a Service has and
// releases its active key, which is what the owner loop's final update commits.
func (f *pgoFixture) finishCollection(ns, svc string) {
	f.t.Helper()
	for _, rec := range f.records() {
		if rec.Namespace != ns || rec.Service != svc || terminal(rec.State) {
			continue
		}
		rec.State = StateCompleted
		finished := slotBase
		rec.FinishedAt = &finished
		f.putJSON(f.jobs, jobKey(rec.ID), rec)
		f.releaseActiveFor(ns, svc, rec.ID)
	}
}

// releaseActiveFor deletes a Service's active key when it names id, the rule
// every terminal transition follows.
func (f *pgoFixture) releaseActiveFor(ns, svc, id string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	e, err := f.jobs.Get(ctx, activeKey(ns, svc))
	if err != nil {
		return
	}
	var v activeValue
	if err := json.Unmarshal(e.Value(), &v); err != nil || v.ID != id {
		return
	}
	f.deleteKey(f.jobs, activeKey(ns, svc))
}

// setOverride writes a Service's stored policy override and returns its revision.
func (f *pgoFixture) setOverride(ns, svc string, override *PolicyOverride) uint64 {
	f.t.Helper()

	return f.putJSON(f.config, overrideKey(ns, svc), StoredOverride{
		Policy:    override,
		UpdatedBy: "test",
		UpdatedAt: slotBase,
	})
}

// enabledOverride is the smallest override that schedules a Service.
func enabledOverride(mutate ...func(*PolicyOverride)) *PolicyOverride {
	enabled := true
	o := &PolicyOverride{Enabled: &enabled}
	for _, m := range mutate {
		m(o)
	}

	return o
}

// withEvery sets the schedule block's every.
func withEvery(d time.Duration) func(*PolicyOverride) {
	return func(o *PolicyOverride) {
		every := Duration(d)
		if o.Schedule == nil {
			o.Schedule = &ScheduleOverride{}
		}
		o.Schedule.Every = &every
	}
}

// withJitter sets the schedule block's jitter.
func withJitter(d time.Duration) func(*PolicyOverride) {
	return func(o *PolicyOverride) {
		jitter := Duration(d)
		if o.Schedule == nil {
			o.Schedule = &ScheduleOverride{}
		}
		o.Schedule.Jitter = &jitter
	}
}

// limitsWith is the shipped ceilings with the fields one test needs changed.
func limitsWith(mutate ...func(*config.PGOLimits)) config.PGOLimits {
	lim := testLimits()
	for _, m := range mutate {
		m(&lim)
	}

	return lim
}

// schedulerDefaults is the operator's default policy every test layers onto.
func schedulerDefaults(t *testing.T) Policy {
	t.Helper()
	p, err := DefaultPolicy(testDefaults())
	if err != nil {
		t.Fatalf("default policy: %v", err)
	}

	return p
}

// replicaOpts shapes one simulated replica.
type replicaOpts struct {
	limits config.PGOLimits
	// clock is shared when two replicas must agree on now, and separate when a
	// test interleaves them.
	clock *fakeClock
	// freezer, when set, holds the delivery of the caches it names, so a test
	// can leave one stale while the seam's own watch stays synced.
	freezer *cacheFreezer
	// wrapClient wraps the client the scheduler and the publisher see, leaving
	// the caches on the raw one.
	wrapClient func(natskv.Client) natskv.Client
}

// replica is one simulated gateway replica: its own connection, caches,
// publisher, and scheduler over the fixture's one server.
type replica struct {
	t      *testing.T
	name   string
	client natskv.Client
	// loopClient is what the scheduler and the worker see: the raw client, or
	// the wrapper a test installed to count or intercept store calls.
	loopClient natskv.Client
	caches     *Caches
	pub        *Publisher
	sched      *Scheduler
	clock      *fakeClock
	recorder   *countingRecorder
	logs       *logCapture
	limits     config.PGOLimits
	cancel     context.CancelFunc
	done       chan struct{}
}

// newReplica connects one replica and runs its caches until the test ends.
func (f *pgoFixture) newReplica(name string, o replicaOpts) *replica {
	f.t.Helper()

	if o.limits == (config.PGOLimits{}) {
		o.limits = testLimits()
	}
	if o.clock == nil {
		o.clock = newFakeClock(slotBase)
	}

	logs := newLogCapture()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	client, err := natskv.Preflight(ctx, natskv.Options{
		URL:            f.url(),
		ConnectTimeout: 5 * time.Second,
	}, name, logs.logger())
	if err != nil {
		f.t.Fatalf("preflight for %s: %v", name, err)
	}

	caches := NewCaches(logs.logger())
	if o.freezer != nil {
		caches.applyGate = o.freezer.gate
	}
	recorder := newCountingRecorder()
	pub := NewPublisher(caches, o.clock, o.limits.MaxLiveCollections, name, logs.logger())

	loopClient := client
	if o.wrapClient != nil {
		loopClient = o.wrapClient(client)
	}

	r := &replica{
		t:        f.t,
		name:     name,
		client:   client,
		caches:   caches,
		pub:      pub,
		clock:    o.clock,
		recorder: recorder,
		logs:     logs,
		limits:   o.limits,
		done:     make(chan struct{}),
	}
	r.loopClient = loopClient
	r.sched = NewScheduler(loopClient, caches, pub, schedulerDefaults(f.t), o.limits, o.clock, recorder, logs.logger())

	runCtx, runCancel := context.WithCancel(context.Background())
	r.cancel = runCancel
	go func() {
		defer close(r.done)
		if err := caches.Run(runCtx, client); err != nil {
			logs.logger().Error("caches stopped", "error", err)
		}
	}()
	f.t.Cleanup(func() {
		r.cancel()
		// A held cache blocks its consumer, and with it the wait below.
		if o.freezer != nil {
			o.freezer.release()
		}
		<-r.done
	})

	return r
}

// waitSynced blocks until the replica's caches have completed their replay
// under the current generation.
func (r *replica) waitSynced() {
	r.t.Helper()
	waitFor(r.t, r.name+" caches synced", func() bool {
		return r.caches.Synced(r.client.Generation())
	})
}

// live is what the replica's caches show for one Service, under the generation the replica reads through.
// The generation is read fresh on every call,
// so a predicate polling to convergence follows a move rather than holding the generation it started under.
// Both results are returned because a caller waiting for a Service to go quiet needs them apart:
// a cache that has not replayed under that generation answers no, which is not the same answer as not live,
// and folding the two would end such a wait on a cache that has said nothing.
func (r *replica) live(ns, svc string) (live, ok bool) {
	return r.caches.Live(r.client.Generation(), ns, svc)
}

// reserve takes one reservation under the generation the replica reads through,
// which is the generation a Session carries into the same call on the request path.
func (r *replica) reserve(ns, svc string) (*Reservation, error) {
	return r.pub.Reserve(r.client.Generation(), ns, svc)
}

// waitCache blocks until pred holds over the replica's caches, so a test never
// calls a tick against a cache that has not yet seen its own setup.
func (r *replica) waitCache(what string, pred func(*Caches) bool) {
	r.t.Helper()
	waitFor(r.t, r.name+" cache "+what, func() bool { return pred(r.caches) })
}

// tick runs one scheduler pass.
func (r *replica) tick() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	r.sched.tick(ctx)
}

// releaseResolved evaluates the release rule once, as a tick would.
func (r *replica) releaseResolved() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	r.pub.ReleaseResolved(ctx, r.jobsView())
}

// loopJobsView is the Jobs bucket this replica's loops see: the same view,
// through whatever wrapper the test installed on their client.
func (r *replica) loopJobsView() natskv.KV {
	r.t.Helper()
	stores, err := r.loopClient.View(r.loopClient.Generation())
	if err != nil {
		r.t.Fatalf("loop view for %s: %v", r.name, err)
	}

	return stores.Jobs
}

// jobsView is the replica's own generation-bound Jobs bucket.
func (r *replica) jobsView() natskv.KV {
	r.t.Helper()
	stores, err := r.client.View(r.client.Generation())
	if err != nil {
		r.t.Fatalf("view for %s: %v", r.name, err)
	}

	return stores.Jobs
}

// testLeaseTTL is the lease every test worker runs under.
const testLeaseTTL = 60 * time.Second

// testPGOConfig is the PGO block a worker runs under.
func testPGOConfig(mutate ...func(*config.PGOConfig)) config.PGOConfig {
	cfg := config.PGOConfig{
		Enabled:      true,
		ConfigAPI:    "enabled",
		LeaseTTL:     testLeaseTTL,
		MaxAttempts:  3,
		JobRetention: 168 * time.Hour,
		Limits:       testLimits(),
		Defaults:     testDefaults(),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

// newWorker builds a worker on this replica's connection, caches, clock,
// recorder, and logger, with run standing in for the work body.
func (r *replica) newWorker(run runFunc, mutate ...func(*config.PGOConfig)) *Worker {
	r.t.Helper()
	w := r.newRoundsWorker(&Rounds{}, mutate...)
	w.run = run

	return w
}

// newRoundsWorker builds a worker whose work body is the real round loop.
func (r *replica) newRoundsWorker(rounds *Rounds, mutate ...func(*config.PGOConfig)) *Worker {
	r.t.Helper()
	cfg := testPGOConfig(mutate...)
	if len(mutate) == 0 {
		cfg.Limits = r.limits
	}

	return NewWorker(r.loopClient, r.caches, cfg,
		Owner{Instance: r.name, Pod: r.name}, rounds, r.clock, r.recorder, r.logs.logger())
}

// newSweeper builds a sweeper on this replica's connection, caches, clock,
// recorder, and logger.
func (r *replica) newSweeper(mutate ...func(*config.PGOConfig)) *Sweeper {
	r.t.Helper()

	return NewSweeper(r.loopClient, r.caches, testPGOConfig(mutate...),
		Owner{Instance: r.name, Pod: r.name}, r.clock, r.recorder, r.logs.logger())
}

// sweepNow runs one sweeper pass.
func sweepNow(t *testing.T, s *Sweeper) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	s.sweep(ctx)
}

// scanNow runs one worker pass.
func scanNow(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	w.scan(ctx)
}

// activeSlots is how many of this replica's maxActiveCollections are taken.
func (w *Worker) activeSlots() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.active
}

// runStub stands in for the work goroutine a later task implements.
// A test starts it, watches its context, and releases it by hand.
type runStub struct {
	result       workResult
	ignoreCancel bool

	mu        sync.Mutex
	inputs    []workInput
	startedCh chan struct{}
	startOnce sync.Once
	cancelCh  chan struct{}
	cancelOne sync.Once
	releaseCh chan struct{}
	relOnce   sync.Once
}

// newRunStub returns a stub held until release runs.
func newRunStub(res workResult) *runStub {
	return &runStub{
		result:    res,
		startedCh: make(chan struct{}),
		cancelCh:  make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
}

// newImmediateRunStub returns a stub that returns as soon as it is entered.
func newImmediateRunStub(res workResult) *runStub {
	s := newRunStub(res)
	s.release()

	return s
}

// fn is the runFunc the worker calls.
func (s *runStub) fn() runFunc {
	return func(ctx context.Context, in workInput) workResult {
		s.mu.Lock()
		s.inputs = append(s.inputs, in)
		s.mu.Unlock()
		s.startOnce.Do(func() { close(s.startedCh) })

		go func() {
			<-ctx.Done()
			s.cancelOne.Do(func() { close(s.cancelCh) })
		}()

		if s.ignoreCancel {
			<-s.releaseCh
		} else {
			select {
			case <-s.releaseCh:
			case <-ctx.Done():
			}
		}

		return s.result
	}
}

// release lets the stub return.
func (s *runStub) release() { s.relOnce.Do(func() { close(s.releaseCh) }) }

// waitStarted blocks until the work body has been entered.
func (s *runStub) waitStarted(t *testing.T) workInput {
	t.Helper()
	select {
	case <-s.startedCh:
	case <-time.After(fixtureTimeout):
		t.Fatalf("the work body was never entered within %s", fixtureTimeout)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.inputs[0]
}

// waitCancelled blocks until the work context has been cancelled.
func (s *runStub) waitCancelled(t *testing.T) {
	t.Helper()
	select {
	case <-s.cancelCh:
	case <-time.After(fixtureTimeout):
		t.Fatalf("the work context was not cancelled within %s", fixtureTimeout)
	}
}

// cancelled reports whether the work context has been cancelled yet.
func (s *runStub) cancelled() bool {
	select {
	case <-s.cancelCh:
		return true
	default:
		return false
	}
}

// calls is how many times the work body was entered.
func (s *runStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.inputs)
}

// trapRun fails the test if the work body is ever entered.
func trapRun(t *testing.T) runFunc {
	return func(_ context.Context, in workInput) workResult {
		t.Errorf("the work body ran for collection %s, which must never be profiled", in.Record.ID)

		return workResult{Reason: ReasonNoSamples}
	}
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
		time.Sleep(2 * time.Millisecond)
	}
}

// fakeClock drives every time-based decision in these tests, so no test waits
// on wall-clock time.
// Moving it fires every timer and ticker whose time has come, so a test states
// what the clock did rather than which channel to poke.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	tickers []*fakeTicker
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now.UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// Set moves the clock to an absolute time and fires what is now due.
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
	c.fireLocked()
}

// Advance moves the clock forward by d and fires what is now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fireLocked()
}

// fireLocked delivers to every timer and ticker whose time has come.
// Every channel is buffered to one and the sends never block, so a consumer
// that is busy sees one tick for whatever it missed, as a real ticker does.
func (c *fakeClock) fireLocked() {
	for _, t := range c.timers {
		if t.active && !t.deadline.After(c.now) {
			t.active = false
			select {
			case t.ch <- c.now:
			default:
			}
		}
	}
	for _, t := range c.tickers {
		if t.isStopped() || t.period <= 0 {
			continue
		}
		for !t.next.After(c.now) {
			t.next = t.next.Add(t.period)
			select {
			case t.ch <- c.now:
			default:
			}
		}
	}
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, ch: make(chan time.Time, 1), deadline: c.now.Add(d), active: true}
	c.timers = append(c.timers, t)
	if d <= 0 {
		t.active = false
		t.ch <- c.now
	}

	return t
}

func (c *fakeClock) NewTicker(d time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{ch: make(chan time.Time, 1), period: d, next: c.now.Add(d)}
	c.tickers = append(c.tickers, t)

	return t
}

// armedTimers is how many timers are waiting for their deadline.
// A timer that has fired or been stopped is not counted.
func (c *fakeClock) armedTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}

	return n
}

// tickerCount is how many tickers this clock has handed out.
func (c *fakeClock) tickerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.tickers)
}

type fakeTimer struct {
	c        *fakeClock
	ch       chan time.Time
	deadline time.Time
	active   bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.deadline = t.c.now.Add(d)
	t.active = true
	if d <= 0 {
		t.active = false
		select {
		case t.ch <- t.c.now:
		default:
		}
	}

	return was
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false

	return was
}

type fakeTicker struct {
	ch     chan time.Time
	period time.Duration
	next   time.Time

	mu      sync.Mutex
	stopped bool
}

func (t *fakeTicker) C() <-chan time.Time { return t.ch }

func (t *fakeTicker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
}

func (t *fakeTicker) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.stopped
}

// countingRecorder counts the metric rows a test asserts on.
type countingRecorder struct {
	mu          sync.Mutex
	slots       map[string]int
	collections map[string]int
	samples     map[string]int
	sweeps      map[string]int
	storeFails  map[string]int
	durations   []time.Duration
	active      int
	activePeak  int
}

func newCountingRecorder() *countingRecorder {
	return &countingRecorder{
		slots:       make(map[string]int),
		collections: make(map[string]int),
		samples:     make(map[string]int),
		sweeps:      make(map[string]int),
		storeFails:  make(map[string]int),
	}
}

// sampleRows is a copy of the profgate_collection_samples_total rows.
func (r *countingRecorder) sampleRows() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.samples))
	for k, v := range r.samples {
		out[k] = v
	}

	return out
}

// collectionRows is a copy of the profgate_collections_total rows.
func (r *countingRecorder) collectionRows() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.collections))
	for k, v := range r.collections {
		out[k] = v
	}

	return out
}

// durationCount is how many profgate_collection_duration_seconds observations
// were made, and peakActive the highest the active gauge ever reached.
func (r *countingRecorder) durationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.durations)
}

func (r *countingRecorder) activeGauge() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.active
}

func (r *countingRecorder) peakActive() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.activePeak
}

// scheduleSlots is a copy of the profgate_schedule_slots_total rows.
func (r *countingRecorder) scheduleSlots() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.slots))
	for k, v := range r.slots {
		out[k] = v
	}

	return out
}

func (r *countingRecorder) ScheduleSlot(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slots[result]++
}

func (r *countingRecorder) Request(metrics.Endpoint, string, string, time.Duration) {}
func (r *countingRecorder) Confirm(string)                                          {}
func (r *countingRecorder) ProfilesInFlight(int)                                    {}
func (r *countingRecorder) DiscoverySynced(bool)                                    {}
func (r *countingRecorder) PGOSyncedFrom(func() bool)                               {}
func (r *countingRecorder) Collection(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collections[result]++
}

func (r *countingRecorder) CollectionSample(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples[result]++
}

func (r *countingRecorder) CollectionDuration(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durations = append(r.durations, d)
}

// sweeperDeletes is a copy of the profgate_sweeper_deletes_total rows.
func (r *countingRecorder) sweeperDeletes() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.sweeps))
	for k, v := range r.sweeps {
		out[k] = v
	}

	return out
}

func (r *countingRecorder) SweeperDelete(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweeps[kind]++
}

func (r *countingRecorder) StoreFailure(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storeFails[op]++
}

func (r *countingRecorder) CollectionsActive(delta int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active += delta
	if r.active > r.activePeak {
		r.activePeak = r.active
	}
}

func (r *countingRecorder) NATSConnected(bool) {}

func (r *countingRecorder) TLSReload(string) {}

func (r *countingRecorder) TLSCertificateExpiry(time.Time) {}

func (r *countingRecorder) AuthFailure(string, string) {}

func (r *countingRecorder) AuthSessionIssued() {}

func (r *countingRecorder) JWKSRefresh(string) {}

func (r *countingRecorder) JWKSKeys(int) {}

func (r *countingRecorder) JWKSFetched(time.Time) {}

func (r *countingRecorder) AuthFileReload(string, string) {}

func (r *countingRecorder) CookieKeys([]metrics.CookieKey) {}

// logCapture collects the records a replica logs, so a test can count
// transition records and violation warnings.
type logCapture struct {
	mu      sync.Mutex
	records []capturedLog
}

// capturedLog is one log record flattened to its message and attributes.
type capturedLog struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

func newLogCapture() *logCapture { return &logCapture{} }

func (c *logCapture) logger() *slog.Logger { return slog.New(&captureHandler{c: c}) }

// with returns every captured record whose message is msg.
func (c *logCapture) with(msg string) []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedLog
	for _, r := range c.records {
		if r.Message == msg {
			out = append(out, r)
		}
	}

	return out
}

// transitions returns the transition records emitted, in order.
func (c *logCapture) transitions() []capturedLog { return c.with("collection transition") }

// text is every captured record flattened, for asserting what is absent.
func (c *logCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, r := range c.records {
		b.WriteString(r.Message)
		for k, v := range r.Attrs {
			fmt.Fprintf(&b, " %s=%v", k, v)
		}
		b.WriteByte('\n')
	}

	return b.String()
}

type captureHandler struct {
	c     *logCapture
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	entry := capturedLog{Level: r.Level, Message: r.Message, Attrs: make(map[string]any)}
	for _, a := range h.attrs {
		entry.Attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		entry.Attrs[a.Key] = a.Value.Any()

		return true
	})
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	h.c.records = append(h.c.records, entry)

	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &captureHandler{c: h.c, attrs: make([]slog.Attr, 0, len(h.attrs)+len(attrs))}
	next.attrs = append(next.attrs, h.attrs...)
	next.attrs = append(next.attrs, attrs...)

	return next
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// hookClient wraps a real client so a test can count the store calls a tick
// issues and turn one of them into ErrUnavailable.
// Connected, Generation, and Synced stay the real client's: the seam's own
// generation behavior is proven in internal/natskv, and a fake that
// re-implemented it here would prove only that the fake was written correctly.
type hookClient struct {
	inner natskv.Client
	hook  *kvHook
}

func newHookClient(inner natskv.Client, hook *kvHook) *hookClient {
	return &hookClient{inner: inner, hook: hook}
}

func (c *hookClient) Connected() bool        { return c.inner.Connected() }
func (c *hookClient) Generation() uint64     { return c.inner.Generation() }
func (c *hookClient) Synced(gen uint64) bool { return c.inner.Synced(gen) }

func (c *hookClient) View(gen uint64) (natskv.Stores, error) {
	stores, err := c.inner.View(gen)
	if err != nil {
		return stores, err
	}
	stores.Jobs = &hookKV{KV: stores.Jobs, hook: c.hook}
	stores.Config = &hookKV{KV: stores.Config, hook: c.hook}
	stores.Artifacts = &hookObjects{Objects: stores.Artifacts, hook: c.hook}

	return stores, nil
}

// hookObjects is the Object Store seen through a kvHook, so a test can hold a
// Put at a barrier and see what was deleted.
type hookObjects struct {
	natskv.Objects
	hook *kvHook
}

func (o *hookObjects) Put(ctx context.Context, name string, r io.Reader) error {
	return o.hook.run("put", name, func() error { return o.Objects.Put(ctx, name, r) })
}

func (o *hookObjects) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := o.hook.run("get-object", name, func() error {
		var innerErr error
		rc, innerErr = o.Objects.Get(ctx, name)

		return innerErr
	})

	return rc, err
}

func (o *hookObjects) List(ctx context.Context) ([]natskv.ObjectInfo, error) {
	var out []natskv.ObjectInfo
	err := o.hook.run("list-objects", "", func() error {
		var innerErr error
		out, innerErr = o.Objects.List(ctx)

		return innerErr
	})

	return out, err
}

func (o *hookObjects) Delete(ctx context.Context, name string) error {
	return o.hook.run("delete-object", name, func() error { return o.Objects.Delete(ctx, name) })
}

// kvHook records every operation and lets a test intervene before or after one.
type kvHook struct {
	mu      sync.Mutex
	calls   []kvCall
	watches []*watchRecord
	// before runs ahead of the real call; a true second result short-circuits
	// it with the returned error, standing in for an uncommitted write.
	before func(op, key string) (error, bool)
	// after rewrites the real call's error, standing in for a write that
	// committed and lost its acknowledgement.
	after func(op, key string, err error) error
}

// kvCall is one recorded store operation.
type kvCall struct {
	Op  string
	Key string
}

// watchRecord is one Watch a hookKV opened: the context it was opened under,
// and a channel closed once the entries it delivers have run out.
type watchRecord struct {
	Prefix string
	Ctx    context.Context
	ended  chan struct{}
}

// Ended reports whether the watch has delivered its last entry.
func (w *watchRecord) Ended() bool {
	select {
	case <-w.ended:
		return true
	default:
		return false
	}
}

// addWatch records one opened watch.
func (h *kvHook) addWatch(w *watchRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watches = append(h.watches, w)
}

// watchesOpened is a copy of every watch opened so far, in the order they were opened.
func (h *kvHook) watchesOpened() []*watchRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*watchRecord, len(h.watches))
	copy(out, h.watches)

	return out
}

// watchesLive counts the watches that have not yet ended.
func (h *kvHook) watchesLive() int {
	live := 0
	for _, w := range h.watchesOpened() {
		if !w.Ended() {
			live++
		}
	}

	return live
}

func (h *kvHook) record(op, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, kvCall{Op: op, Key: key})
}

// setBefore installs the ahead-of-the-call intervention.
func (h *kvHook) setBefore(fn func(op, key string) (error, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.before = fn
}

// setAfter installs the rewrite of the call's error.
func (h *kvHook) setAfter(fn func(op, key string, err error) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.after = fn
}

// reset forgets every call recorded so far.
func (h *kvHook) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
}

// callsFor is every recorded call of one operation on one key.
func (h *kvHook) callsFor(op, key string) []kvCall {
	var out []kvCall
	for _, c := range h.operations() {
		if c.Op == op && c.Key == key {
			out = append(out, c)
		}
	}

	return out
}

// operations is a copy of every recorded call.
func (h *kvHook) operations() []kvCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]kvCall, len(h.calls))
	copy(out, h.calls)

	return out
}

// run wraps one operation in the hook's interventions.
func (h *kvHook) run(op, key string, call func() error) error {
	h.record(op, key)
	h.mu.Lock()
	before, after := h.before, h.after
	h.mu.Unlock()

	if before != nil {
		if err, handled := before(op, key); handled {
			return err
		}
	}
	err := call()
	if after != nil {
		err = after(op, key, err)
	}

	return err
}

// hookKV is one bucket seen through a kvHook.
type hookKV struct {
	natskv.KV
	hook *kvHook
}

func (k *hookKV) Get(ctx context.Context, key string) (natskv.Entry, error) {
	var e natskv.Entry
	err := k.hook.run("get", key, func() error {
		var innerErr error
		e, innerErr = k.KV.Get(ctx, key)

		return innerErr
	})

	return e, err
}

func (k *hookKV) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	var rev uint64
	err := k.hook.run("create", key, func() error {
		var innerErr error
		rev, innerErr = k.KV.Create(ctx, key, value)

		return innerErr
	})

	return rev, err
}

func (k *hookKV) Update(ctx context.Context, key string, value []byte, expected uint64) (uint64, error) {
	var rev uint64
	err := k.hook.run("update", key, func() error {
		var innerErr error
		rev, innerErr = k.KV.Update(ctx, key, value, expected)

		return innerErr
	})

	return rev, err
}

func (k *hookKV) Delete(ctx context.Context, key string, expected uint64) error {
	return k.hook.run("delete", key, func() error { return k.KV.Delete(ctx, key, expected) })
}

// Keys is recorded under the prefix it lists, which is the key the sweeper's
// probe pass acts on.
func (k *hookKV) Keys(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	err := k.hook.run("keys", prefix, func() error {
		var innerErr error
		out, innerErr = k.KV.Keys(ctx, prefix)

		return innerErr
	})

	return out, err
}

// Watch is recorded under the prefix it opens, so a test can fail one source's
// open and then see what became of the watches opened before it.
// The channel it returns carries the underlying watch's entries
// and closes when that watch closes,
// which kvView.Watch does once the context it was given is done.
func (k *hookKV) Watch(ctx context.Context, prefix string) (<-chan natskv.Entry, error) {
	var src <-chan natskv.Entry
	if err := k.hook.run("watch", prefix, func() error {
		var innerErr error
		src, innerErr = k.KV.Watch(ctx, prefix)

		return innerErr
	}); err != nil {
		return nil, err
	}

	rec := &watchRecord{Prefix: prefix, Ctx: ctx, ended: make(chan struct{})}
	k.hook.addWatch(rec)
	out := make(chan natskv.Entry)
	go func() {
		for e := range src {
			out <- e
		}
		// ended closes ahead of out,
		// so a caller that has seen its consumer finish never finds the record still open.
		close(rec.ended)
		close(out)
	}()

	return out, nil
}

// cacheFreezer holds one cache's delivery.
// Armed after the replay it leaves a stale cache while the seam's own watch
// stays synced, which is the "frozen watch" of the scheduling cases;
// armed from the start it holds the replay itself, so the caches never
// complete a generation and the replay barrier stays closed.
type cacheFreezer struct {
	kinds map[cacheKind]struct{}

	mu     sync.Mutex
	cond   *sync.Cond
	frozen bool
}

// newFreezer returns a freezer over the named caches that passes everything
// until freeze runs.
func newFreezer(kinds ...cacheKind) *cacheFreezer {
	fz := &cacheFreezer{kinds: make(map[cacheKind]struct{}, len(kinds))}
	fz.cond = sync.NewCond(&fz.mu)
	for _, k := range kinds {
		fz.kinds[k] = struct{}{}
	}

	return fz
}

// newArmedFreezer returns a freezer that holds the caches' replay from the start.
func newArmedFreezer(kinds ...cacheKind) *cacheFreezer {
	fz := newFreezer(kinds...)
	fz.freeze()

	return fz
}

// gate is the Caches.applyGate this freezer installs.
func (fz *cacheFreezer) gate(kind cacheKind, _ natskv.Entry) {
	if _, ok := fz.kinds[kind]; !ok {
		return
	}
	fz.mu.Lock()
	defer fz.mu.Unlock()
	for fz.frozen {
		fz.cond.Wait()
	}
}

// freeze starts holding; every later entry waits for release.
func (fz *cacheFreezer) freeze() {
	fz.mu.Lock()
	defer fz.mu.Unlock()
	fz.frozen = true
}

// release lets the held entries through, until freeze runs again.
func (fz *cacheFreezer) release() {
	fz.mu.Lock()
	defer fz.mu.Unlock()
	fz.frozen = false
	fz.cond.Broadcast()
}

// fakeDiscovery stands in for the cluster: a fixed target list per call, and a
// per-Pod confirmation answer.
type fakeDiscovery struct {
	mu         sync.Mutex
	rounds     [][]k8s.Target // one entry per call; the last repeats
	calls      int
	err        error
	confirmErr map[string]error
	confirms   int
	selections []k8s.PortSelection // the port selection of every Targets call, in order
}

func newFakeDiscovery(targets ...k8s.Target) *fakeDiscovery {
	return &fakeDiscovery{rounds: [][]k8s.Target{targets}, confirmErr: make(map[string]error)}
}

// perRound makes each call return the next list, so a rollout can be staged.
func newRollingDiscovery(rounds ...[]k8s.Target) *fakeDiscovery {
	return &fakeDiscovery{rounds: rounds, confirmErr: make(map[string]error)}
}

func (d *fakeDiscovery) Targets(_ context.Context, _, _ string, sel k8s.PortSelection) ([]k8s.Target, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.selections = append(d.selections, sel)
	if d.err != nil {
		return nil, d.err
	}
	i := min(d.calls, len(d.rounds)-1)
	d.calls++
	out := make([]k8s.Target, len(d.rounds[i]))
	copy(out, d.rounds[i])

	return out, nil
}

// selected returns the port selections Targets was called with, in order.
func (d *fakeDiscovery) selected() []k8s.PortSelection {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]k8s.PortSelection(nil), d.selections...)
}

func (d *fakeDiscovery) HasSynced() bool { return true }

// Catalog is never called by PGO, which schedules against configured Services rather than a listing.
func (d *fakeDiscovery) Catalog(context.Context, string) ([]k8s.ServiceRef, error) { return nil, nil }

// Explain is never called by PGO, which selects targets rather than explaining an empty listing.
func (d *fakeDiscovery) Explain(context.Context, string, string, k8s.PortSelection) (k8s.Explanation, error) {
	return k8s.Explanation{}, nil
}

func (d *fakeDiscovery) Confirm(_ context.Context, t k8s.Target) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.confirms++

	return d.confirmErr[t.Pod]
}

// podServer is one Pod's pprof endpoint.
type podServer struct {
	target k8s.Target
	server *httptest.Server
	hits   atomic.Int64
}

// newPodServer serves body from a Pod named pod at version.
func newPodServer(t *testing.T, pod, version string, body []byte) *podServer {
	t.Helper()

	return newPodHandler(t, pod, version, func(w http.ResponseWriter, _ *http.Request) {
		//nolint:errcheck // an httptest write that fails fails the assertion instead
		_, _ = w.Write(body)
	})
}

// newPodHandler serves whatever the handler writes.
func newPodHandler(t *testing.T, pod, version string, handler http.HandlerFunc) *podServer {
	t.Helper()
	p := &podServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p.hits.Add(1)
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	p.server = srv
	p.target = targetFor(t, pod, version, srv)

	return p
}

// newTrapServer fails the test if anything dials it.
func newTrapServer(t *testing.T, pod, version string) *podServer {
	t.Helper()

	return newPodHandler(t, pod, version, func(http.ResponseWriter, *http.Request) {
		t.Errorf("the trap pprof server for pod %s was dialed", pod)
	})
}

// targetFor builds the Target that reaches one httptest server.
func targetFor(t *testing.T, pod, version string, srv *httptest.Server) k8s.Target {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", srv.URL, err)
	}

	if port < 0 || port > 65535 {
		t.Fatalf("port %d of %s is out of range", port, srv.URL)
	}

	return k8s.Target{
		Namespace: "payment",
		Service:   "payment-api",
		Pod:       pod,
		Node:      "node-" + pod,
		PodIP:     u.Hostname(),
		//nolint:gosec // G109: the range is checked just above
		Port:    int32(port),
		Version: version,
		UID:     "uid-" + pod,
	}
}

// fixtureProfile reads one testdata profile as it is served.
func fixtureProfile(t *testing.T, name string) []byte {
	t.Helper()
	//nolint:gosec // G304: a fixture path this test built itself
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return b
}

// gzipBytes compresses b, as a pprof endpoint would.
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	return buf.Bytes()
}

// roundsOpts shapes one Rounds under test.
type roundsOpts struct {
	discovery k8s.Discovery
	limits    config.PGOLimits
	clock     *fakeClock
	recorder  metrics.Recorder
	logs      *logCapture
	shuffle   func(n int, swap func(i, j int))
}

// newTestRounds builds a Rounds over the real proxy transport.
func newTestRounds(t *testing.T, o roundsOpts) *Rounds {
	t.Helper()
	if o.limits == (config.PGOLimits{}) {
		o.limits = testLimits()
	}
	if o.clock == nil {
		o.clock = newFakeClock(slotBase)
	}
	if o.logs == nil {
		o.logs = newLogCapture()
	}
	if o.recorder == nil {
		o.recorder = newCountingRecorder()
	}
	if o.shuffle == nil {
		// Identity: a test that cares about order says so.
		o.shuffle = func(int, func(i, j int)) {}
	}

	return NewRounds(RoundsDeps{
		Discovery:    o.discovery,
		Proxy:        proxy.New(proxy.Options{HeaderDeadline: func(int) time.Duration { return 5 * time.Second }}),
		Limits:       o.limits,
		Clock:        o.clock,
		Recorder:     o.recorder,
		Log:          o.logs.logger(),
		VersionLabel: "app.kubernetes.io/version",
		Gateway:      "profgate-test/abc",
		Shuffle:      o.shuffle,
	})
}

// runInput is the workInput a rounds test drives, with an Object Store that
// keeps what was put in memory.
type runInput struct {
	record    Record
	artifacts *memoryObjects
	progress  []Progress
	mu        sync.Mutex
}

// newRunInput builds a claimed record with policy applied.
func newRunInput(t *testing.T, mutate ...func(*Record)) *runInput {
	t.Helper()
	started := slotBase
	deadline := slotBase.Add(time.Hour)
	rec := Record{
		ID:        newID(),
		Namespace: "payment",
		Service:   "payment-api",
		Origin:    OriginSchedule,
		Policy:    schedulerDefaults(t),
		State:     StateRunning,
		Attempt:   1,
		StartedAt: &started,
		Deadline:  &deadline,
		CreatedAt: slotBase,
	}
	rec.Policy.Sampling.Rounds = 1
	rec.Policy.Sampling.MaxParallel = 2
	// Zero by default: the interval between rounds runs on the fake clock, so
	// a test that wants one says so and advances the clock itself.
	rec.Policy.Sampling.RoundInterval = 0
	rec.Policy.Sampling.Duration = Duration(time.Second)
	for _, m := range mutate {
		m(&rec)
	}

	return &runInput{record: rec, artifacts: newMemoryObjects()}
}

// input is the workInput the work body is called with.
func (i *runInput) input() workInput {
	return workInput{
		Record:    i.record,
		Artifacts: i.artifacts,
		Progress: func(p Progress) {
			i.mu.Lock()
			defer i.mu.Unlock()
			i.progress = append(i.progress, p)
		},
	}
}

// memoryObjects is an Object Store that keeps what it is given.
type memoryObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
	// beforePut runs before every Put, so a test can hold one at a barrier.
	beforePut func(name string)
}

func newMemoryObjects() *memoryObjects {
	return &memoryObjects{objects: make(map[string][]byte)}
}

func (m *memoryObjects) Put(_ context.Context, name string, r io.Reader) error {
	m.mu.Lock()
	hook, err := m.beforePut, m.putErr
	m.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	if err != nil {
		return err
	}
	b, rerr := io.ReadAll(r)
	if rerr != nil {
		return rerr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[name] = b

	return nil
}

func (m *memoryObjects) Get(_ context.Context, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[name]
	if !ok {
		return nil, natskv.ErrObjectNotFound
	}

	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memoryObjects) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, name)

	return nil
}

func (m *memoryObjects) List(context.Context) ([]natskv.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]natskv.ObjectInfo, 0, len(m.objects))
	for name, b := range m.objects {
		out = append(out, natskv.ObjectInfo{Name: name, Size: uint64(len(b))})
	}

	return out, nil
}

// object returns what was stored under name.
func (m *memoryObjects) object(name string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[name]

	return b, ok
}

// count is how many objects the store holds.
func (m *memoryObjects) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.objects)
}

var _ natskv.Objects = (*memoryObjects)(nil)

// gunzipBytes decompresses b, which is how a profile reaches the decoder.
func gunzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}

	return out
}
