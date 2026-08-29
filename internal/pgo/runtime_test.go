package pgo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/natskv"
)

// newRuntime binds a runtime over this replica's connection, caches,
// publisher, clock, recorder, and logger, which is what cmd/profgate binds
// once its NATS preflight has passed.
func (r *replica) newRuntime() *Runtime {
	r.t.Helper()

	rt := NewRuntime()
	rt.Bind(Bundle{
		Client:    r.client,
		Caches:    r.caches,
		Publisher: r.pub,
		Bucket:    NewTokenBucket(r.limits.OnDemandPerMinute, r.clock),
		Defaults:  schedulerDefaults(r.t),
		Limits:    r.limits,
		Clock:     r.clock,
		Recorder:  r.recorder,
		Instance:  r.name,
		Log:       r.logs.logger(),
	})

	return rt
}

// session takes one request's view, failing the test when the barrier refuses.
func (r *replica) session(rt *Runtime) *Session {
	r.t.Helper()

	sess, err := rt.Session()
	if err != nil {
		r.t.Fatalf("session: %v", err)
	}

	return sess
}

// TestRuntimeIsUnavailableUntilBound proves the late-binding seam: the HTTP
// server is built before the NATS preflight has succeeded, and until Bind runs
// every request is told the runtime cannot answer.
func TestRuntimeIsUnavailableUntilBound(t *testing.T) {
	rt := NewRuntime()

	if rt.bound() {
		t.Error("a fresh runtime reports itself bound")
	}
	_, err := rt.Session()
	if !errors.Is(err, natskv.ErrUnavailable) {
		t.Fatalf("Session before Bind error = %v, want ErrUnavailable", err)
	}

	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	rt = r.newRuntime()

	if !rt.bound() {
		t.Error("a bound runtime reports itself unbound")
	}
	if _, err := rt.Session(); err != nil {
		t.Fatalf("Session after Bind error = %v, want nil", err)
	}
}

// TestSessionWaitsForBothHalvesOfTheBarrier pins the half a reader is most
// likely to drop: the seam marks a watch synced when it forwards the replay
// marker into its channel, which is before the cache has applied a single
// entry of it, so a session taken on the seam's flag alone would decide from a
// cache that has seen nothing.
func TestSessionWaitsForBothHalvesOfTheBarrier(t *testing.T) {
	f := startPGO(t)
	freezer := newArmedFreezer(cacheJobs)
	r := f.newReplica("a", replicaOpts{freezer: freezer})
	rt := r.newRuntime()

	gen := r.client.Generation()
	waitFor(t, "the seam's own watches synced", func() bool { return r.client.Synced(gen) })
	if r.caches.Synced(gen) {
		t.Fatal("the held cache reports itself synced")
	}
	if _, err := rt.Session(); !errors.Is(err, natskv.ErrUnavailable) {
		t.Fatalf("Session with a replaying cache error = %v, want ErrUnavailable", err)
	}

	freezer.release()
	r.waitSynced()
	if _, err := rt.Session(); err != nil {
		t.Fatalf("Session once the caches replayed error = %v, want nil", err)
	}
}

// TestSessionIsUnavailableWhileDisconnected proves a lost connection closes
// the routes at once: the generation moves in the disconnected callback, so
// the barrier is false for the new generation before the connection is usable
// again.
func TestSessionIsUnavailableWhileDisconnected(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	rt := r.newRuntime()

	f.stopServer()
	waitFor(t, "the connection reported down", func() bool { return !r.client.Connected() })

	if _, err := rt.Session(); !errors.Is(err, natskv.ErrUnavailable) {
		t.Fatalf("Session while disconnected error = %v, want ErrUnavailable", err)
	}
}

// TestSessionReadsAndWritesRecords covers the record operations the
// Collection routes are built from: a fresh read carries the stored bytes and
// the revision, a write at that revision commits, and a write at a stale one
// loses.
func TestSessionReadsAndWritesRecords(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	sess := r.session(r.newRuntime())
	ctx := context.Background()

	if _, err := sess.ReadRecord(ctx, newID()); !errors.Is(err, natskv.ErrKeyNotFound) {
		t.Fatalf("ReadRecord of a missing collection error = %v, want ErrKeyNotFound", err)
	}

	id := f.seedRecord("payment", "payment-api", StatePending)
	stored, err := sess.ReadRecord(ctx, id)
	if err != nil {
		t.Fatalf("ReadRecord error = %v", err)
	}
	if stored.Record.ID != id || stored.Revision == 0 || len(stored.Value) == 0 {
		t.Fatalf("ReadRecord = %+v, want the record, its bytes, and a revision", stored)
	}

	rec := stored.Record
	rec.State = StateCancelled
	rec.Reason = ReasonCancelledByAPI
	if err := sess.WriteRecord(ctx, rec, stored.Revision); err != nil {
		t.Fatalf("WriteRecord at the read revision error = %v", err)
	}
	if got := f.record(id).State; got != StateCancelled {
		t.Errorf("state = %q, want %q", got, StateCancelled)
	}
	if err := sess.WriteRecord(ctx, rec, stored.Revision); !errors.Is(err, natskv.ErrRevisionMismatch) {
		t.Fatalf("WriteRecord at a stale revision error = %v, want ErrRevisionMismatch", err)
	}
}

// TestSessionReadsAndWritesOverrides covers the policy route's store
// operations, including the revision each one is conditional on.
func TestSessionReadsAndWritesOverrides(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	sess := r.session(r.newRuntime())
	ctx := context.Background()

	if _, _, err := sess.ReadOverride(ctx, "payment", "payment-api"); !errors.Is(err, natskv.ErrKeyNotFound) {
		t.Fatalf("ReadOverride of a Service without one error = %v, want ErrKeyNotFound", err)
	}

	stored := StoredOverride{Policy: enabledOverride(), UpdatedBy: "anonymous", UpdatedAt: slotBase}
	rev, err := sess.CreateOverride(ctx, "payment", "payment-api", stored)
	if err != nil {
		t.Fatalf("CreateOverride error = %v", err)
	}
	if _, err := sess.CreateOverride(ctx, "payment", "payment-api", stored); !errors.Is(err, natskv.ErrKeyExists) {
		t.Fatalf("CreateOverride of an existing key error = %v, want ErrKeyExists", err)
	}

	next, err := sess.UpdateOverride(ctx, "payment", "payment-api", stored, rev)
	if err != nil {
		t.Fatalf("UpdateOverride error = %v", err)
	}
	if _, err := sess.UpdateOverride(ctx, "payment", "payment-api", stored, rev); !errors.Is(err, natskv.ErrRevisionMismatch) {
		t.Fatalf("UpdateOverride at a stale revision error = %v, want ErrRevisionMismatch", err)
	}

	if err := sess.DeleteOverride(ctx, "payment", "payment-api", rev); !errors.Is(err, natskv.ErrRevisionMismatch) {
		t.Fatalf("DeleteOverride at a stale revision error = %v, want ErrRevisionMismatch", err)
	}
	if err := sess.DeleteOverride(ctx, "payment", "payment-api", next); err != nil {
		t.Fatalf("DeleteOverride error = %v", err)
	}
	if f.hasKey(f.config, overrideKey("payment", "payment-api")) {
		t.Error("the override survived its delete")
	}
}

// TestSessionReleasesOnlyItsOwnActiveKey proves the cancel handler frees the
// Service by the same rule the worker does: a key that names another
// Collection is left alone, so a cancel can never release a successor's claim.
func TestSessionReleasesOnlyItsOwnActiveKey(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	sess := r.session(r.newRuntime())
	ctx := context.Background()

	id := f.seedLiveCollection("payment", "payment-api", StateRunning)
	other := f.record(id)
	other.ID = newID()

	sess.ReleaseActive(ctx, other)
	if !f.hasKey(f.jobs, activeKey("payment", "payment-api")) {
		t.Fatal("releasing for another Collection deleted the active key")
	}

	sess.ReleaseActive(ctx, f.record(id))
	if f.hasKey(f.jobs, activeKey("payment", "payment-api")) {
		t.Error("the active key survived its own Collection's release")
	}
}

// TestSessionRecordsOneTransitionPerCommit pins the ownership rule: whichever
// conditional update wins emits the transition record and the metric row, so
// no transition is counted twice.
func TestSessionRecordsOneTransitionPerCommit(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	sess := r.session(r.newRuntime())

	rec := f.record(f.seedRecord("payment", "payment-api", StateRunning))
	rec.State = StateExpired
	sess.RecordTransition(rec)

	if got := r.recorder.collectionRows()[string(StateExpired)]; got != 1 {
		t.Errorf("Collection(%q) rows = %d, want 1", StateExpired, got)
	}
	transitions := r.logs.transitions()
	if len(transitions) != 1 {
		t.Fatalf("transition records = %d, want 1", len(transitions))
	}
	if got := transitions[0].Attrs["instance"]; got != r.name {
		t.Errorf("transition instance = %v, want %q", got, r.name)
	}
}

// TestCachesCollectionsListsNewestFirst covers the listing the Collections
// route answers from: one Service's Collections, newest first, capped.
func TestCachesCollectionsListsNewestFirst(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	want := make([]string, 0, 3)
	for i := range 3 {
		id := f.seedRecord("payment", "payment-api", StateCompleted)
		rec := f.record(id)
		rec.CreatedAt = slotBase.Add(time.Duration(i) * time.Minute)
		rec.Origin = OriginAPI
		rec.Attempt = i + 1
		rec.ResolvedVersion = "1.42.3"
		f.putJSON(f.jobs, jobKey(id), rec)
		want = append([]string{id}, want...)
	}
	other := f.seedRecord("payment", "other-api", StateCompleted)

	r.waitCache("three collections", func(c *Caches) bool { return len(c.Collections("payment", "payment-api")) == 3 })
	got := r.caches.Collections("payment", "payment-api")

	for i, view := range got {
		if view.ID != want[i] {
			t.Errorf("entry %d = %s, want %s (newest first)", i, view.ID, want[i])
		}
		if view.Origin != OriginAPI || view.ResolvedVersion != "1.42.3" || view.Attempt == 0 {
			t.Errorf("entry %d = %+v, want the record's origin, attempt, and version", i, view)
		}
		if view.ID == other {
			t.Errorf("entry %d is another Service's Collection", i)
		}
	}
}

// TestCachesCollectionsCapsTheListing pins the ceiling of section "List
// Collections": at most 100 entries and no pagination behind them.
func TestCachesCollectionsCapsTheListing(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	for i := range maxListCollections + 5 {
		id := f.seedRecord("payment", "payment-api", StateCompleted)
		rec := f.record(id)
		rec.CreatedAt = slotBase.Add(time.Duration(i) * time.Minute)
		f.putJSON(f.jobs, jobKey(id), rec)
	}

	r.waitCache("every collection", func(c *Caches) bool {
		return len(c.jobEntries()) == maxListCollections+5
	})
	if got := len(r.caches.Collections("payment", "payment-api")); got != maxListCollections {
		t.Errorf("listing length = %d, want %d", got, maxListCollections)
	}
}

// TestCachesOverrideCarriesItsRevision proves the on-demand handler reads the
// stored override with the revision a Collection records as its
// configRevision, which is never 0 for a Service that has one.
func TestCachesOverrideCarriesItsRevision(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	if _, rev := r.caches.Override("payment", "payment-api"); rev != 0 {
		t.Errorf("revision without an override = %d, want 0", rev)
	}

	want := f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour)))
	r.waitCache("the override", func(c *Caches) bool {
		_, rev := c.Override("payment", "payment-api")

		return rev == want
	})

	override, rev := r.caches.Override("payment", "payment-api")
	if rev != want {
		t.Errorf("revision = %d, want %d", rev, want)
	}
	if override == nil || override.Schedule == nil || override.Schedule.Every.Duration() != time.Hour {
		t.Errorf("override = %+v, want the stored every", override)
	}
}

// TestResolveVersion is round 0's version rule, which the on-demand handler
// applies as its advisory pre-check and the round loop applies for real.
func TestResolveVersion(t *testing.T) {
	labelled := func(pod, version string) k8s.Target {
		return k8s.Target{Namespace: "payment", Service: "payment-api", Pod: pod, UID: "uid-" + pod, Version: version}
	}

	cases := []struct {
		name    string
		targets []k8s.Target
		pin     string
		version string
		reason  string
	}{
		{"one version", []k8s.Target{labelled("a", "1.0"), labelled("b", "1.0")}, "", "1.0", ""},
		{"two versions", []k8s.Target{labelled("a", "1.0"), labelled("b", "2.0")}, "", "", ReasonVersionConflict},
		{"no labels", []k8s.Target{labelled("a", ""), labelled("b", "")}, "", "", ReasonVersionMissing},
		{"no targets", nil, "", "", ReasonVersionMissing},
		{"a pin picks one of two", []k8s.Target{labelled("a", "1.0"), labelled("b", "2.0")}, "2.0", "2.0", ""},
		{"a pin nothing carries", []k8s.Target{labelled("a", "1.0")}, "9.9", "", ReasonVersionMissing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, reason := ResolveVersion(tc.targets, tc.pin)
			if version != tc.version || reason != tc.reason {
				t.Errorf("ResolveVersion = (%q,%q), want (%q,%q)", version, reason, tc.version, tc.reason)
			}
		})
	}
}

// TestTokenBucketBoundsOnDemandCreation proves the per-replica on-demand rate
// limit: a full bucket of onDemandPerMinute tokens, refilled at that rate, so
// a caller with pgo.collect across many Services cannot outrun the workers.
func TestTokenBucketBoundsOnDemandCreation(t *testing.T) {
	clock := newFakeClock(slotBase)
	bucket := NewTokenBucket(10, clock)

	for i := range 10 {
		if !bucket.Take() {
			t.Fatalf("token %d refused from a full bucket", i)
		}
	}
	if bucket.Take() {
		t.Fatal("an eleventh token came out of a bucket of ten")
	}

	clock.Advance(6 * time.Second)
	if !bucket.Take() {
		t.Error("no token after a tenth of a minute at ten a minute")
	}
	if bucket.Take() {
		t.Error("two tokens after a tenth of a minute at ten a minute")
	}

	clock.Advance(time.Hour)
	for i := range 10 {
		if !bucket.Take() {
			t.Fatalf("token %d refused after a refill past capacity", i)
		}
	}
	if bucket.Take() {
		t.Error("the bucket refilled past its capacity")
	}
}

// pulseWait is how long a subscription assertion gives one entry to reach the caches,
// and how long it watches for a pulse that must never arrive.
const pulseWait = 2 * time.Second

// expectPulse fails unless the subscription is woken.
func expectPulse(t *testing.T, what string, pulses <-chan struct{}) {
	t.Helper()

	select {
	case <-pulses:
	case <-time.After(pulseWait):
		t.Fatalf("no pulse for %s within %s", what, pulseWait)
	}
}

// expectNoPulse fails when the subscription is woken.
// The caller has already waited for the entry it means to see no pulse for, so
// the window here only covers the fan-out that would follow it.
func expectNoPulse(t *testing.T, what string, pulses <-chan struct{}) {
	t.Helper()

	select {
	case <-pulses:
		t.Fatalf("a pulse arrived for %s", what)
	case <-time.After(50 * time.Millisecond):
	}
}

// subscriberCount is how many registrations the caches hold for one record.
func subscriberCount(c *Caches, id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.subscribers[id])
}

// TestSubscribeWakesOnlyItsOwnRecord proves the fan-out is per record:
// a request waiting on one Collection is woken by every entry applied for it,
// and by no other Service's traffic.
func TestSubscribeWakesOnlyItsOwnRecord(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	mine := f.seedRecord("payment", "payment-api", StatePending)
	other := f.seedRecord("payment", "payment-web", StatePending)
	r.waitCache("both records", func(c *Caches) bool { return c.hasJob(mine) && c.hasJob(other) })

	pulses, unsubscribe := r.caches.Subscribe(mine)
	defer unsubscribe()

	f.failRecord(other, ReasonNoSamples)
	r.waitCache("the other record failed", func(c *Caches) bool {
		return c.jobEntries()[jobKey(other)].State == StateFailed
	})
	expectNoPulse(t, "another record's entry", pulses)

	f.failRecord(mine, ReasonNoSamples)
	expectPulse(t, "the subscribed record's entry", pulses)

	f.deleteKey(f.jobs, jobKey(mine))
	expectPulse(t, "the subscribed record's deletion", pulses)
}

// TestSubscribeNeverBlocksAnApply proves a subscriber nobody is reading costs
// the cache nothing: the send is dropped rather than held, so the entry is
// applied and every other subscriber is woken.
func TestSubscribeNeverBlocksAnApply(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	id := f.seedRecord("payment", "payment-api", StatePending)
	r.waitCache("the record", func(c *Caches) bool { return c.hasJob(id) })

	_, unsubscribeIdle := r.caches.Subscribe(id)
	defer unsubscribeIdle()
	pulses, unsubscribe := r.caches.Subscribe(id)
	defer unsubscribe()

	// The idle subscriber's buffer fills on the first entry and stays full.
	for _, state := range []State{StateRunning, StateFailed} {
		rec := f.record(id)
		rec.State = state
		f.putJSON(f.jobs, jobKey(id), rec)
		expectPulse(t, "an entry beside an idle subscriber", pulses)
	}
	r.waitCache("the record applied", func(c *Caches) bool {
		return c.jobEntries()[jobKey(id)].State == StateFailed
	})
}

// TestSubscribePulseIsCoalesced proves a pulse is a hint:
// a buffer that is full drops the entry's wake-up rather than blocking apply,
// and the next entry wakes the subscriber again.
func TestSubscribePulseIsCoalesced(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	id := f.seedRecord("payment", "payment-api", StatePending)
	r.waitCache("the record", func(c *Caches) bool { return c.hasJob(id) })

	pulses, unsubscribe := r.caches.Subscribe(id)
	defer unsubscribe()

	for _, state := range []State{StateRunning, StateCompleted} {
		rec := f.record(id)
		rec.State = state
		f.putJSON(f.jobs, jobKey(id), rec)
	}
	r.waitCache("both entries applied", func(c *Caches) bool {
		return c.jobEntries()[jobKey(id)].State == StateCompleted
	})

	expectPulse(t, "the first of two entries", pulses)
	expectNoPulse(t, "the coalesced second entry", pulses)

	f.deleteKey(f.jobs, jobKey(id))
	expectPulse(t, "the entry after the coalesced one", pulses)
}

// TestUnsubscribeLeavesNothingBehind proves the registration ends with the
// request: no further pulse reaches it and the caches hold no entry for it.
func TestUnsubscribeLeavesNothingBehind(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()

	id := f.seedRecord("payment", "payment-api", StatePending)
	r.waitCache("the record", func(c *Caches) bool { return c.hasJob(id) })

	pulses, unsubscribe := r.caches.Subscribe(id)
	if got := subscriberCount(r.caches, id); got != 1 {
		t.Fatalf("subscribers for the record = %d, want 1", got)
	}
	unsubscribe()
	if got := subscriberCount(r.caches, id); got != 0 {
		t.Errorf("subscribers after the request ended = %d, want 0", got)
	}

	f.failRecord(id, ReasonNoSamples)
	r.waitCache("the record failed", func(c *Caches) bool {
		return c.jobEntries()[jobKey(id)].State == StateFailed
	})
	expectNoPulse(t, "an entry after the registration was removed", pulses)

	r.caches.mu.Lock()
	left := len(r.caches.subscribers)
	r.caches.mu.Unlock()
	if left != 0 {
		t.Errorf("subscriber map entries = %d, want 0: an empty registration is removed with its record", left)
	}
}

// expectChannelClosed fails unless the channel is closed.
func expectChannelClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	default:
		t.Errorf("%s is still open", what)
	}
}

// expectChannelOpen fails when the channel is closed.
func expectChannelOpen(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
		t.Errorf("%s is closed", what)
	default:
	}
}

// TestGenerationBroadcastReachesEverySessionOfThatGeneration proves the broadcast ends a parked request:
// every session taken under the generation being left behind sees its channel close,
// and a session taken after the move holds the channel of the generation that followed,
// which the move it came after does not close.
//
// The channel a session holds is the one it captured,
// not the one current when it is asked for:
// a generation that moves between a request's read and its select hands back the replacement,
// and the signal is lost,
// leaving the request parked to its deadline over an outage.
func TestGenerationBroadcastReachesEverySessionOfThatGeneration(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("a", replicaOpts{})
	r.waitSynced()
	rt := r.newRuntime()

	one := r.session(rt)
	two := r.session(rt)
	expectChannelOpen(t, "a session's channel before any move", one.GenerationMoved())
	expectChannelOpen(t, "a second session's channel before any move", two.GenerationMoved())

	rt.MoveGeneration()

	expectChannelClosed(t, "the first session's channel after the move", one.GenerationMoved())
	expectChannelClosed(t, "the second session's channel after the move", two.GenerationMoved())

	after := r.session(rt)
	expectChannelOpen(t, "a session taken after the move", after.GenerationMoved())
	expectChannelClosed(t, "the first session's channel once a later session exists", one.GenerationMoved())

	rt.MoveGeneration()
	expectChannelClosed(t, "the later session's channel after the second move", after.GenerationMoved())
}
