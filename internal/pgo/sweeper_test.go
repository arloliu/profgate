package pgo

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/natskv"
)

// TestSweeperExpiry proves the order retention runs in and what a lost update
// costs: the object goes before the record flips, and a record that moved
// under the sweeper is left exactly as it was.
func TestSweeperExpiry(t *testing.T) {
	t.Run("the object goes before the record flips", func(t *testing.T) {
		f := startPGO(t)
		hook := &kvHook{}
		r := f.newReplica("replica", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		rec := f.seedCompleted(r, "payment", "payment-api")
		r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })

		hook.reset()
		r.clock.Set(rec.ExpiresAt.Add(2 * skewMargin))
		sweepNow(t, r.newSweeper())

		if got := f.record(rec.ID).State; got != StateExpired {
			t.Fatalf("record is %q, want expired", got)
		}
		if got := f.objectNames(r); len(got) != 0 {
			t.Fatalf("the artifact bucket still holds %v, want the expired object gone", got)
		}

		// A completed record must never name an object that is already gone
		// for longer than one pass, so the delete leads and the flip follows.
		deleted, flipped := -1, -1
		ops := hook.operations()
		for i, c := range ops {
			switch {
			case c.Op == "delete-object" && c.Key == rec.Artifact.Object:
				deleted = i
			case c.Op == "update" && c.Key == jobKey(rec.ID):
				flipped = i
			}
		}
		if deleted < 0 || flipped < 0 || deleted > flipped {
			t.Fatalf("the pass issued %v, want the object deleted before the record flipped", ops)
		}

		if got := r.recorder.collectionRows(); len(got) != 1 || got[string(StateExpired)] != 1 {
			t.Fatalf("collection rows are %v, want one expired", got)
		}
		if got := r.recorder.sweeperDeletes(); len(got) != 1 || got[sweepArtifact] != 1 {
			t.Fatalf("sweeper deletes are %v, want one artifact", got)
		}
		transitions := r.logs.transitions()
		if len(transitions) != 1 || transitions[0].Attrs["state"] != string(StateExpired) {
			t.Fatalf("transition records are %v, want one expired", transitions)
		}
	})

	t.Run("a record inside the margin past its retention is left alone", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		rec := f.seedCompleted(r, "payment", "payment-api")
		r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })

		// Two clocks are assumed to differ by the margin, so an artifact is
		// kept that much longer than its retention rather than that much less.
		r.clock.Set(rec.ExpiresAt.Add(skewMargin / 2))
		sweepNow(t, r.newSweeper())

		if got := f.record(rec.ID).State; got != StateCompleted {
			t.Fatalf("record is %q, want completed inside the margin", got)
		}
		if got := f.objectNames(r); len(got) != 1 {
			t.Fatalf("the bucket holds %v, want the artifact kept inside the margin", got)
		}
		if got := r.recorder.sweeperDeletes(); len(got) != 0 {
			t.Fatalf("sweeper deletes are %v, want none", got)
		}
	})

	t.Run("a lost update leaves the record alone", func(t *testing.T) {
		f := startPGO(t)
		fz := newFreezer(cacheJobs)
		r := f.newReplica("replica", replicaOpts{freezer: fz})
		r.waitSynced()
		rec := f.seedCompleted(r, "payment", "payment-api")
		r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })

		// The record moves after the cache saw it, so the revision the sweeper
		// writes against is no longer the bucket's.
		fz.freeze()
		moved := rec
		moved.Progress.SamplesOK = 9
		f.putJSON(f.jobs, jobKey(rec.ID), moved)

		r.clock.Set(rec.ExpiresAt.Add(2 * skewMargin))
		sweepNow(t, r.newSweeper())

		// The object is gone: the delete runs first and is not conditional.
		// That is the state the missing-object condition exists to finish, on
		// this sweeper's next pass or another replica's.
		if got := f.record(rec.ID).State; got != StateCompleted {
			t.Fatalf("record is %q, want completed: an update at a stale revision must lose", got)
		}
		if got := f.record(rec.ID).Progress.SamplesOK; got != 9 {
			t.Fatalf("the record's progress is %d, want the 9 the other writer left", got)
		}
		if got := r.recorder.collectionRows(); len(got) != 0 {
			t.Fatalf("collection rows are %v, want none: no transition was committed", got)
		}
	})
}

// TestSweeperMissingObjectFlips proves the second half of the expiry pair: a
// completed record whose object has gone becomes expired, whatever its
// retention says, and nothing is deleted for it.
func TestSweeperMissingObjectFlips(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	// Well inside its retention, which is what an expiry that deleted the
	// object and died before the flip leaves behind.
	rec := f.seedCompleted(r, "payment", "payment-api", func(rec *Record) {
		expires := slotBase.Add(24 * time.Hour)
		rec.ExpiresAt = &expires
	})
	r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })
	f.deleteObject(r, rec.Artifact.Object)

	sweepNow(t, r.newSweeper())

	if got := f.record(rec.ID).State; got != StateExpired {
		t.Fatalf("record is %q, want expired: a completed record whose object has gone is expired", got)
	}
	if got := r.recorder.collectionRows(); len(got) != 1 || got[string(StateExpired)] != 1 {
		t.Fatalf("collection rows are %v, want one expired", got)
	}
	if got := r.recorder.sweeperDeletes(); len(got) != 0 {
		t.Fatalf("sweeper deletes are %v, want none: there was nothing left to delete", got)
	}
}

// TestSweeperJobRetention proves which records the sweeper deletes outright
// and when: a completed one never, and every other terminal one only once it
// has outlived pgo.jobRetention.
func TestSweeperJobRetention(t *testing.T) {
	retention := testPGOConfig().JobRetention
	tests := []struct {
		name  string
		state State
		// age is how far past finishedAt the pass runs.
		age  time.Duration
		gone bool
	}{
		{"an expired record past its retention", StateExpired, retention + 2*skewMargin, true},
		{"a failed record past its retention", StateFailed, retention + 2*skewMargin, true},
		{"a cancelled record past its retention", StateCancelled, retention + 2*skewMargin, true},
		{"a failed record inside the margin past its retention", StateFailed, retention + skewMargin/2, false},
		{"a completed record past its retention", StateCompleted, retention + 2*skewMargin, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			r := f.newReplica("replica", replicaOpts{})
			r.waitSynced()
			rec := f.seedCompleted(r, "payment", "payment-api", func(rec *Record) {
				rec.State = tc.state
				if tc.state == StateCompleted {
					// Retention on the artifact outlasts the pass, so the
					// record is not carried off by the expiry condition and
					// the deletion rule is the only one under test.
					expires := slotBase.Add(retention + 1000*time.Hour)
					rec.ExpiresAt = &expires

					return
				}
				// A record that never completed names no artifact.
				rec.Artifact = nil
				rec.ExpiresAt = nil
			})
			r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(rec.ID) })

			r.clock.Set(rec.FinishedAt.Add(tc.age))
			sweepNow(t, r.newSweeper())

			present := f.hasKey(f.jobs, jobKey(rec.ID))
			if present == tc.gone {
				t.Fatalf("the record is present=%v, want gone=%v", present, tc.gone)
			}
			want := 0
			if tc.gone {
				want = 1
			}
			if got := r.recorder.sweeperDeletes()[sweepRecord]; got != want {
				t.Fatalf("the pass recorded %d record deletes, want %d", got, want)
			}
		})
	}
}

// TestSweeperSlotKeys proves a slot key outlives its own retainUntil and
// nothing shorter: the value the key carries decides, not the policy now.
func TestSweeperSlotKeys(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()

	retain := slotBase.Add(time.Hour)
	key := slotKey("payment", "payment-api", slotBase)
	f.putJSON(f.jobs, key, slotValue{RetainUntil: retain})
	r.waitCache("holds the slot key", func(c *Caches) bool {
		_, ok := c.slotEntries()[key]

		return ok
	})

	s := r.newSweeper()
	// Past retainUntil but inside the margin: two clocks are assumed to differ
	// by that much, so the deletion waits for it.
	r.clock.Set(retain.Add(skewMargin / 2))
	sweepNow(t, s)
	if !f.hasKey(f.jobs, key) {
		t.Fatal("the slot key was deleted inside the skew margin that follows its retainUntil")
	}

	r.clock.Set(retain.Add(2 * skewMargin))
	sweepNow(t, s)
	if f.hasKey(f.jobs, key) {
		t.Fatal("the slot key survived its retainUntil and the margin")
	}
	if got := r.recorder.sweeperDeletes(); len(got) != 1 || got[sweepSlot] != 1 {
		t.Fatalf("sweeper deletes are %v, want one slot", got)
	}
}

// TestSweeperOrphanObjects proves the age boundary on both sides and that a
// named object is never touched.
// An object's age is the server's ModTime, so these fixtures run on a clock
// anchored to the wall clock the server stamps with rather than to slotBase.
func TestSweeperOrphanObjects(t *testing.T) {
	t.Run("an unnamed object past the orphan age and the margin goes", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{clock: newFakeClock(time.Now().UTC())})
		r.waitSynced()

		orphan := newID() + "-1.pprof"
		f.putObject(r, orphan)

		r.clock.Set(f.objectModTime(r, orphan).Add(orphanAge + 2*skewMargin))
		sweepNow(t, r.newSweeper())

		if got := f.objectNames(r); len(got) != 0 {
			t.Fatalf("the bucket still holds %v, want the orphan gone", got)
		}
		if got := r.recorder.sweeperDeletes(); len(got) != 1 || got[sweepOrphan] != 1 {
			t.Fatalf("sweeper deletes are %v, want one orphan", got)
		}
	})

	t.Run("an unnamed object inside the margin past the orphan age is kept", func(t *testing.T) {
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{clock: newFakeClock(time.Now().UTC())})
		r.waitSynced()

		orphan := newID() + "-1.pprof"
		f.putObject(r, orphan)

		// A Put slow enough to still be in flight when the completed update
		// that will name it lands is what the age exists for, and the margin
		// covers the two clocks that measured it.
		r.clock.Set(f.objectModTime(r, orphan).Add(orphanAge + skewMargin/2))
		sweepNow(t, r.newSweeper())

		if got := f.objectNames(r); len(got) != 1 {
			t.Fatalf("the bucket holds %v, want the object kept inside the margin", got)
		}
	})

	t.Run("an object a completed record names is kept", func(t *testing.T) {
		base := time.Now().UTC()
		f := startPGO(t)
		r := f.newReplica("replica", replicaOpts{clock: newFakeClock(base)})
		r.waitSynced()
		rec := f.seedCompleted(r, "payment", "payment-api", func(rec *Record) {
			expires := base.Add(time.Hour)
			rec.ExpiresAt = &expires
		})
		r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })

		// Old enough to be a candidate on age alone, and well inside its
		// retention, so only the record naming it saves it.
		r.clock.Set(f.objectModTime(r, rec.Artifact.Object).Add(orphanAge + 2*skewMargin))
		sweepNow(t, r.newSweeper())

		if got := f.objectNames(r); len(got) != 1 {
			t.Fatalf("the bucket holds %v, want the named object kept until its retention", got)
		}
		if got := r.recorder.sweeperDeletes(); len(got) != 0 {
			t.Fatalf("sweeper deletes are %v, want none", got)
		}
	})
}

// TestSweeperFreshReadKeepsAnObject proves the rule that makes the cache a
// candidate filter and never the authority for a delete.
func TestSweeperFreshReadKeepsAnObject(t *testing.T) {
	t.Run("a cache that has not delivered a completed record costs nothing", func(t *testing.T) {
		base := time.Now().UTC()
		f := startPGO(t)
		fz := newFreezer(cacheJobs)
		r := f.newReplica("replica", replicaOpts{clock: newFakeClock(base), freezer: fz})
		r.waitSynced()

		// The authoritative bucket holds the Collection; this replica's watch
		// has not delivered it, which is what a listing arriving before a
		// replay finishes looks like.
		fz.freeze()
		rec := f.seedCompleted(r, "payment", "payment-api", func(rec *Record) {
			expires := base.Add(time.Hour)
			rec.ExpiresAt = &expires
		})

		r.clock.Set(f.objectModTime(r, rec.Artifact.Object).Add(orphanAge + 2*skewMargin))
		sweepNow(t, r.newSweeper())

		if got := f.objectNames(r); len(got) != 1 {
			t.Fatalf("the bucket holds %v, want the live artifact of %s kept", got, rec.ID)
		}
		if got := r.recorder.sweeperDeletes(); len(got) != 0 {
			t.Fatalf("sweeper deletes are %v, want none", got)
		}
	})

	t.Run("an unavailable read keeps the object", func(t *testing.T) {
		base := time.Now().UTC()
		f := startPGO(t)
		hook := &kvHook{}
		r := f.newReplica("replica", replicaOpts{
			clock:      newFakeClock(base),
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()

		// No record at all: without the failed read this object would go.
		orphan := newID() + "-1.pprof"
		f.putObject(r, orphan)
		id, ok := collectionOf(orphan)
		if !ok {
			t.Fatalf("%q is not an artifact name", orphan)
		}
		hook.setBefore(func(op, key string) (error, bool) {
			if op == "get" && key == jobKey(id) {
				return natskv.ErrUnavailable, true
			}

			return nil, false
		})

		r.clock.Set(f.objectModTime(r, orphan).Add(orphanAge + 2*skewMargin))
		sweepNow(t, r.newSweeper())

		if got := f.objectNames(r); len(got) != 1 {
			t.Fatalf("the bucket holds %v, want the object kept while the store is unavailable", got)
		}
		if got := r.recorder.sweeperDeletes(); len(got) != 0 {
			t.Fatalf("sweeper deletes are %v, want none", got)
		}
	})
}

// TestSweeperActiveKeys proves a Service is released only once a fresh read
// shows its Collection over, and never while one is still live.
func TestSweeperActiveKeys(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()

	live := f.seedRecord("payment", "live-api", StateRunning)
	f.putJSON(f.jobs, activeKey("payment", "live-api"), activeValue{ID: live, CreatedAt: slotBase})

	done := f.seedCompleted(r, "payment", "done-api", func(rec *Record) {
		expires := slotBase.Add(24 * time.Hour)
		rec.ExpiresAt = &expires
	})
	f.putJSON(f.jobs, activeKey("payment", "done-api"), activeValue{ID: done.ID, CreatedAt: slotBase})

	// A key naming a Collection whose record has already been retired.
	f.putJSON(f.jobs, activeKey("payment", "gone-api"), activeValue{ID: newID(), CreatedAt: slotBase})

	r.waitCache("holds the three active keys", func(c *Caches) bool { return len(c.activeEntries()) == 3 })

	sweepNow(t, r.newSweeper())

	if !f.hasKey(f.jobs, activeKey("payment", "live-api")) {
		t.Fatal("the active key of a running collection was released")
	}
	if f.hasKey(f.jobs, activeKey("payment", "done-api")) {
		t.Fatal("the active key of a completed collection was kept")
	}
	if f.hasKey(f.jobs, activeKey("payment", "gone-api")) {
		t.Fatal("the active key of a collection with no record was kept")
	}
	if got := r.recorder.sweeperDeletes()[sweepActive]; got != 2 {
		t.Fatalf("the pass released %d active keys, want 2", got)
	}
}

// TestSweeperKeepsAPausedCreatorsActiveKey proves the publication order pays
// off: a creator paused between its active create and its pending update
// leaves an initializing record, a fresh read finds it, and the key stays.
// The Service is busy for anyone else meanwhile, so exactly one Collection
// results.
func TestSweeperKeepsAPausedCreatorsActiveKey(t *testing.T) {
	f := startPGO(t)

	// The publication's one update is the pending CAS, which follows the
	// active create.
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	var held atomic.Bool
	hook := &kvHook{}
	hook.setBefore(func(op, key string) (error, bool) {
		if op == "update" && strings.HasPrefix(key, jobPrefix) && held.CompareAndSwap(false, true) {
			reached <- struct{}{}
			<-release
		}

		return nil, false
	})

	r := f.newReplica("replica", replicaOpts{
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	r.waitSynced()

	type publication struct {
		id      string
		outcome Outcome
		err     error
	}
	first := make(chan publication, 1)
	go func() {
		id, outcome, err := r.publishOne(t, r.loopJobsView(), "payment", "payment-api")
		first <- publication{id: id, outcome: outcome, err: err}
	}()
	<-reached
	r.waitCache("holds the active key", func(c *Caches) bool {
		_, ok := c.activeID("payment", "payment-api")

		return ok
	})

	sweepNow(t, r.newSweeper())
	if !f.hasKey(f.jobs, activeKey("payment", "payment-api")) {
		t.Fatal("the sweeper released the active key of a collection still being published")
	}
	if got := r.recorder.sweeperDeletes()[sweepActive]; got != 0 {
		t.Fatalf("the pass released %d active keys, want none", got)
	}

	if _, outcome, err := r.publishOne(t, r.loopJobsView(), "payment", "payment-api"); err != nil || outcome != OutcomeBusy {
		t.Fatalf("the second creator got %q, %v; want busy", outcome, err)
	}

	close(release)
	got := <-first
	if got.err != nil || got.outcome != OutcomeWon {
		t.Fatalf("the paused creator got %q, %v; want won", got.outcome, got.err)
	}
	if live := f.liveServices(); len(live) != 1 {
		t.Fatalf("the bucket shows %d services live, want 1", len(live))
	}
	if records := f.nonterminalRecords(); len(records) != 1 || records[0].ID != got.id {
		t.Fatalf("the bucket holds %d live records, want only %s", len(records), got.id)
	}
}

// TestSweeperProbeCleanup proves a preflight probe a crash stranded is removed
// by age alone, in both KV buckets and in the artifact bucket.
// A probe's age is the server's timestamp — Entry.Created for a key, ModTime
// for an object — so these fixtures run on a clock anchored to the wall clock.
func TestSweeperProbeCleanup(t *testing.T) {
	tests := []struct {
		name string
		// age is how far past the newest of the three probes the pass runs.
		age  time.Duration
		gone bool
	}{
		{"a probe inside the margin past the orphan age may still be a live preflight's", orphanAge + skewMargin/2, false},
		{"a probe past the orphan age and the margin was stranded", orphanAge + 2*skewMargin, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			r := f.newReplica("replica", replicaOpts{clock: newFakeClock(time.Now().UTC())})
			r.waitSynced()

			jobsProbe := probeKeyPrefix + "jobs-instance"
			configProbe := probeKeyPrefix + "config-instance"
			objectProbe := probeObjectPrefix + "instance"
			f.putJSON(f.jobs, jobsProbe, "profgate preflight probe")
			f.putJSON(f.config, configProbe, "profgate preflight probe")
			f.putObject(r, objectProbe)

			// The server stamps each one as it lands, so the pass is placed
			// relative to the last of them: all three are then the same side
			// of the threshold.
			newest := f.keyCreated(f.jobs, jobsProbe)
			for _, at := range []time.Time{f.keyCreated(f.config, configProbe), f.objectModTime(r, objectProbe)} {
				if at.After(newest) {
					newest = at
				}
			}

			r.clock.Set(newest.Add(tc.age))
			sweepNow(t, r.newSweeper())

			if got := f.hasKey(f.jobs, jobsProbe); got == tc.gone {
				t.Errorf("the jobs probe key is present=%v, want gone=%v", got, tc.gone)
			}
			if got := f.hasKey(f.config, configProbe); got == tc.gone {
				t.Errorf("the config probe key is present=%v, want gone=%v", got, tc.gone)
			}
			if got := len(f.objectNames(r)); (got == 0) != tc.gone {
				t.Errorf("the artifact bucket holds %d objects, want gone=%v", got, tc.gone)
			}
			want := 0
			if tc.gone {
				want = 3
			}
			if got := r.recorder.sweeperDeletes()[sweepProbe]; got != want {
				t.Fatalf("the pass deleted %d probes, want %d", got, want)
			}
		})
	}
}

// TestSweeperBehindTheBarrier proves nothing is swept from caches that have
// not finished replaying, and that the first pass after the marker does the
// work the held ones did not.
func TestSweeperBehindTheBarrier(t *testing.T) {
	f := startPGO(t)
	hook := &kvHook{}
	fz := newArmedFreezer(cacheOverrides, cacheJobs, cacheActive, cacheSlots)
	r := f.newReplica("replica", replicaOpts{
		freezer:    fz,
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	rec := f.seedCompleted(r, "payment", "payment-api")
	r.clock.Set(rec.ExpiresAt.Add(2 * skewMargin))

	s := r.newSweeper()
	hook.reset()
	for range 3 {
		sweepNow(t, s)
	}

	// A pass that read anything at all would be deciding from caches with an
	// unknown gap behind them, whatever it then chose to do.
	if got := hook.operations(); len(got) != 0 {
		t.Fatalf("a pass behind the barrier issued %v, want no store operation", got)
	}
	if got := f.record(rec.ID).State; got != StateCompleted {
		t.Fatalf("record is %q, want completed: nothing is swept behind the barrier", got)
	}
	if got := f.objectNames(r); len(got) != 1 {
		t.Fatalf("the bucket holds %v, want the artifact untouched", got)
	}
	if got := r.recorder.sweeperDeletes(); len(got) != 0 {
		t.Fatalf("sweeper deletes are %v, want none behind the barrier", got)
	}

	fz.release()
	r.waitSynced()
	r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })
	sweepNow(t, s)

	if got := f.record(rec.ID).State; got != StateExpired {
		t.Fatalf("record is %q, want expired once the marker was delivered", got)
	}
	if got := f.objectNames(r); len(got) != 0 {
		t.Fatalf("the bucket still holds %v, want the expired object gone", got)
	}
}

// TestSweeperRun proves the loop sweeps on its clock and stops with its context.
func TestSweeperRun(t *testing.T) {
	f := startPGO(t)
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	rec := f.seedCompleted(r, "payment", "payment-api")
	r.waitCache("holds the completed record", func(c *Caches) bool { return c.hasJob(rec.ID) })
	r.clock.Set(rec.ExpiresAt.Add(2 * skewMargin))

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.newSweeper().Run(ctx)
	}()

	waitFor(t, "the loop started its ticker", func() bool { return r.clock.tickerCount() > 0 })
	waitFor(t, "a pass expired the collection", func() bool {
		r.clock.Advance(sweeperTick)

		return f.record(rec.ID).State == StateExpired
	})

	cancel()
	select {
	case <-stopped:
	case <-time.After(fixtureTimeout):
		t.Fatal("Run did not return when its context ended")
	}
}

// TestCollectionOf pins the artifact-name grammar the orphan rule reads, so a
// key this version does not write is left alone rather than misread.
func TestCollectionOf(t *testing.T) {
	id := newID()
	tests := []struct {
		object string
		want   string
	}{
		{id + "-1.pprof", id},
		{id + "-12.pprof", id},
		{id + "-1.txt", ""},
		{id + ".pprof", ""},
		{id + "-.pprof", ""},
		{id + "-x.pprof", ""},
		{"not-an-id-1.pprof", ""},
		{probeObjectPrefix + "instance", ""},
	}

	for _, tc := range tests {
		t.Run(tc.object, func(t *testing.T) {
			got, ok := collectionOf(tc.object)
			if ok != (tc.want != "") || got != tc.want {
				t.Fatalf("collectionOf(%q) is %q, %v; want %q", tc.object, got, ok, tc.want)
			}
		})
	}
}

// deleteObject removes one artifact, standing in for a sweeper pass that
// deleted it and died before the flip, or for a loss outside Profgate.
func (f *pgoFixture) deleteObject(r *replica, name string) {
	f.t.Helper()
	stores, err := r.client.View(r.client.Generation())
	if err != nil {
		f.t.Fatalf("view: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if err := stores.Artifacts.Delete(ctx, name); err != nil {
		f.t.Fatalf("delete object %s: %v", name, err)
	}
}
