package pgo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/natskv"
)

// publishOne runs one publication for a Service through r's publisher.
func (r *replica) publishOne(
	t *testing.T, jobs natskv.KV, ns, svc string, mutate ...func(*PublishInput),
) (string, Outcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()

	res, err := r.pub.Reserve(r.client.Generation(), ns, svc)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errCeilingRefused, err)
	}
	in := PublishInput{
		Namespace: ns, Service: svc,
		Origin:    OriginAPI,
		ClaimBy:   r.clock.Now().Add(time.Hour),
		Policy:    schedulerDefaults(t),
		CreatedBy: "tester",
	}
	for _, m := range mutate {
		m(&in)
	}

	return r.pub.Publish(ctx, jobs, res, in)
}

// keyed makes a publication carry an Idempotency-Key for one principal,
// which is the only difference between a request that binds a key and one that does not.
func keyed(principal, key string) func(*PublishInput) {
	return func(in *PublishInput) {
		in.CreatedBy = principal
		in.IdempotencyKey = key
	}
}

// errCeilingRefused reports that Reserve refused before any write.
var errCeilingRefused = errors.New("the reservation was refused")

// TestPublishWriteOrder pins the order that closes the publication race: the
// record exists as initializing before the active key names it, and only then
// does the record become claimable.
func TestPublishWriteOrder(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()

	hook := &kvHook{}
	id, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook}, "payment", "payment-api")
	if err != nil || outcome != OutcomeWon {
		t.Fatalf("publish returned (%q, %v), want a won publication", outcome, err)
	}

	want := []kvCall{
		{Op: "create", Key: jobKey(id)},
		{Op: "create", Key: activeKey("payment", "payment-api")},
		{Op: "update", Key: jobKey(id)},
	}
	if got := hook.operations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("publication issued %v, want %v", got, want)
	}

	var rec Record
	f.getJSON(f.jobs, jobKey(id), &rec)
	if rec.State != StatePending {
		t.Errorf("record state is %q, want %q", rec.State, StatePending)
	}
	if rec.Origin != OriginAPI || rec.Slot != "" {
		t.Errorf("record is %q with slot %q, want an api record with no slot", rec.Origin, rec.Slot)
	}
	var active activeValue
	f.getJSON(f.jobs, activeKey("payment", "payment-api"), &active)
	if active.ID != id {
		t.Errorf("active key names %q, want %q", active.ID, id)
	}

	// The reservation is given back only once a cache has delivered the
	// publication, never by the write returning.
	if got := r.pub.Reserved(); got != 1 {
		t.Fatalf("reservations held right after the publication are %d, want 1", got)
	}
	r.waitCache("holds the new record", func(c *Caches) bool { return c.hasJob(id) })
	r.releaseResolved()
	if got := r.pub.Reserved(); got != 0 {
		t.Fatalf("reservations held after the cache delivered are %d, want 0", got)
	}
}

// TestPublishLostActiveCreate proves a creator that loses the active key
// deletes its own initializing record, so the bucket keeps exactly the one
// Collection the Service already had.
func TestPublishLostActiveCreate(t *testing.T) {
	f := startPGO(t)
	frozen := newFreezer(cacheJobs, cacheActive)
	r := f.newReplica("replica", replicaOpts{freezer: frozen})
	r.waitSynced()
	frozen.freeze()

	liveID := f.seedLiveCollection("payment", "payment-api", StateRunning)
	_, outcome, err := r.publishOne(t, r.jobsView(), "payment", "payment-api")
	if err != nil || outcome != OutcomeBusy {
		t.Fatalf("publish returned (%q, %v), want a busy publication", outcome, err)
	}

	keys := f.jobKeys()
	if len(keys) != 1 || keys[0] != jobKey(liveID) {
		t.Fatalf("records are %v, want only %s", keys, jobKey(liveID))
	}
	if got := r.pub.Reserved(); got != 0 {
		t.Fatalf("reservations held are %d, want 0: the definite delete leaves nothing to count", got)
	}
}

// TestReserveCeiling proves the ceiling is measured over the Services the
// caches show as live plus this replica's publications they have not delivered.
func TestReserveCeiling(t *testing.T) {
	f := startPGO(t)
	frozen := newFreezer(cacheJobs, cacheActive)
	r := f.newReplica("replica", replicaOpts{
		limits:  limitsWith(func(l *config.PGOLimits) { l.MaxLiveCollections = 2 }),
		freezer: frozen,
	})
	r.waitSynced()

	first, err := r.reserve("payment", "a")
	if err != nil {
		t.Fatalf("the first reservation against an empty cache was refused: %v", err)
	}
	if _, err := r.reserve("payment", "b"); err != nil {
		t.Fatalf("the second reservation below the ceiling was refused: %v", err)
	}
	if _, err := r.reserve("payment", "c"); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("a third reservation at a ceiling of two = %v, want ErrCapacityExhausted", err)
	}

	first.Release()
	third, err := r.reserve("payment", "c")
	if err != nil {
		t.Fatalf("a reservation after one was given back was refused: %v", err)
	}
	third.Release()

	// A Service the caches show as live counts against the same ceiling.
	f.seedLiveCollection("payment", "d", StateRunning)
	r.waitCache("sees the live service", func(c *Caches) bool {
		live, ok := c.cachedLive(r.client.Generation())

		return ok && live == 1
	})
	if _, err := r.reserve("payment", "e"); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("a reservation with one cached live collection and one held = %v, want ErrCapacityExhausted", err)
	}
	frozen.freeze()
}

// TestReleaseRule walks the two observations that give a reservation back, and
// the readings that leave it held.
func TestReleaseRule(t *testing.T) {
	t.Run("a cache delivering the record releases it", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		id, _, err := r.publishOne(t, r.jobsView(), "payment", "payment-api")
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d, want 0", got)
		}
	})

	t.Run("a cache delivering the active key releases it", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()

		id, _, err := r.publishOne(t, r.jobsView(), "payment", "payment-api")
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		r.waitCache("holds the active key", func(c *Caches) bool {
			cached, ok := c.activeID("payment", "payment-api")

			return ok && cached == id
		})
		if r.caches.hasJob(id) {
			t.Fatal("the job cache was not held")
		}
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d, want 0", got)
		}
	})

	t.Run("fresh reads finding nothing release it", func(t *testing.T) {
		f := startPGO(t)
		// The reservation is taken behind the replay barrier, which is where every caller takes one,
		// and the freeze that follows is what leaves the caches showing nothing of the publication.
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()

		res, err := r.reserve("payment", "payment-api")
		if err != nil {
			t.Fatalf("the reservation was refused: %v", err)
		}
		frozen.freeze()
		res.Track(newID())
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d, want 0: nothing of the publication exists", got)
		}
	})

	t.Run("a terminal record and another id release it", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()

		res, err := r.reserve("payment", "payment-api")
		if err != nil {
			t.Fatalf("the reservation was refused: %v", err)
		}
		// Everything seeded from here is held at the gate, so the caches show none of it.
		frozen.freeze()
		mine := f.seedRecord("payment", "payment-api", StateFailed)
		f.seedLiveCollection("payment", "payment-api", StateRunning)
		res.Track(mine)
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d, want 0", got)
		}
	})

	t.Run("a nonterminal record holds it", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()

		res, err := r.reserve("payment", "payment-api")
		if err != nil {
			t.Fatalf("the reservation was refused: %v", err)
		}
		frozen.freeze()
		mine := f.seedRecord("payment", "payment-api", StateInitializing)
		res.Track(mine)
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d, want 1: the record is still nonterminal", got)
		}
	})

	t.Run("an unavailable read holds it", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()

		hook := &kvHook{after: func(op, _ string, err error) error {
			if op == "get" {
				return natskv.ErrUnavailable
			}

			return err
		}}
		res, err := r.reserve("payment", "payment-api")
		if err != nil {
			t.Fatalf("the reservation was refused: %v", err)
		}
		frozen.freeze()
		res.Track(newID())

		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		r.pub.ReleaseResolved(ctx, &hookKV{KV: r.jobsView(), hook: hook})
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d, want 1: an unavailable read decides nothing", got)
		}
	})
}

// TestReservationSurvivesClaimBy proves nothing but the release rule gives a
// reservation back, and in particular not the passing of claimBy: a replica at
// its ceiling with a frozen cache publishes nothing for a second Service.
func TestReservationSurvivesClaimBy(t *testing.T) {
	f := startPGO(t)
	for _, svc := range []string{"s1", "s2"} {
		f.setOverride("payment", svc, enabledOverride(withEvery(time.Hour), withJitter(0)))
	}
	frozen := newFreezer(cacheJobs, cacheActive)
	r := f.newReplica("replica", replicaOpts{
		limits:  limitsWith(func(l *config.PGOLimits) { l.MaxLiveCollections = 1 }),
		freezer: frozen,
	})
	r.waitSynced()
	r.waitCache("holds both overrides", func(c *Caches) bool { return len(c.overrideSnapshot()) == 2 })
	frozen.freeze()

	r.tick()
	records := f.records()
	if len(records) != 1 {
		t.Fatalf("the first tick left %d records, want 1", len(records))
	}

	// The Collection is claimed and running, and its claimBy is now behind us.
	rec := records[0]
	rec.State = StateRunning
	rec.ClaimBy = slotBase
	f.putJSON(f.jobs, jobKey(rec.ID), rec)
	r.clock.Advance(2 * time.Hour)

	for range 3 {
		r.tick()
	}
	if got := len(f.keys(f.jobs, activePrefix)); got != 1 {
		t.Fatalf("active keys are %d, want 1: passing claimBy must not release the reservation", got)
	}
	if got := len(f.jobKeys()); got != 1 {
		t.Fatalf("records are %d, want 1", got)
	}
	if got := r.pub.Reserved(); got != 1 {
		t.Fatalf("reservations held are %d, want 1", got)
	}
}

// TestIndeterminateCreates walks every write whose result the creator cannot
// know: the reservation stays tracked, the creator never retries, and only the
// release rule resolves it.
func TestIndeterminateCreates(t *testing.T) {
	t.Run("a committed job create keeps the reservation until a cache delivers", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()

		hook := &kvHook{after: func(op, key string, err error) error {
			if op == "create" && strings.HasPrefix(key, jobPrefix) {
				return natskv.ErrUnavailable
			}

			return err
		}}
		id, _, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook}, "payment", "payment-api")
		if !errors.Is(err, natskv.ErrUnavailable) {
			t.Fatalf("publish returned %v, want ErrUnavailable", err)
		}
		if got := f.jobKeys(); len(got) != 1 {
			t.Fatalf("the committed create left %v, want one record", got)
		}
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d, want 1", got)
		}

		// Fresh reads find the record still initializing, so it stays held.
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d after a fresh read, want 1", got)
		}

		frozen.release()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d after the cache delivered, want 0", got)
		}
		if !r.live("payment", "payment-api") {
			t.Fatal("the delivered record does not make the service live")
		}
	})

	t.Run("an uncommitted job create releases on the fresh reads", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()

		hook := &kvHook{before: func(op, key string) (error, bool) {
			if op == "create" && strings.HasPrefix(key, jobPrefix) {
				return natskv.ErrUnavailable, true
			}

			return nil, false
		}}
		if _, _, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook}, "payment", "payment-api"); !errors.Is(err, natskv.ErrUnavailable) {
			t.Fatalf("publish returned %v, want ErrUnavailable", err)
		}
		if got := f.jobKeys(); len(got) != 0 {
			t.Fatalf("an uncommitted create left %v, want nothing", got)
		}
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d right after the write, want 1", got)
		}
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d, want 0: nothing committed", got)
		}
	})

	t.Run("a committed active create keeps the reservation", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()

		hook := &kvHook{after: func(op, key string, err error) error {
			if op == "create" && strings.HasPrefix(key, activePrefix) {
				return natskv.ErrUnavailable
			}

			return err
		}}
		id, _, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook}, "payment", "payment-api")
		if !errors.Is(err, natskv.ErrUnavailable) {
			t.Fatalf("publish returned %v, want ErrUnavailable", err)
		}

		var rec Record
		f.getJSON(f.jobs, jobKey(id), &rec)
		if rec.State != StateInitializing {
			t.Errorf("record state is %q, want %q: the pending update never ran", rec.State, StateInitializing)
		}
		var active activeValue
		f.getJSON(f.jobs, activeKey("payment", "payment-api"), &active)
		if active.ID != id {
			t.Errorf("active key names %q, want %q", active.ID, id)
		}
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d, want 1", got)
		}
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d after a fresh read, want 1", got)
		}
	})

	t.Run("a lost active create whose delete committed releases", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()
		liveID := f.seedLiveCollection("payment", "payment-api", StateRunning)

		hook := &kvHook{after: func(op, _ string, err error) error {
			if op == "delete" {
				return natskv.ErrUnavailable
			}

			return err
		}}
		_, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook}, "payment", "payment-api")
		if err != nil || outcome != OutcomeBusy {
			t.Fatalf("publish returned (%q, %v), want a busy publication", outcome, err)
		}
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d, want 1: the delete's result is unknown", got)
		}

		// The delete did commit, so the fresh reads find no record of this
		// publication and the active key naming another Collection.
		keys := f.jobKeys()
		if len(keys) != 1 || keys[0] != jobKey(liveID) {
			t.Fatalf("records are %v, want only the live one", keys)
		}
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d, want 0", got)
		}
	})

	t.Run("a lost active create whose delete did not commit holds", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()
		f.seedLiveCollection("payment", "payment-api", StateRunning)

		hook := &kvHook{before: func(op, _ string) (error, bool) {
			if op == "delete" {
				return natskv.ErrUnavailable, true
			}

			return nil, false
		}}
		id, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook}, "payment", "payment-api")
		if err != nil || outcome != OutcomeBusy {
			t.Fatalf("publish returned (%q, %v), want a busy publication", outcome, err)
		}
		if got := len(f.jobKeys()); got != 2 {
			t.Fatalf("records are %d, want 2: the creator's own record survived", got)
		}
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 1 {
			t.Fatalf("reservations held are %d, want 1 while the record is still initializing", got)
		}

		// The worker scan fails a stale initializing record; only then is
		// there nothing left of this publication to count.
		f.failRecord(id, ReasonNotPublished)
		r.releaseResolved()
		if got := r.pub.Reserved(); got != 0 {
			t.Fatalf("reservations held are %d after the scan failed the record, want 0", got)
		}
	})
}

// TestKilledCreatorLeftovers proves what a creator that died between its
// writes leaves, and that the Service runs again only once the scan has
// cleared it.
func TestKilledCreatorLeftovers(t *testing.T) {
	tests := []struct {
		name      string
		withKey   bool
		wantLive  bool
		wantAfter int
	}{
		{name: "killed after the initializing create", withKey: false, wantLive: true, wantAfter: 2},
		{name: "killed after the active create", withKey: true, wantLive: true, wantAfter: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
			var leftover string
			if tc.withKey {
				leftover = f.seedLiveCollection("payment", "payment-api", StateInitializing)
			} else {
				leftover = f.seedRecord("payment", "payment-api", StateInitializing)
			}

			r := f.newReplica("replacement", replicaOpts{})
			r.waitSynced()
			r.waitCache("sees the leftover", func(c *Caches) bool {
				return len(c.overrideSnapshot()) == 1 && r.live("payment", "payment-api") == tc.wantLive
			})

			r.tick()
			if got := len(f.jobKeys()); got != 1 {
				t.Fatalf("a leftover publication left %d records, want 1", got)
			}

			f.failRecord(leftover, ReasonNotPublished)
			r.waitCache("sees the service free again", func(*Caches) bool {
				return !r.live("payment", "payment-api")
			})
			r.clock.Advance(time.Hour)
			r.tick()
			if got := len(f.jobKeys()); got != tc.wantAfter {
				t.Fatalf("the next slot left %d records, want %d", got, tc.wantAfter)
			}
			if got := len(f.keys(f.jobs, activePrefix)); got != 1 {
				t.Fatalf("active keys are %d, want 1", got)
			}
		})
	}
}

// TestCeilingAcrossReplicas proves live Collections never exceed
// replicas × maxLiveCollections, whatever the caches have delivered.
func TestCeilingAcrossReplicas(t *testing.T) {
	const (
		services = 100
		maxLive  = 8
		ceiling  = 2 * maxLive
	)

	t.Run("ticking replicas stay under the ceiling", func(t *testing.T) {
		f := startPGO(t)
		for i := range services {
			f.setOverride("payment", fmt.Sprintf("svc-%03d", i), enabledOverride(withEvery(time.Hour), withJitter(0)))
		}
		limits := limitsWith(func(l *config.PGOLimits) { l.MaxLiveCollections = maxLive })
		one := f.newReplica("replica-one", replicaOpts{limits: limits})
		two := f.newReplica("replica-two", replicaOpts{limits: limits})
		for _, r := range []*replica{one, two} {
			r.waitSynced()
			r.waitCache("holds every override", func(c *Caches) bool { return len(c.overrideSnapshot()) == services })
		}

		for range 10 {
			one.tick()
			two.tick()
			if live := len(f.liveServices()); live > ceiling {
				t.Fatalf("%d live collections, want at most %d", live, ceiling)
			}
		}
		if live := len(f.liveServices()); live == 0 {
			t.Fatal("no collection was published at all")
		}
	})

	t.Run("frozen caches keep each replica to its own headroom", func(t *testing.T) {
		f := startPGO(t)
		for i := range services {
			f.setOverride("payment", fmt.Sprintf("svc-%03d", i), enabledOverride(withEvery(time.Hour), withJitter(0)))
		}
		limits := limitsWith(func(l *config.PGOLimits) { l.MaxLiveCollections = maxLive })
		oneFrozen := newFreezer(cacheJobs, cacheActive)
		twoFrozen := newFreezer(cacheJobs, cacheActive)
		one := f.newReplica("replica-one", replicaOpts{limits: limits, freezer: oneFrozen})
		two := f.newReplica("replica-two", replicaOpts{limits: limits, freezer: twoFrozen})
		for _, r := range []*replica{one, two} {
			r.waitSynced()
			r.waitCache("holds every override", func(c *Caches) bool { return len(c.overrideSnapshot()) == services })
		}
		oneFrozen.freeze()
		twoFrozen.freeze()

		// Scheduled passes and on-demand publications for distinct Services,
		// all released together while neither replica's caches move.
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, r := range []*replica{one, two} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r.tick()
			}()
			for i := range 5 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					//nolint:errcheck // a refused reservation is one of the outcomes under test
					_, _, _ = r.publishOne(t, r.jobsView(), "ondemand", fmt.Sprintf("%s-%d", r.name, i))
				}()
			}
		}
		close(start)
		wg.Wait()

		if live := len(f.liveServices()); live > ceiling {
			t.Fatalf("%d live collections with frozen caches, want at most %d", live, ceiling)
		}
		for _, r := range []*replica{one, two} {
			if got := r.pub.Reserved(); got > maxLive {
				t.Fatalf("%s holds %d reservations, want at most %d", r.name, got, maxLive)
			}
		}
	})
}

// TestNonterminalRecordsBounded proves the bound the release rule keeps:
// across ticks, indeterminate writes, and a replaced publisher, records that
// are not terminal never exceed 2 × replicas × maxLiveCollections.
func TestNonterminalRecordsBounded(t *testing.T) {
	const (
		replicas = 2
		maxLive  = 1
		bound    = 2 * replicas * maxLive
	)

	f := startPGO(t)
	for _, svc := range []string{"s1", "s2", "s3", "s4"} {
		f.setOverride("payment", svc, enabledOverride(withEvery(time.Hour), withJitter(0)))
	}
	limits := limitsWith(func(l *config.PGOLimits) { l.MaxLiveCollections = maxLive })

	// Every active create commits and loses its acknowledgement, which is the
	// worst case for the counter: the record and the key both exist and no
	// creator knows it.
	hook := &kvHook{after: func(op, key string, err error) error {
		if op == "create" && strings.HasPrefix(key, activePrefix) {
			return natskv.ErrUnavailable
		}

		return err
	}}
	frozen := []*cacheFreezer{newFreezer(cacheJobs, cacheActive), newFreezer(cacheJobs, cacheActive)}
	rs := make([]*replica, 0, replicas)
	for i := range replicas {
		r := f.newReplica(fmt.Sprintf("replica-%d", i), replicaOpts{
			limits:  limits,
			freezer: frozen[i],
			wrapClient: func(c natskv.Client) natskv.Client {
				return newHookClient(c, hook)
			},
		})
		r.waitSynced()
		r.waitCache("holds every override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 4 })
		rs = append(rs, r)
	}
	for _, fz := range frozen {
		fz.freeze()
	}

	for round := range 5 {
		for _, r := range rs {
			r.tick()
			r.clock.Advance(time.Hour)
		}
		if got := len(f.nonterminalRecords()); got > bound {
			t.Fatalf("round %d left %d nonterminal records, want at most %d", round, got, bound)
		}
	}

	// A replacement publisher, as a restarted gateway's would be, stays behind
	// the barrier until its own watches have replayed what its predecessor left.
	replacementFrozen := newArmedFreezer(cacheJobs, cacheActive, cacheOverrides, cacheSlots)
	replacement := f.newReplica("replacement", replicaOpts{limits: limits, freezer: replacementFrozen})
	before := len(f.nonterminalRecords())
	for range 3 {
		replacement.tick()
	}
	if got := len(f.nonterminalRecords()); got != before {
		t.Fatalf("a replacement behind the barrier changed the record count from %d to %d", before, got)
	}
	if got := len(f.nonterminalRecords()); got > bound {
		t.Fatalf("%d nonterminal records, want at most %d", got, bound)
	}
}

// assertPublishedRecord holds every publication to what a record carries:
// the canonical hash of the policy it runs with, whatever created it,
// the idempotency key of the request that created it,
// and the principal the receipt is scoped to.
func assertPublishedRecord(t *testing.T, f *pgoFixture, id, wantKey, wantPrincipal string) Record {
	t.Helper()
	rec := f.record(id)
	if want := SnapshotHash(rec.Policy); rec.SnapshotHash != want {
		t.Errorf("record hash is %q, want %q over its own policy", rec.SnapshotHash, want)
	}
	if rec.SnapshotHash == "" {
		t.Error("the record carries no snapshot hash")
	}
	if rec.IdempotencyKey != wantKey {
		t.Errorf("record idempotencyKey is %q, want %q", rec.IdempotencyKey, wantKey)
	}
	if rec.CreatedBy != wantPrincipal {
		t.Errorf("record createdBy is %q, want %q", rec.CreatedBy, wantPrincipal)
	}

	return rec
}

// TestPublishReceiptOrder proves the write that releases a keyed publication:
// the receipt lands after the active key is won and before the record becomes claimable,
// so a Collection a caller can poll is one whose key is durable.
func TestPublishReceiptOrder(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()

	const principal, key = "tester", "k-1"
	name := ReceiptKey(principal, "payment", "payment-api", key)
	hook := &kvHook{}
	jobs := &hookKV{KV: r.jobsView(), hook: hook}

	reached := make(chan struct{})
	release := make(chan struct{})
	hook.setBefore(func(op, k string) (error, bool) {
		if op == "create" && k == name {
			close(reached)
			<-release
		}

		return nil, false
	})

	type published struct {
		id      string
		outcome Outcome
		err     error
	}
	done := make(chan published, 1)
	go func() {
		id, outcome, err := r.publishOne(t, jobs, "payment", "payment-api", keyed(principal, key))
		done <- published{id, outcome, err}
	}()

	select {
	case <-reached:
	case got := <-done:
		t.Fatalf("the publication finished as (%q, %v) without creating a receipt", got.outcome, got.err)
	}
	// At the barrier the active key is won and the record exists,
	// but it is not claimable and no key resolves to it.
	if f.hasKey(f.jobs, name) {
		t.Error("the receipt is in the bucket before its own create ran")
	}
	held := f.records()
	if len(held) != 1 || held[0].State != StateInitializing {
		t.Fatalf("the bucket holds %v, want one initializing record", held)
	}
	if !f.hasKey(f.jobs, activeKey("payment", "payment-api")) {
		t.Error("the active key is not won before the receipt is written")
	}
	close(release)

	got := <-done
	if got.err != nil || got.outcome != OutcomeWon {
		t.Fatalf("publish returned (%q, %v), want a won publication", got.outcome, got.err)
	}

	want := []kvCall{
		{Op: "create", Key: jobKey(got.id)},
		{Op: "create", Key: activeKey("payment", "payment-api")},
		{Op: "create", Key: name},
		{Op: "update", Key: jobKey(got.id)},
	}
	if ops := hook.operations(); !reflect.DeepEqual(ops, want) {
		t.Fatalf("the publication issued %v, want %v", ops, want)
	}

	rec := assertPublishedRecord(t, f, got.id, key, principal)
	if rec.State != StatePending {
		t.Errorf("record state is %q, want %q", rec.State, StatePending)
	}
	receipt := f.receipt(name)
	if receipt.ID != got.id || receipt.SnapshotHash != rec.SnapshotHash {
		t.Errorf("receipt is %+v, want the record's id and hash", receipt)
	}
	if receipt.CreatedAt.IsZero() {
		t.Error("the receipt carries no createdAt, which the reconciliation reads")
	}
}

// TestPublishReceiptRecovery covers what a receipt Create can meet,
// and what each answer costs, up to the withdraw that leaves nothing behind.
func TestPublishReceiptRecovery(t *testing.T) {
	const principal, key = "tester", "k-1"

	t.Run("an unavailable create is retried once", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		name := ReceiptKey(principal, "payment", "payment-api", key)

		var creates atomic.Int32
		hook := &kvHook{}
		hook.setBefore(func(op, k string) (error, bool) {
			if op == "create" && k == name && creates.Add(1) == 1 {
				return natskv.ErrUnavailable, true
			}

			return nil, false
		})

		id, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook},
			"payment", "payment-api", keyed(principal, key))
		if err != nil || outcome != OutcomeWon {
			t.Fatalf("publish returned (%q, %v), want the retry to complete it", outcome, err)
		}
		if got := len(hook.callsFor("create", name)); got != 2 {
			t.Fatalf("the receipt was created %d times, want one retry behind the same generation", got)
		}
		if got := f.receipt(name).ID; got != id {
			t.Fatalf("the receipt names %q, want %q", got, id)
		}
		assertPublishedRecord(t, f, id, key, principal)
	})

	t.Run("a create that fails twice withdraws", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		name := ReceiptKey(principal, "payment", "payment-api", key)

		hook := &kvHook{}
		hook.setBefore(func(op, k string) (error, bool) {
			if op == "create" && k == name {
				return natskv.ErrUnavailable, true
			}

			return nil, false
		})

		_, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook},
			"payment", "payment-api", keyed(principal, key))
		if !errors.Is(err, natskv.ErrUnavailable) {
			t.Fatalf("publish returned (%q, %v), want an unavailable outcome", outcome, err)
		}
		if got := f.jobKeys(); len(got) != 0 {
			t.Fatalf("the withdraw left %v, want no record", got)
		}
		if got := f.keys(f.jobs, activePrefix); len(got) != 0 {
			t.Fatalf("the withdraw left %v, want no active key", got)
		}
		if got := f.receiptKeys(); len(got) != 0 {
			t.Fatalf("the withdraw left %v, want no receipt", got)
		}
	})

	t.Run("a receipt already naming this collection is the earlier attempt", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		name := ReceiptKey(principal, "payment", "payment-api", key)

		// The first create commits and loses its acknowledgement,
		// so the retry meets the receipt this very publication wrote.
		var creates atomic.Int32
		hook := &kvHook{}
		hook.setAfter(func(op, k string, err error) error {
			if op == "create" && k == name && creates.Add(1) == 1 {
				return natskv.ErrUnavailable
			}

			return err
		})

		id, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook},
			"payment", "payment-api", keyed(principal, key))
		if err != nil || outcome != OutcomeWon {
			t.Fatalf("publish returned (%q, %v), want a won publication", outcome, err)
		}
		if got := len(hook.callsFor("create", name)); got != 2 {
			t.Fatalf("the receipt was created %d times, want the retry and no write beyond it", got)
		}
		if got := len(hook.callsFor("delete", name)); got != 0 {
			t.Fatalf("the publication deleted its own receipt %d times, want 0", got)
		}
		if got := f.receipt(name).ID; got != id {
			t.Fatalf("the receipt names %q, want %q", got, id)
		}
	})

	t.Run("a receipt whose record is absent is stale and is replaced", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()

		// What a sweep that deleted a record without reaching its receipt leaves:
		// a receipt naming a Collection that is gone.
		stale := newID()
		name := f.seedReceipt(principal, "payment", "payment-api", key, Receipt{
			ID: stale, SnapshotHash: SnapshotHash(schedulerDefaults(t)), CreatedAt: slotBase,
		})

		hook := &kvHook{}
		id, outcome, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook},
			"payment", "payment-api", keyed(principal, key))
		if err != nil || outcome != OutcomeWon {
			t.Fatalf("publish returned (%q, %v), want a won publication", outcome, err)
		}
		if got := f.receipt(name).ID; got != id {
			t.Fatalf("the receipt names %q, want the new collection %q", got, id)
		}

		want := []kvCall{
			{Op: "create", Key: name},
			{Op: "get", Key: name},
			{Op: "get", Key: jobKey(stale)},
			{Op: "delete", Key: name},
			{Op: "create", Key: name},
		}
		var got []kvCall
		for _, c := range hook.operations() {
			if c.Key == name || c.Key == jobKey(stale) {
				got = append(got, c)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the receipt step issued %v, want %v", got, want)
		}
	})
}

// TestPublishWithoutAReceipt covers the publications that write none:
// a loser, and the scheduler, whose records still carry a snapshot hash.
func TestPublishWithoutAReceipt(t *testing.T) {
	const principal, key = "tester", "k-1"

	t.Run("a keyed publication whose active create loses writes no receipt", func(t *testing.T) {
		f := startPGO(t)
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		frozen.freeze()
		f.seedLiveCollection("payment", "payment-api", StateRunning)

		_, outcome, err := r.publishOne(t, r.jobsView(), "payment", "payment-api", keyed(principal, key))
		if err != nil || outcome != OutcomeBusy {
			t.Fatalf("publish returned (%q, %v), want a busy publication", outcome, err)
		}
		if got := f.receiptKeys(); len(got) != 0 {
			t.Fatalf("a loser wrote %v, want no receipt", got)
		}
	})

	t.Run("a scheduled publication writes no receipt and still carries a hash", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()

		id, outcome, err := r.publishOne(t, r.jobsView(), "payment", "payment-api", func(in *PublishInput) {
			in.Origin = OriginSchedule
			in.Slot = slotBase
			in.CreatedBy = createdBySchedule
		})
		if err != nil || outcome != OutcomeWon {
			t.Fatalf("publish returned (%q, %v), want a won publication", outcome, err)
		}
		if got := f.receiptKeys(); len(got) != 0 {
			t.Fatalf("the scheduler wrote %v, want no receipt", got)
		}
		rec := assertPublishedRecord(t, f, id, "", createdBySchedule)
		if want := SnapshotHash(schedulerDefaults(t)); rec.SnapshotHash != want {
			t.Fatalf("record hash is %q, want %q over the policy it runs with", rec.SnapshotHash, want)
		}
	})
}

// TestKeyedCreatorDied proves what the two windows of a keyed publication leave behind,
// and that neither ever ran:
// a record no key resolves, and a receipt naming a record that never became claimable.
// Both end the same way, in a record the scan fails not_published.
func TestKeyedCreatorDied(t *testing.T) {
	const principal, key = "tester", "k-1"

	t.Run("killed after its record and before its receipt", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedLiveCollection("payment", "payment-api", StateInitializing, func(rec *Record) {
			rec.Origin = OriginAPI
			rec.CreatedBy = principal
			rec.IdempotencyKey = key
		})
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		if got := f.receiptKeys(); len(got) != 0 {
			t.Fatalf("the bucket holds %v, want no key resolving to this record", got)
		}
		if f.hasKey(f.jobs, ReceiptKey(principal, "payment", "payment-api", key)) {
			t.Fatal("the record's own key resolves to a receipt it never wrote")
		}

		r.clock.Set(slotBase.Add(publishGrace + skewMargin + time.Second))
		scanNow(t, r.newWorker(trapRun(t)))
		if rec := f.record(id); rec.State != StateFailed || rec.Reason != ReasonNotPublished {
			t.Fatalf("record is %q %q, want failed %q", rec.State, rec.Reason, ReasonNotPublished)
		}
	})

	t.Run("killed after its receipt and before the pending update", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		name := ReceiptKey(principal, "payment", "payment-api", key)

		hook := &kvHook{}
		hook.setBefore(func(op, k string) (error, bool) {
			if op == "update" && strings.HasPrefix(k, jobPrefix) {
				return natskv.ErrUnavailable, true
			}

			return nil, false
		})

		id, _, err := r.publishOne(t, &hookKV{KV: r.jobsView(), hook: hook},
			"payment", "payment-api", keyed(principal, key))
		if !errors.Is(err, natskv.ErrUnavailable) {
			t.Fatalf("publish returned %v, want ErrUnavailable", err)
		}
		if got := f.receipt(name).ID; got != id {
			t.Fatalf("the receipt names %q, want %q: it landed before the update", got, id)
		}
		if got := f.record(id).State; got != StateInitializing {
			t.Fatalf("record state is %q, want %q: the update never ran", got, StateInitializing)
		}

		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		r.clock.Set(slotBase.Add(publishGrace + skewMargin + time.Second))
		scanNow(t, r.newWorker(trapRun(t)))
		if rec := f.record(id); rec.State != StateFailed || rec.Reason != ReasonNotPublished {
			t.Fatalf("record is %q %q, want failed %q", rec.State, rec.Reason, ReasonNotPublished)
		}
	})
}
