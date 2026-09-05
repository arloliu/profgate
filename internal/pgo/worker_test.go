package pgo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/natskv"
)

// ipv4Pattern is what a Pod address would look like if one ever reached a log.
var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// waitClaimed blocks until the record is running with the given attempt.
func waitClaimed(t *testing.T, f *pgoFixture, id string, attempt int) Record {
	t.Helper()
	var rec Record
	waitFor(t, "the record is claimed", func() bool {
		rec = f.record(id)

		return rec.State == StateRunning && rec.Attempt == attempt
	})

	return rec
}

// TestWorkerTwoWorkersRaceOnePending proves the record's revision decides:
// one worker runs, the other's conditional update loses and it profiles
// nothing.
func TestWorkerTwoWorkersRaceOnePending(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")

	one := f.newReplica("replica-one", replicaOpts{})
	two := f.newReplica("replica-two", replicaOpts{})
	stubs := make([]*runStub, 2)
	workers := make([]*Worker, 2)
	for i, r := range []*replica{one, two} {
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		stubs[i] = newRunStub(workResult{Object: id + "-1.pprof"})
		workers[i] = r.newWorker(stubs[i].fn())
	}
	t.Cleanup(func() {
		for _, s := range stubs {
			s.release()
		}
	})

	scanNow(t, workers[0])
	scanNow(t, workers[1])

	// The loser never starts a work goroutine at all, so the count settles at
	// one and stays there.
	waitFor(t, "one worker entered the work body", func() bool {
		return stubs[0].calls()+stubs[1].calls() == 1
	})
	rec := f.record(id)
	if rec.State != StateRunning || rec.Attempt != 1 {
		t.Fatalf("record is %q attempt %d, want running attempt 1", rec.State, rec.Attempt)
	}
	slots := workers[0].activeSlots() + workers[1].activeSlots()
	if slots != 1 {
		t.Fatalf("%d local slots are taken, want 1: the loser must release its own", slots)
	}
}

// TestWorkerScanReclaimsLapsedLease proves the scan exists because time
// passing writes no KV revision: after an owner dies the last thing any watch
// delivered was a valid lease, and nothing else would revisit the record.
func TestWorkerScanReclaimsLapsedLease(t *testing.T) {
	f := startPGO(t)
	lease := slotBase.Add(30 * time.Second)
	started := slotBase
	deadline := slotBase.Add(time.Hour)
	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		rec.State = StateRunning
		rec.Attempt = 1
		rec.Owner = &Owner{Instance: "dead-replica", Pod: "dead-replica"}
		rec.LeaseUntil = &lease
		rec.StartedAt = &started
		rec.Deadline = &deadline
	})

	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
	stub := newRunStub(workResult{})
	t.Cleanup(stub.release)
	w := r.newWorker(stub.fn())

	// One second before the lease plus the margin: still someone else's.
	r.clock.Set(lease.Add(skewMargin - time.Second))
	scanNow(t, w)
	if got := f.record(id); got.Attempt != 1 {
		t.Fatalf("a valid lease was reclaimed at attempt %d", got.Attempt)
	}

	r.clock.Set(lease.Add(skewMargin + time.Second))
	scanNow(t, w)
	rec := f.record(id)
	if rec.Attempt != 2 || rec.State != StateRunning {
		t.Fatalf("record is %q attempt %d, want running attempt 2", rec.State, rec.Attempt)
	}
	if rec.Owner == nil || rec.Owner.Instance != "replica" {
		t.Fatalf("owner is %+v, want the reclaiming replica", rec.Owner)
	}
	if !rec.StartedAt.Equal(started) || !rec.Deadline.Equal(deadline) {
		t.Fatalf("a reclaim moved startedAt or the deadline: %s, %s", rec.StartedAt, rec.Deadline)
	}
}

// TestWorkerFastClaimerKeepsOff proves a claimer whose clock runs skewMargin
// ahead of the owner's does not reclaim a lease that is still valid there.
func TestWorkerFastClaimerKeepsOff(t *testing.T) {
	f := startPGO(t)
	lease := slotBase.Add(30 * time.Second)
	deadline := slotBase.Add(time.Hour)
	started := slotBase
	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		rec.State = StateRunning
		rec.Attempt = 1
		rec.Owner = &Owner{Instance: "owner", Pod: "owner"}
		rec.LeaseUntil = &lease
		rec.StartedAt = &started
		rec.Deadline = &deadline
	})

	r := f.newReplica("fast-replica", replicaOpts{clock: newFakeClock(lease.Add(skewMargin))})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
	w := r.newWorker(trapRun(t))
	scanNow(t, w)

	if got := f.record(id); got.Attempt != 1 || got.Owner.Instance != "owner" {
		t.Fatalf("record is attempt %d owned by %+v, want the original owner untouched", got.Attempt, got.Owner)
	}
}

// TestWorkerScanTerminations proves the three deadlines the scan enforces, and
// that each releases the Service.
func TestWorkerScanTerminations(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Record)
		at         time.Time
		notYet     time.Time
		wantReason string
	}{
		{
			name:       "an initializing record its creator never published",
			mutate:     func(rec *Record) { rec.State = StateInitializing },
			notYet:     slotBase.Add(publishGrace + skewMargin),
			at:         slotBase.Add(publishGrace + skewMargin + time.Second),
			wantReason: ReasonNotPublished,
		},
		{
			// A pending record inside its claimBy is claimable, not idle, so
			// there is no earlier moment to check: only the deadline itself.
			name:       "a pending record nobody claimed",
			mutate:     func(rec *Record) { rec.ClaimBy = slotBase.Add(time.Hour) },
			at:         slotBase.Add(time.Hour + skewMargin + time.Second),
			wantReason: ReasonNotClaimed,
		},
		{
			name: "a running record past its deadline",
			mutate: func(rec *Record) {
				lease := slotBase.Add(time.Hour)
				started := slotBase
				deadline := slotBase.Add(30 * time.Minute)
				rec.State = StateRunning
				rec.Attempt = 1
				rec.Owner = &Owner{Instance: "wedged", Pod: "wedged"}
				rec.LeaseUntil = &lease
				rec.StartedAt = &started
				rec.Deadline = &deadline
			},
			notYet:     slotBase.Add(30*time.Minute + skewMargin),
			at:         slotBase.Add(30*time.Minute + skewMargin + time.Second),
			wantReason: ReasonDeadlineExceeded,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			id := f.seedClaimable("payment", "payment-api", tc.mutate)
			r := f.newReplica("replica", replicaOpts{})
			r.waitSynced()
			r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
			w := r.newWorker(trapRun(t))

			if !tc.notYet.IsZero() {
				r.clock.Set(tc.notYet)
				scanNow(t, w)
				if got := f.record(id); got.State == StateFailed {
					t.Fatalf("the record was failed %q before its margin had passed", got.Reason)
				}
			}

			r.clock.Set(tc.at)
			scanNow(t, w)
			rec := f.record(id)
			if rec.State != StateFailed || rec.Reason != tc.wantReason {
				t.Fatalf("record is %q %q, want failed %q", rec.State, rec.Reason, tc.wantReason)
			}
			if rec.FinishedAt == nil {
				t.Error("a terminal record has no finishedAt")
			}
			if keys := f.keys(f.jobs, activePrefix); len(keys) != 0 {
				t.Errorf("active keys are %v, want the service released", keys)
			}
			if got := r.recorder.collectionRows(); !reflect.DeepEqual(got, map[string]int{string(StateFailed): 1}) {
				t.Errorf("collection rows are %v, want one failed", got)
			}
			if got := r.recorder.peakActive(); got != 0 {
				t.Errorf("the active gauge reached %d, want 0: nothing was claimed", got)
			}
		})
	}
}

// TestWorkerLimitExceeded proves a snapshot that exceeds this replica's
// ceilings is failed before any local capacity is reserved, so such a record
// never takes a slot from one this replica can actually run.
func TestWorkerLimitExceeded(t *testing.T) {
	f := startPGO(t)
	// Created under maxParallel 8, claimed by a replica whose ceiling is 4.
	over := f.seedClaimable("payment", "over-the-ceiling", func(rec *Record) {
		rec.Policy.Sampling.MaxParallel = 8
	})
	fine := f.seedClaimable("payment", "within-the-ceiling")

	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds both records", func(c *Caches) bool { return c.hasJob(over) && c.hasJob(fine) })

	stub := newRunStub(workResult{})
	t.Cleanup(stub.release)
	// One local slot, so a leaked reservation would starve the valid record.
	w := r.newWorker(stub.fn(), func(cfg *config.PGOConfig) {
		cfg.Limits = limitsWith(func(l *config.PGOLimits) { l.MaxActiveCollections = 1 })
	})
	scanNow(t, w)

	rec := f.record(over)
	if rec.State != StateFailed || rec.Reason != ReasonLimitExceeded {
		t.Fatalf("the over-ceiling record is %q %q, want failed %q", rec.State, rec.Reason, ReasonLimitExceeded)
	}
	if _, err := f.jobs.Get(context.Background(), activeKey("payment", "over-the-ceiling")); err == nil {
		t.Error("the over-ceiling service was not released")
	}
	if got := stub.waitStarted(t).Record.ID; got != fine {
		t.Fatalf("the work body ran for %s, want the record within the ceilings", got)
	}
	if stub.calls() != 1 {
		t.Fatalf("the work body ran %d times, want once: only the valid record is profiled", stub.calls())
	}
	if got := r.recorder.peakActive(); got != 1 {
		t.Fatalf("the active gauge peaked at %d, want 1", got)
	}
}

// TestWorkerRetentionUnderInterval proves the claim measures a snapshot against the rule that judges a policy by itself,
// as well as against the ceilings:
// a Collection that would keep its artifact for less than its own interval
// is failed before any local capacity is reserved.
// The record is written straight into the store,
// so a rule enforced only where a policy is written would let it reach the work body.
func TestWorkerRetentionUnderInterval(t *testing.T) {
	f := startPGO(t)
	under := f.seedClaimable("payment", "retains-under-an-interval", func(rec *Record) {
		rec.Policy.Schedule.Every = Duration(6 * time.Hour)
		rec.Policy.Artifact.Retention = Duration(time.Hour)
	})
	fine := f.seedClaimable("payment", "retains-a-whole-interval")

	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds both records", func(c *Caches) bool { return c.hasJob(under) && c.hasJob(fine) })

	stub := newRunStub(workResult{})
	t.Cleanup(stub.release)
	// One local slot, so a leaked reservation would starve the coherent record.
	w := r.newWorker(stub.fn(), func(cfg *config.PGOConfig) {
		cfg.Limits = limitsWith(func(l *config.PGOLimits) { l.MaxActiveCollections = 1 })
	})
	scanNow(t, w)

	rec := f.record(under)
	if rec.State != StateFailed || rec.Reason != ReasonLimitExceeded {
		t.Fatalf("the incoherent record is %q %q, want failed %q", rec.State, rec.Reason, ReasonLimitExceeded)
	}
	if got := stub.waitStarted(t).Record.ID; got != fine {
		t.Fatalf("the work body ran for %s, want the coherent record", got)
	}
	if stub.calls() != 1 {
		t.Fatalf("the work body ran %d times, want once", stub.calls())
	}
}

// TestWorkerAttemptsExhausted proves the claim that would exceed maxAttempts
// fails the record instead, and gives its local slot straight back.
func TestWorkerAttemptsExhausted(t *testing.T) {
	f := startPGO(t)
	lease := slotBase
	started := slotBase
	deadline := slotBase.Add(time.Hour)
	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		rec.State = StateRunning
		rec.Attempt = 3
		rec.Owner = &Owner{Instance: "dead", Pod: "dead"}
		rec.LeaseUntil = &lease
		rec.StartedAt = &started
		rec.Deadline = &deadline
	})

	r := f.newReplica("replica", replicaOpts{clock: newFakeClock(lease.Add(2 * skewMargin))})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
	w := r.newWorker(trapRun(t))
	scanNow(t, w)

	rec := f.record(id)
	if rec.State != StateFailed || rec.Reason != ReasonAttemptsExhausted {
		t.Fatalf("record is %q %q, want failed %q", rec.State, rec.Reason, ReasonAttemptsExhausted)
	}
	if got := w.activeSlots(); got != 0 {
		t.Fatalf("%d local slots are taken, want 0", got)
	}
}

// TestWorkerClaimUnavailable proves an indeterminate claim profiles nothing:
// a worker that ran on it would either profile without owning the record or
// own it without the revision every later conditional write needs.
func TestWorkerClaimUnavailable(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")

	hook := &kvHook{after: func(op, key string, err error) error {
		if op == "update" && key == jobKey(id) {
			return natskv.ErrUnavailable
		}

		return err
	}}
	r := f.newReplica("replica", replicaOpts{
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
	w := r.newWorker(trapRun(t))
	scanNow(t, w)

	if got := w.activeSlots(); got != 0 {
		t.Fatalf("%d local slots are taken, want 0: an indeterminate claim frees its slot", got)
	}
	if got := r.recorder.peakActive(); got != 0 {
		t.Fatalf("the active gauge reached %d, want 0", got)
	}
	// The claim did commit, and the lease this replica will never renew is
	// reclaimed by the next scan after leaseTTL + skewMargin, as after a crash.
	rec := f.record(id)
	if rec.State != StateRunning || rec.Attempt != 1 {
		t.Fatalf("record is %q attempt %d, want the committed claim", rec.State, rec.Attempt)
	}
}

// TestWorkerBehindBarrier proves nothing on a replica decides from a cache
// that has not seen the bucket under the current store generation.
func TestWorkerBehindBarrier(t *testing.T) {
	t.Run("a held replay claims and scans nothing", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")

		hook := &kvHook{}
		held := newArmedFreezer(cacheOverrides, cacheJobs, cacheActive, cacheSlots)
		r := f.newReplica("replica", replicaOpts{
			freezer:    held,
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		stub := newRunStub(workResult{})
		t.Cleanup(stub.release)
		w := r.newWorker(stub.fn())

		for range 3 {
			scanNow(t, w)
		}
		if got := hook.operations(); len(got) != 0 {
			t.Fatalf("a scan behind the barrier issued %v, want no store operation", got)
		}
		if stub.calls() != 0 {
			t.Fatal("the work body ran behind the barrier")
		}

		held.release()
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		scanNow(t, w)
		stub.waitStarted(t)
		if got := f.record(id); got.State != StateRunning {
			t.Fatalf("record is %q, want running once the replay had landed", got.State)
		}
	})

	t.Run("a cache left over from before an outage issues no store operation", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")

		hook := &kvHook{}
		frozen := newFreezer(cacheOverrides, cacheJobs, cacheActive, cacheSlots)
		r := f.newReplica("replica", replicaOpts{
			freezer:    frozen,
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		// The connection drops and comes back while the caches are held, so
		// they still hold what they read under the old generation.
		before := r.client.Generation()
		f.stopServer()
		waitFor(t, "the disconnect moved the generation", func() bool { return r.client.Generation() != before })
		frozen.freeze()
		f.restartServer()

		// The generation is read inside the predicate: a restart can produce a
		// second disconnect, and a value captured before the wait would never
		// be current again.
		waitFor(t, "the seam re-opened its watches", func() bool {
			return r.client.Synced(r.client.Generation())
		})
		gen := r.client.Generation()
		if len(r.caches.nonterminalJobs()) == 0 {
			t.Fatal("the caches emptied themselves; the stale contents are what this case is about")
		}

		w := r.newWorker(trapRun(t))
		hook.reset()
		for range 3 {
			scanNow(t, w)
		}
		if !r.client.Synced(gen) {
			t.Fatal("the seam's own watches never re-synced, so the barrier was not the reason nothing happened")
		}
		if r.caches.Synced(gen) {
			t.Fatal("the caches reported synced while their replay was held")
		}
		if got := hook.operations(); len(got) != 0 {
			t.Fatalf("a scan behind the barrier issued %v, want no store operation", got)
		}
	})
}

// TestWorkerRunClaimsOnDeliveryAndOnItsTicker proves both entry points: the
// job cache delivering a record, and the scan the loop runs on its own timer.
func TestWorkerRunClaimsOnDeliveryAndOnItsTicker(t *testing.T) {
	t.Run("on delivery", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		stub := newRunStub(workResult{})
		t.Cleanup(stub.release)
		w := r.newWorker(stub.fn())

		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			w.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-stopped
		})

		id := f.seedClaimable("payment", "payment-api")
		stub.waitStarted(t)
		waitClaimed(t, f, id, 1)
	})

	t.Run("on the scan timer", func(t *testing.T) {
		f := startPGO(t)
		lease := slotBase
		started := slotBase
		deadline := slotBase.Add(time.Hour)
		id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
			rec.State = StateRunning
			rec.Attempt = 1
			rec.Owner = &Owner{Instance: "dead", Pod: "dead"}
			rec.LeaseUntil = &lease
			rec.StartedAt = &started
			rec.Deadline = &deadline
		})
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		stub := newRunStub(workResult{})
		t.Cleanup(stub.release)
		w := r.newWorker(stub.fn())

		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			w.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-stopped
		})

		// No KV write happens as the lease lapses; only the scan revisits it.
		waitFor(t, "the scan timer started", func() bool { return r.clock.tickerCount() > 0 })
		r.clock.Set(lease.Add(2*skewMargin + testLeaseTTL/2))
		waitFor(t, "the scan reclaimed the record", func() bool { return f.record(id).Attempt == 2 })
	})
}

// TestWorkerRenewalKeepsTheLease proves the owner loop renews on its timer and
// that a renewal commits only what the Update stored.
func TestWorkerRenewalKeepsTheLease(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	stub := newRunStub(workResult{Object: id + "-1.pprof", Bytes: 10})
	w := r.newWorker(stub.fn())
	scanNow(t, w)
	claimed := waitClaimed(t, f, id, 1)

	for range 3 {
		before := f.record(id)
		r.clock.Advance(testLeaseTTL / 3)
		waitFor(t, "a renewal moved the lease", func() bool {
			return f.record(id).LeaseUntil.After(*before.LeaseUntil)
		})
	}
	renewed := f.record(id)
	if !renewed.LeaseUntil.After(*claimed.LeaseUntil) {
		t.Fatalf("the lease never moved: %s", renewed.LeaseUntil)
	}

	// Renewal and finish are serialized by construction, so the final update
	// never loses to the owner's own newer revision.
	stub.release()
	waitFor(t, "the collection completed", func() bool { return f.record(id).State == StateCompleted })
	if got := f.record(id).Artifact; got == nil || got.Object != id+"-1.pprof" {
		t.Fatalf("the completed record names %+v, want the object the work stored", got)
	}
}

// TestWorkerUnavailableRenewalKeepsCommittedLease proves a failed renewal
// leaves the committed lease exactly where the last successful write put it,
// so the cutoff the owner enforces is always a lease another replica can see.
func TestWorkerUnavailableRenewalKeepsCommittedLease(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")

	hook := &kvHook{}
	r := f.newReplica("replica", replicaOpts{
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	stub := newRunStub(workResult{})
	t.Cleanup(stub.release)
	w := r.newWorker(stub.fn())
	scanNow(t, w)
	claimed := waitClaimed(t, f, id, 1)
	stub.waitStarted(t)

	// Every renewal from here on is unavailable.
	hook.setAfter(func(op, key string, err error) error {
		if op == "update" && key == jobKey(id) {
			return natskv.ErrUnavailable
		}

		return err
	})

	// Just short of the committed cutoff: no renewal succeeded, and the owner
	// has not given up either.
	r.clock.Set(claimed.LeaseUntil.Add(-skewMargin - time.Second))
	waitFor(t, "a renewal was attempted and failed", func() bool {
		return len(hook.callsFor("update", jobKey(id))) > 1
	})
	if stub.cancelled() {
		t.Fatal("the work was cancelled while the committed lease still had time")
	}

	// Past it, the owner aborts: a failed renewal never moved the cutoff.
	r.clock.Set(claimed.LeaseUntil.Add(-skewMargin + time.Second))
	stub.waitCancelled(t)
	stub.release()
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })
	if got := f.record(id).State; got != StateRunning {
		t.Fatalf("record is %q, want running: an owner that cannot prove its lease writes nothing", got)
	}
}

// TestWorkerBlockedRenewalAborts proves the cutoff does not wait for the
// renewal to come back: a renewal still in flight past the committed lease
// minus the margin has already had the work cancelled under it.
func TestWorkerBlockedRenewalAborts(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")

	var (
		hook    = &kvHook{}
		blocked = make(chan struct{}, 1)
		resume  = make(chan struct{})
		claimed = make(chan struct{})
		once    sync.Once
	)
	r := f.newReplica("replica", replicaOpts{
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	stub := newRunStub(workResult{})
	t.Cleanup(stub.release)
	w := r.newWorker(stub.fn())

	hook.setBefore(func(op, key string) (error, bool) {
		if op != "update" || key != jobKey(id) {
			return nil, false
		}
		select {
		case <-claimed:
		default:
			return nil, false // the claim itself
		}
		once.Do(func() {
			blocked <- struct{}{}
			<-resume
		})

		return nil, false
	})

	scanNow(t, w)
	rec := waitClaimed(t, f, id, 1)
	close(claimed)
	stub.waitStarted(t)

	// The first renewal blocks inside NATS while the committed lease runs out.
	r.clock.Advance(testLeaseTTL / 3)
	<-blocked
	r.clock.Set(rec.LeaseUntil.Add(-skewMargin))
	stub.waitCancelled(t)
	close(resume)

	stub.release()
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })
}

// TestWorkerRevisionMismatchCancelsImmediately proves a record that moved
// under its owner cancels the work at once, without waiting for the cutoff,
// and that the owner commits nothing afterwards.
func TestWorkerRevisionMismatchCancelsImmediately(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	stub := newRunStub(workResult{Object: id + "-1.pprof"})
	w := r.newWorker(stub.fn())
	scanNow(t, w)
	claimed := waitClaimed(t, f, id, 1)
	stub.waitStarted(t)

	// The cancel handler's conditional update advances the revision.
	cancelled := claimed
	cancelled.State = StateCancelled
	cancelled.Reason = "cancelled_by_api"
	finished := r.clock.Now()
	cancelled.FinishedAt = &finished
	f.putJSON(f.jobs, jobKey(id), cancelled)

	// One renewal tick, far short of the cutoff, is all it takes.
	r.clock.Advance(testLeaseTTL / 3)
	stub.waitCancelled(t)
	if now, cutoff := r.clock.Now(), claimed.LeaseUntil.Add(-skewMargin); !now.Before(cutoff) {
		t.Fatalf("the clock reached %s, past the cutoff %s: the cancel did not act at once", now, cutoff)
	}

	stub.release()
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })
	rec := f.record(id)
	if rec.State != StateCancelled {
		t.Fatalf("record is %q, want cancelled: the worker's lost final update writes nothing", rec.State)
	}
	if rec.Artifact != nil {
		t.Fatalf("a cancelled collection names artifact %+v, want none", rec.Artifact)
	}
	// The cancelled row belongs to the handler whose CAS won, not to the worker.
	if got := r.recorder.collectionRows()[string(StateCancelled)]; got != 0 {
		t.Fatalf("the worker recorded %d cancelled rows, want 0", got)
	}
	if got := r.recorder.activeGauge(); got != 0 {
		t.Fatalf("the active gauge is %d, want 0", got)
	}
}

// TestWorkerWorkHeldPastCutoff proves the owner issues no final update after
// the cutoff, with and without another replica reclaiming the record.
func TestWorkerWorkHeldPastCutoff(t *testing.T) {
	t.Run("a lapsed lease aborts before the work returns", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		object := id + "-1.pprof"
		stub := newRunStub(workResult{Object: object, Bytes: 4})
		w := r.newWorker(stub.fn())
		scanNow(t, w)
		claimed := waitClaimed(t, f, id, 1)
		stub.waitStarted(t)
		f.putObject(r, object)

		r.clock.Set(claimed.LeaseUntil.Add(time.Second))
		stub.waitCancelled(t)
		stub.release()
		waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

		if got := f.record(id).State; got != StateRunning {
			t.Fatalf("record is %q, want running: no final update may be issued past the cutoff", got)
		}
		f.waitObjectGone(r, object)
	})

	t.Run("a reclaimed collection leaves the stale owner nothing to commit", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")
		one := f.newReplica("replica-one", replicaOpts{})
		two := f.newReplica("replica-two", replicaOpts{})
		for _, r := range []*replica{one, two} {
			r.waitSynced()
			r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		}

		object := id + "-1.pprof"
		stale := newRunStub(workResult{Object: object, Bytes: 4})
		staleWorker := one.newWorker(stale.fn())
		scanNow(t, staleWorker)
		claimed := waitClaimed(t, f, id, 1)
		stale.waitStarted(t)
		f.putObject(one, object)

		// The stale owner's lease lapses and the other replica reclaims it.
		two.clock.Set(claimed.LeaseUntil.Add(2 * skewMargin))
		reclaimed := newRunStub(workResult{})
		t.Cleanup(reclaimed.release)
		scanNow(t, two.newWorker(reclaimed.fn()))
		waitClaimed(t, f, id, 2)

		one.clock.Set(claimed.LeaseUntil.Add(time.Second))
		stale.waitCancelled(t)
		stale.release()
		waitFor(t, "the stale owner loop stopped", func() bool { return staleWorker.activeSlots() == 0 })

		if got := f.record(id).Attempt; got != 2 {
			t.Fatalf("record is at attempt %d, want the reclaimed 2", got)
		}
		f.waitObjectGone(one, object)
	})
}

// TestWorkerReclaimedOwnerPastDeadline proves the final update is gated on the
// deadline as well as the lease: an owner whose Put took long enough for the
// deadline the first claimer wrote to pass has nothing to commit, and removes
// the object it stored.
func TestWorkerReclaimedOwnerPastDeadline(t *testing.T) {
	f := startPGO(t)
	lapsed := slotBase
	started := slotBase
	deadline := slotBase.Add(30 * time.Second)
	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		rec.State = StateRunning
		rec.Attempt = 1
		rec.Owner = &Owner{Instance: "dead", Pod: "dead"}
		rec.LeaseUntil = &lapsed
		rec.StartedAt = &started
		rec.Deadline = &deadline
	})

	// A clock skewMargin ahead of the first claimer's, which is what makes the
	// lapsed lease reclaimable here.
	r := f.newReplica("replica", replicaOpts{clock: newFakeClock(slotBase.Add(2 * skewMargin))})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	object := id + "-2.pprof"
	stub := newRunStub(workResult{Object: object, Bytes: 9})
	w := r.newWorker(stub.fn())
	scanNow(t, w)
	claimed := waitClaimed(t, f, id, 2)
	stub.waitStarted(t)
	if !claimed.Deadline.Equal(deadline) {
		t.Fatalf("the reclaim moved the deadline to %s, want the first claimer's %s", claimed.Deadline, deadline)
	}
	f.putObject(r, object)

	// Past deadline - skewMargin, with the lease still comfortably valid.
	r.clock.Set(deadline.Add(-skewMargin + time.Second))
	if !r.clock.Now().Before(claimed.LeaseUntil.Add(-skewMargin)) {
		t.Fatalf("the lease had also lapsed at %s; the deadline must be the only gate", r.clock.Now())
	}
	stub.release()
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

	if got := f.record(id); got.State != StateRunning || got.Artifact != nil {
		t.Fatalf("record is %q naming %+v, want running and naming nothing", got.State, got.Artifact)
	}
	f.waitObjectGone(r, object)
	if got := r.recorder.collectionRows(); len(got) != 0 {
		t.Fatalf("collection rows are %v, want none: nothing terminal was committed", got)
	}
}

// TestWorkerLostFinalUpdate proves the losing half of the artifact fence: an
// owner whose final update loses deletes the object it wrote, names nothing,
// and records no terminal row.
// The lease and the deadline are both still valid here, so the conditional
// update is the only thing that refuses it.
func TestWorkerLostFinalUpdate(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api")
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	object := id + "-1.pprof"
	stub := newRunStub(workResult{Object: object, Bytes: 5})
	w := r.newWorker(stub.fn())
	scanNow(t, w)
	claimed := waitClaimed(t, f, id, 1)
	stub.waitStarted(t)
	f.putObject(r, object)

	// The cancel handler's conditional update lands between the last renewal
	// and the final update, and the clock never moves, so no renewal tick
	// tells the owner about it first.
	cancelled := claimed
	cancelled.State = StateCancelled
	cancelled.Reason = "cancelled_by_api"
	finished := r.clock.Now()
	cancelled.FinishedAt = &finished
	f.putJSON(f.jobs, jobKey(id), cancelled)

	stub.release()
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

	rec := f.record(id)
	if rec.State != StateCancelled || rec.Artifact != nil {
		t.Fatalf("record is %q naming %+v, want cancelled and naming nothing", rec.State, rec.Artifact)
	}
	f.waitObjectGone(r, object)
	if got := r.recorder.collectionRows(); len(got) != 0 {
		t.Fatalf("collection rows are %v, want none: a lost final update records nothing", got)
	}
	if got := r.recorder.durationCount(); got != 0 {
		t.Fatalf("%d duration observations, want none", got)
	}
	if got := r.logs.transitions(); len(got) != 1 {
		t.Fatalf("%d transition records, want only the claim", len(got))
	}
}

// TestWorkerCancellationIgnoringWork proves the local slot and the memory it
// stands for stay held until the work goroutine exits, so a replacement
// Collection on this replica waits for it.
func TestWorkerCancellationIgnoringWork(t *testing.T) {
	f := startPGO(t)
	first := f.seedClaimable("payment", "first")
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(first) })

	stub := newRunStub(workResult{Object: first + "-1.pprof"})
	stub.ignoreCancel = true
	w := r.newWorker(stub.fn(), func(cfg *config.PGOConfig) {
		cfg.Limits = limitsWith(func(l *config.PGOLimits) { l.MaxActiveCollections = 1 })
	})
	scanNow(t, w)
	claimed := waitClaimed(t, f, first, 1)
	stub.waitStarted(t)

	r.clock.Set(claimed.LeaseUntil.Add(time.Second))
	stub.waitCancelled(t)

	second := f.seedClaimable("payment", "second")
	r.waitCache("holds the second record", func(c *Caches) bool { return c.hasJob(second) })
	scanNow(t, w)
	if got := f.record(second).State; got != StatePending {
		t.Fatalf("the second record is %q, want pending: the local slot is still held", got)
	}
	if got := w.activeSlots(); got != 1 {
		t.Fatalf("%d local slots are taken, want 1", got)
	}

	stub.release()
	waitFor(t, "the work goroutine exited", func() bool { return w.activeSlots() == 0 })
	scanNow(t, w)
	waitFor(t, "the second record was claimed", func() bool { return f.record(second).Attempt == 1 })
	if got := f.record(first).State; got != StateRunning {
		t.Fatalf("the abandoned record is %q, want running: its owner issued no final update", got)
	}
}

// TestWorkerObservability pins the worker's slice of the operations contract:
// one row per terminal transition it commits, one duration observation for the
// completed one, a gauge that returns to zero, and one transition record per
// state change, carrying no Pod IP.
func TestWorkerObservability(t *testing.T) {
	f := startPGO(t)
	good := f.seedClaimable("payment", "completes")
	bad := f.seedClaimable("payment", "fails")
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds both records", func(c *Caches) bool { return c.hasJob(good) && c.hasJob(bad) })

	completing := newImmediateRunStub(workResult{Object: good + "-1.pprof", Bytes: 7})
	failing := newImmediateRunStub(workResult{Reason: ReasonNoSamples})
	// Both Collections run on this replica at once,
	// which the shipped ceiling of one active Collection would not admit.
	w := r.newWorker(func(ctx context.Context, in workInput) workResult {
		if in.Record.ID == good {
			return completing.fn()(ctx, in)
		}

		return failing.fn()(ctx, in)
	}, func(c *config.PGOConfig) { c.Limits.MaxActiveCollections = 2 })

	scanNow(t, w)
	waitFor(t, "both collections ended", func() bool {
		return terminal(f.record(good).State) && terminal(f.record(bad).State)
	})
	waitFor(t, "both owner loops stopped", func() bool { return w.activeSlots() == 0 })

	want := map[string]int{string(StateCompleted): 1, string(StateFailed): 1}
	if got := r.recorder.collectionRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("collection rows are %v, want %v", got, want)
	}
	if got := r.recorder.durationCount(); got != 1 {
		t.Fatalf("%d duration observations, want 1: only the completed update observes one", got)
	}
	if got := r.recorder.activeGauge(); got != 0 {
		t.Fatalf("the active gauge is %d, want 0", got)
	}
	// One transition record per state change this worker commits: two claims
	// and two terminal updates.
	transitions := r.logs.transitions()
	if len(transitions) != 4 {
		t.Fatalf("%d transition records, want 4", len(transitions))
	}
	states := map[string]int{}
	for _, entry := range transitions {
		state, _ := entry.Attrs["state"].(string)
		states[state]++
		for _, key := range []string{"collection", "namespace", "service", "state", "attempt", "reason", "instance"} {
			if _, ok := entry.Attrs[key]; !ok {
				t.Errorf("a transition record has no %s: %v", key, entry.Attrs)
			}
		}
		if got := entry.Attrs["instance"]; got != "replica" {
			t.Errorf("transition instance is %v, want the claiming replica", got)
		}
	}
	wantStates := map[string]int{string(StateRunning): 2, string(StateCompleted): 1, string(StateFailed): 1}
	if !reflect.DeepEqual(states, wantStates) {
		t.Fatalf("transition states are %v, want %v", states, wantStates)
	}
	if got := ipv4Pattern.FindString(r.logs.text()); got != "" {
		t.Errorf("a log record carries %q, which looks like a pod address", got)
	}

	// Every terminal transition releases the Service.
	if keys := f.keys(f.jobs, activePrefix); len(keys) != 0 {
		t.Errorf("active keys are %v, want none", keys)
	}
}

// TestWorkerReleaseActiveLeavesASuccessorAlone proves the release deletes only
// a key that names this Collection, so it can never free a successor's claim.
func TestWorkerReleaseActiveLeavesASuccessorAlone(t *testing.T) {
	f := startPGO(t)
	id := f.seedClaimable("payment", "payment-api", func(rec *Record) { rec.ClaimBy = slotBase })
	r := f.newReplica("replica", replicaOpts{clock: newFakeClock(slotBase.Add(time.Hour))})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	// The Service has moved on to another Collection by the time the scan
	// fails this one.
	successor := newID()
	f.putJSON(f.jobs, activeKey("payment", "payment-api"), activeValue{ID: successor, CreatedAt: slotBase})

	w := r.newWorker(trapRun(t))
	scanNow(t, w)

	if got := f.record(id); got.Reason != ReasonNotClaimed {
		t.Fatalf("record is %q %q, want failed not_claimed", got.State, got.Reason)
	}
	var active activeValue
	f.getJSON(f.jobs, activeKey("payment", "payment-api"), &active)
	if active.ID != successor {
		t.Fatalf("the active key names %q, want the successor %q untouched", active.ID, successor)
	}
}

// TestWorkerDrain proves the lifecycle seam cmd/profgate stops through: Drain
// waits for the work to exit, and abandons a Collection still running at its
// deadline rather than blocking shutdown for ever.
func TestWorkerDrain(t *testing.T) {
	t.Run("waits for the work goroutine", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		stub := newRunStub(workResult{Object: id + "-1.pprof"})
		w := r.newWorker(stub.fn())
		scanNow(t, w)
		stub.waitStarted(t)

		drained := make(chan error, 1)
		go func() { drained <- w.Drain(context.Background()) }()
		select {
		case err := <-drained:
			t.Fatalf("Drain returned %v while the work was still running", err)
		case <-time.After(50 * time.Millisecond):
		}

		stub.release()
		select {
		case err := <-drained:
			if err != nil {
				t.Fatalf("Drain returned %v, want nil", err)
			}
		case <-time.After(fixtureTimeout):
			t.Fatal("Drain did not return once the work had exited")
		}
	})

	t.Run("waits for a claim that lands after it began", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")

		// The claim's conditional write is held open, which leaves the scan
		// inside the window between reserving a local slot and registering the
		// Collection: a snapshot taken now sees nothing to wait for.
		claiming := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		hook := &kvHook{}
		hook.setBefore(func(op, key string) (error, bool) {
			if op == "update" && key == jobKey(id) {
				once.Do(func() { close(claiming) })
				<-release
			}

			return nil, false
		})
		r := f.newReplica("replica", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		stub := newRunStub(workResult{Object: id + "-1.pprof"})
		t.Cleanup(stub.release)
		w := r.newWorker(stub.fn())
		scanned := make(chan struct{})
		go func() {
			defer close(scanned)
			w.scan(context.Background())
		}()
		<-claiming

		drained := make(chan error, 1)
		go func() { drained <- w.Drain(context.Background()) }()
		select {
		case err := <-drained:
			t.Fatalf("Drain returned %v while a claim was still committing", err)
		case <-time.After(50 * time.Millisecond):
		}

		close(release)
		stub.waitStarted(t)
		select {
		case err := <-drained:
			t.Fatalf("Drain returned %v while the claim's work goroutine was running", err)
		case <-time.After(50 * time.Millisecond):
		}

		stub.release()
		select {
		case err := <-drained:
			if err != nil {
				t.Fatalf("Drain returned %v, want nil", err)
			}
		case <-time.After(fixtureTimeout):
			t.Fatal("Drain did not return once the work had exited")
		}
		<-scanned
	})

	t.Run("returns at the lease cutoff, whatever the lease", func(t *testing.T) {
		for _, lease := range []time.Duration{30 * time.Second, testLeaseTTL, 10 * time.Minute} {
			t.Run(lease.String(), func(t *testing.T) {
				f := startPGO(t)
				id := f.seedClaimable("payment", "payment-api")
				r := f.newReplica("replica", replicaOpts{})
				r.waitSynced()
				r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

				// A merge takes no context and runs to completion once entered,
				// so the drain has to return without a work goroutine that ignores cancellation.
				stub := newRunStub(workResult{})
				stub.ignoreCancel = true
				t.Cleanup(stub.release)
				w := r.newWorker(stub.fn(), func(c *config.PGOConfig) { c.LeaseTTL = lease })
				scanNow(t, w)
				claimed := waitClaimed(t, f, id, 1)
				stub.waitStarted(t)

				drained := make(chan error, 1)
				go func() { drained <- w.Drain(context.Background()) }()

				cutoff := claimed.LeaseUntil.Add(-skewMargin)
				r.clock.Set(cutoff.Add(-time.Second))
				select {
				case err := <-drained:
					t.Fatalf("Drain returned %v a second short of the lease cutoff", err)
				case <-time.After(50 * time.Millisecond):
				}

				var err error
				waitFor(t, "Drain returned at the lease cutoff", func() bool {
					r.clock.Set(cutoff.Add(time.Second))
					select {
					case err = <-drained:
						return true
					default:
						return false
					}
				})
				if err != nil {
					t.Fatalf("Drain returned %v, want nil: an owner past its cutoff is reclaimed, not a failure", err)
				}
			})
		}
	})

	t.Run("leaves a collection past its cutoff for another replica", func(t *testing.T) {
		f := startPGO(t)
		id := f.seedClaimable("payment", "payment-api")
		one := f.newReplica("replica-one", replicaOpts{})
		two := f.newReplica("replica-two", replicaOpts{})
		for _, r := range []*replica{one, two} {
			r.waitSynced()
			r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		}

		stub := newRunStub(workResult{Object: id + "-1.pprof", Bytes: 4})
		stub.ignoreCancel = true
		t.Cleanup(stub.release)
		w := one.newWorker(stub.fn())
		scanNow(t, w)
		claimed := waitClaimed(t, f, id, 1)
		stub.waitStarted(t)

		drained := make(chan error, 1)
		go func() { drained <- w.Drain(context.Background()) }()
		waitFor(t, "Drain returned at the lease cutoff", func() bool {
			one.clock.Set(claimed.LeaseUntil.Add(-skewMargin + time.Second))
			select {
			case err := <-drained:
				if err != nil {
					t.Errorf("Drain returned %v, want nil", err)
				}

				return true
			default:
				return false
			}
		})

		// The drained owner committed nothing:
		// the record is still the running attempt it was,
		// and the lease it holds is the one it stopped renewing.
		if got := f.record(id); got.State != StateRunning || got.Attempt != 1 {
			t.Fatalf("record is %q attempt %d, want the running attempt the drain left behind", got.State, got.Attempt)
		}

		two.clock.Set(claimed.LeaseUntil.Add(2 * skewMargin))
		reclaimed := newRunStub(workResult{})
		t.Cleanup(reclaimed.release)
		scanNow(t, two.newWorker(reclaimed.fn()))
		rec := waitClaimed(t, f, id, 2)
		if rec.Owner == nil || rec.Owner.Instance != "replica-two" {
			t.Fatalf("owner is %+v, want the reclaiming replica", rec.Owner)
		}
	})

	t.Run("an artifact stored after the drain returned is swept as an orphan", func(t *testing.T) {
		f := startPGO(t)
		// The orphan age is measured against the server's own ModTime,
		// so this fixture runs on a clock anchored to the wall clock.
		now := time.Now().UTC()
		id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
			rec.CreatedAt = now
			rec.ClaimBy = now.Add(time.Hour)
		})
		r := f.newReplica("replica", replicaOpts{clock: newFakeClock(now)})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		object := id + "-1.pprof"
		stub := newRunStub(workResult{Object: object, Bytes: 4})
		stub.ignoreCancel = true
		t.Cleanup(stub.release)
		w := r.newWorker(stub.fn())
		scanNow(t, w)
		claimed := waitClaimed(t, f, id, 1)
		stub.waitStarted(t)

		drained := make(chan error, 1)
		go func() { drained <- w.Drain(context.Background()) }()
		waitFor(t, "Drain returned at the lease cutoff", func() bool {
			r.clock.Set(claimed.LeaseUntil.Add(-skewMargin + time.Second))
			select {
			case err := <-drained:
				if err != nil {
					t.Errorf("Drain returned %v, want nil", err)
				}

				return true
			default:
				return false
			}
		})

		// The work goroutine stores its profile after the drain has returned.
		f.putObject(r, object)
		if got := f.record(id).Artifact; got != nil {
			t.Fatalf("the record names %+v, want no artifact: the drained owner committed nothing", got)
		}

		// The Collection ends on the replica that reclaimed it,
		// naming whatever that attempt produced and never this object.
		f.failRecord(id, ReasonNoSamples)
		r.clock.Set(f.objectModTime(r, object).Add(orphanAge + 2*skewMargin))
		sweepNow(t, r.newSweeper())
		f.waitObjectGone(r, object)
	})

	t.Run("returns at once with nothing in flight", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()

		stub := newRunStub(workResult{})
		t.Cleanup(stub.release)
		drained := make(chan error, 1)
		go func() { drained <- r.newWorker(stub.fn()).Drain(context.Background()) }()
		select {
		case err := <-drained:
			if err != nil {
				t.Fatalf("Drain returned %v, want nil", err)
			}
		case <-time.After(fixtureTimeout):
			t.Fatal("Drain did not return with nothing in flight")
		}
	})
}

// putObject stores a stand-in artifact through a replica's own view.
func (f *pgoFixture) putObject(r *replica, name string) {
	f.t.Helper()
	stores, err := r.client.View(r.client.Generation())
	if err != nil {
		f.t.Fatalf("view: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if err := stores.Artifacts.Put(ctx, name, bytes.NewReader([]byte("profile"))); err != nil {
		f.t.Fatalf("put %s: %v", name, err)
	}
}

// readObject returns what an artifact holds, failing the test when it is absent.
func (f *pgoFixture) readObject(r *replica, name string) []byte {
	f.t.Helper()
	b, err := f.getObject(r, name)
	if err != nil {
		f.t.Fatalf("get object %s: %v", name, err)
	}

	return b
}

// getObject returns an artifact's bytes, or the seam's error.
func (f *pgoFixture) getObject(r *replica, name string) ([]byte, error) {
	f.t.Helper()
	stores, err := r.client.View(r.client.Generation())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	rc, err := stores.Artifacts.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	return io.ReadAll(rc)
}

// waitObjectGone blocks until the named object has been removed.
func (f *pgoFixture) waitObjectGone(r *replica, name string) {
	f.t.Helper()
	waitFor(f.t, "the object "+name+" was deleted", func() bool {
		stores, err := r.client.View(r.client.Generation())
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		rc, err := stores.Artifacts.Get(ctx, name)
		if errors.Is(err, natskv.ErrObjectNotFound) {
			return true
		}
		if err == nil {
			//nolint:errcheck // draining and closing so the subscription goes away
			_, _ = io.Copy(io.Discard, rc)
			//nolint:errcheck // the object is still there either way
			_ = rc.Close()
		}

		return false
	})
}

// seedFiftyRunning writes fifty running Collections, each on a Service of its own,
// owned by a replica that is not this one, with a lease an hour out and a deadline two hours out.
func seedFiftyRunning(f *pgoFixture) []string {
	f.t.Helper()
	lease := slotBase.Add(time.Hour)
	started := slotBase
	deadline := slotBase.Add(2 * time.Hour)
	ids := make([]string, 0, 50)
	for i := range 50 {
		ids = append(ids, f.seedClaimable("payment", fmt.Sprintf("payment-api-%02d", i), func(rec *Record) {
			rec.State = StateRunning
			rec.Attempt = 1
			rec.Owner = &Owner{Instance: "other-replica", Pod: "other-replica"}
			rec.LeaseUntil = &lease
			rec.StartedAt = &started
			rec.Deadline = &deadline
		}))
	}

	return ids
}

// jobReads counts the fresh reads a scan issued over the named Collections.
func jobReads(hook *kvHook, ids []string) int {
	reads := 0
	for _, id := range ids {
		reads += len(hook.callsFor("get", jobKey(id)))
	}

	return reads
}

// jobUpdates counts the conditional updates issued on any Collection record.
func jobUpdates(hook *kvHook) int {
	updates := 0
	for _, c := range hook.operations() {
		if c.Op == "update" && strings.HasPrefix(c.Key, jobPrefix) {
			updates++
		}
	}

	return updates
}

// TestWorkerScanReadsOnlyWhatIsDue proves a pass reads fresh only the records it may act on,
// so one delivery costs the store what is due rather than what is stored:
// a running record under a valid lease is skipped from the cache alone,
// a pending record is read only while this replica could claim it or once its claim deadline has passed,
// and the cache stays a candidate filter that never decides a write.
func TestWorkerScanReadsOnlyWhatIsDue(t *testing.T) {
	t.Run("fifty running records with valid leases cost no read", func(t *testing.T) {
		f := startPGO(t)
		ids := seedFiftyRunning(f)

		hook := &kvHook{}
		r := f.newReplica("replica", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds all fifty", func(c *Caches) bool {
			for _, id := range ids {
				if !c.hasJob(id) {
					return false
				}
			}

			return true
		})
		w := r.newWorker(trapRun(t))
		hook.reset()
		scanNow(t, w)

		if got := jobReads(hook, ids); got != 0 {
			t.Fatalf("the scan read %d records fresh, want 0: none of them is due under a valid lease", got)
		}
	})

	t.Run("lapsed leases cost one read each", func(t *testing.T) {
		f := startPGO(t)
		ids := seedFiftyRunning(f)

		hook := &kvHook{}
		r := f.newReplica("replica", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds all fifty", func(c *Caches) bool {
			for _, id := range ids {
				if !c.hasJob(id) {
					return false
				}
			}

			return true
		})
		stub := newRunStub(workResult{})
		t.Cleanup(stub.release)
		// maxActiveCollections is one, so the first claim takes the only slot
		// and the other forty-nine are read and refused for want of one.
		w := r.newWorker(stub.fn())
		r.clock.Set(slotBase.Add(time.Hour + skewMargin + time.Second))
		hook.reset()
		scanNow(t, w)
		stub.waitStarted(t)

		if got := jobReads(hook, ids); got != 50 {
			t.Fatalf("the scan read %d records fresh, want 50: every lapsed lease is due", got)
		}
		if got := jobUpdates(hook); got != 1 {
			t.Fatalf("the scan issued %d updates, want the one claim the slot allowed", got)
		}
	})

	t.Run("a replica at its ceiling reads no pending record until claimBy passes", func(t *testing.T) {
		f := startPGO(t)
		owned := f.seedClaimable("payment", "payment-api")

		hook := &kvHook{}
		r := f.newReplica("replica", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(owned) })

		// The held stub keeps the one slot maxActiveCollections allows.
		stub := newRunStub(workResult{})
		t.Cleanup(stub.release)
		w := r.newWorker(stub.fn())
		scanNow(t, w)
		stub.waitStarted(t)
		waitClaimed(t, f, owned, 1)

		// A second Service's Collection arrives while the slot is taken;
		// its claim deadline is ten seconds out, inside the lease the owner holds.
		claimBy := slotBase.Add(10 * time.Second)
		pending := f.seedClaimable("payment", "other-api", func(rec *Record) { rec.ClaimBy = claimBy })
		r.waitCache("holds the pending record", func(c *Caches) bool { return c.hasJob(pending) })

		hook.reset()
		scanNow(t, w)
		if got := len(hook.callsFor("get", jobKey(pending))); got != 0 {
			t.Fatalf("a replica with no free slot read the pending record %d times, want 0", got)
		}

		r.clock.Set(claimBy.Add(skewMargin + time.Second))
		hook.reset()
		scanNow(t, w)
		if got := len(hook.callsFor("get", jobKey(pending))); got != 1 {
			t.Fatalf("the pending record past its claim deadline was read %d times, want once", got)
		}
		if rec := f.record(pending); rec.State != StateFailed || rec.Reason != ReasonNotClaimed {
			t.Fatalf("record is %q %q, want failed not_claimed", rec.State, rec.Reason)
		}
	})

	// This case holds on the unchanged code as on the changed:
	// it is the candidate-filter rule the cases above must keep.
	t.Run("a cached lease that lags the store reads fresh and claims nothing", func(t *testing.T) {
		f := startPGO(t)
		lapsed := slotBase.Add(-time.Minute)
		started := slotBase.Add(-2 * time.Minute)
		deadline := slotBase.Add(time.Hour)
		id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
			rec.State = StateRunning
			rec.Attempt = 1
			rec.Owner = &Owner{Instance: "other-replica", Pod: "other-replica"}
			rec.LeaseUntil = &lapsed
			rec.StartedAt = &started
			rec.Deadline = &deadline
		})

		hook := &kvHook{}
		frozen := newFreezer(cacheJobs)
		r := f.newReplica("replica", replicaOpts{
			freezer:    frozen,
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		// The owner renews while this replica's job cache is held,
		// so the cache still shows the lapsed lease the store no longer holds.
		frozen.freeze()
		renewed := f.record(id)
		lease := slotBase.Add(time.Hour)
		renewed.LeaseUntil = &lease
		f.putJSON(f.jobs, jobKey(id), renewed)

		w := r.newWorker(trapRun(t))
		hook.reset()
		scanNow(t, w)

		if got := len(hook.callsFor("get", jobKey(id))); got != 1 {
			t.Fatalf("the record was read %d times, want once: the cache made it a candidate", got)
		}
		if got := jobUpdates(hook); got != 0 {
			t.Fatalf("the scan issued %d updates, want 0: the fresh lease is valid", got)
		}
		if got := f.record(id); got.Attempt != 1 || got.Owner.Instance != "other-replica" {
			t.Fatalf("record is attempt %d owned by %s, want the renewing owner's attempt 1", got.Attempt, got.Owner.Instance)
		}
	})
}
