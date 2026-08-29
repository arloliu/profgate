package pgo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/natskv"
)

// TestSlotFor pins the slot arithmetic: floor(now / every) × every, aligned to
// the Unix epoch in UTC, and never a catch-up of a slot already past.
func TestSlotFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		now   time.Time
		every time.Duration
		want  time.Time
	}{
		{"start of a slot", slotBase, time.Hour, slotBase},
		{"inside a slot", slotBase.Add(59 * time.Minute), time.Hour, slotBase},
		{"one nanosecond before the next", slotBase.Add(time.Hour - 1), time.Hour, slotBase},
		{"start of the next", slotBase.Add(time.Hour), time.Hour, slotBase.Add(time.Hour)},
		{"a day-long slot", slotBase.Add(23*time.Hour + 59*time.Minute), 24 * time.Hour, slotBase},
		{"a quarter-hour slot", slotBase.Add(20 * time.Minute), 15 * time.Minute, slotBase.Add(15 * time.Minute)},
		{"unaligned now", slotBase.Add(7*time.Hour + 3*time.Minute), 6 * time.Hour, slotBase.Add(6 * time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := slotFor(tc.now, tc.every); !got.Equal(tc.want) {
				t.Errorf("slotFor(%s, %s) = %s, want %s", tc.now, tc.every, got, tc.want)
			}
		})
	}
}

// TestSlotEncoding pins the one encoding both gateway versions must agree on:
// the key and the jitter hash input for the slot starting 2026-08-24T00:00:00Z.
// An encoding left to the implementation could let two versions fire the same
// slot twice.
func TestSlotEncoding(t *testing.T) {
	t.Parallel()

	const (
		wantKey  = "schedule.payment.payment-api.1787529600"
		wantHash = "payment/payment-api/1787529600"
	)
	if got := slotKey("payment", "payment-api", slotBase); got != wantKey {
		t.Errorf("slotKey = %q, want %q", got, wantKey)
	}
	if got := hashInput("payment", "payment-api", slotBase); got != wantHash {
		t.Errorf("hashInput = %q, want %q", got, wantHash)
	}
}

// TestOffset proves the fire time is a function of the three inputs alone, so
// every replica computes the same one and jitter spreads Services rather than
// replicas.
func TestOffset(t *testing.T) {
	t.Parallel()

	const jitter = 10 * time.Minute

	t.Run("the same inputs give the same offset", func(t *testing.T) {
		first := offset("payment", "payment-api", slotBase, jitter)
		second := offset("payment", "payment-api", slotBase, jitter)
		if first != second {
			t.Fatalf("offset is not a function of its inputs: %s then %s", first, second)
		}
		if first < 0 || first >= jitter {
			t.Fatalf("offset %s is outside [0, %s)", first, jitter)
		}
	})

	t.Run("no jitter fires at the slot start", func(t *testing.T) {
		if got := offset("payment", "payment-api", slotBase, 0); got != 0 {
			t.Fatalf("offset without jitter = %s, want 0", got)
		}
	})

	t.Run("different services spread across the window", func(t *testing.T) {
		seen := make(map[time.Duration]struct{})
		for i := range 20 {
			seen[offset("payment", fmt.Sprintf("svc-%d", i), slotBase, jitter)] = struct{}{}
		}
		if len(seen) < 15 {
			t.Fatalf("20 services produced %d distinct offsets, want the jitter to spread them", len(seen))
		}
	})

	t.Run("different slots move the offset", func(t *testing.T) {
		first := offset("payment", "payment-api", slotBase, jitter)
		second := offset("payment", "payment-api", slotBase.Add(time.Hour), jitter)
		if first == second {
			t.Fatalf("two slots share offset %s; the slot is not in the hash input", first)
		}
	})
}

// TestSchedulerOneRecordPerSlot runs two replicas over one server with clocks
// interleaved inside one slot: the slot key admits exactly one of them and the
// Service ends with exactly one Collection.
func TestSchedulerOneRecordPerSlot(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))

	one := f.newReplica("replica-one", replicaOpts{clock: newFakeClock(slotBase)})
	two := f.newReplica("replica-two", replicaOpts{clock: newFakeClock(slotBase)})
	for _, r := range []*replica{one, two} {
		r.waitSynced()
		r.waitCache("holds the override", func(c *Caches) bool {
			_, ok := c.overrideSnapshot()[serviceRef{Namespace: "payment", Service: "payment-api"}]

			return ok
		})
	}

	for i := range 100 {
		one.clock.Set(slotBase.Add(time.Duration(i) * 7 * time.Second))
		two.clock.Set(slotBase.Add(time.Duration(i) * 11 * time.Second))
		one.tick()
		two.tick()
	}

	records := f.records()
	if len(records) != 1 {
		t.Fatalf("100 interleaved ticks left %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.State != StatePending {
		t.Errorf("record state is %q, want %q", rec.State, StatePending)
	}
	if rec.Origin != OriginSchedule {
		t.Errorf("record origin is %q, want %q", rec.Origin, OriginSchedule)
	}
	if rec.Slot != slotBase.Format(time.RFC3339) {
		t.Errorf("record slot is %q, want %q", rec.Slot, slotBase.Format(time.RFC3339))
	}
	if rec.CreatedBy != createdBySchedule {
		t.Errorf("record createdBy is %q, want %q", rec.CreatedBy, createdBySchedule)
	}
	if slots := f.keys(f.jobs, slotPrefix); len(slots) != 1 {
		t.Errorf("slot keys are %v, want exactly one", slots)
	}
	if active := f.keys(f.jobs, activePrefix); len(active) != 1 {
		t.Errorf("active keys are %v, want exactly one", active)
	}
}

// TestSchedulerIneligibleServices proves a Service that is off, over a
// ceiling, or has no override at all never consumes a slot.
func TestSchedulerIneligibleServices(t *testing.T) {
	disabled := false
	tests := []struct {
		name     string
		override *PolicyOverride
	}{
		{"disabled", &PolicyOverride{Enabled: &disabled}},
		{"no override", nil},
		{"over a ceiling", enabledOverride(withEvery(48 * time.Hour))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			f.setOverride("payment", "payment-api", tc.override)
			r := f.newReplica("replica", replicaOpts{})
			r.waitSynced()
			r.waitCache("holds the override", func(c *Caches) bool {
				return len(c.overrideSnapshot()) == 1
			})
			r.tick()

			if keys := f.keys(f.jobs, slotPrefix); len(keys) != 0 {
				t.Errorf("slot keys are %v, want none", keys)
			}
			if keys := f.jobKeys(); len(keys) != 0 {
				t.Errorf("records are %v, want none", keys)
			}
			if rows := r.recorder.scheduleSlots(); len(rows) != 0 {
				t.Errorf("an ineligible service recorded %v, want no row", rows)
			}
		})
	}
}

// TestSchedulerLogsViolationOncePerRevision proves an override the ceilings
// refuse is reported once for each revision of it, not once every ten seconds.
func TestSchedulerLogsViolationOncePerRevision(t *testing.T) {
	const message = "pgo: policy exceeds a ceiling and the service is not scheduled"

	f := startPGO(t)
	// One ceiling crossed and nothing else, so the count of log lines is the
	// count of evaluations rather than the count of faults.
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(2*time.Hour)))
	r := f.newReplica("replica", replicaOpts{
		limits: limitsWith(func(l *config.PGOLimits) { l.MaxEvery = time.Hour }),
	})
	r.waitSynced()
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

	for range 3 {
		r.tick()
	}
	logged := r.logs.with(message)
	if len(logged) != 1 {
		t.Fatalf("three ticks logged the violation %d times, want once", len(logged))
	}
	if got := logged[0].Attrs["field"]; got != "schedule.every" {
		t.Errorf("violation names field %v, want schedule.every", got)
	}
	if got := logged[0].Attrs["ceiling"]; got != "pgo.limits.maxEvery" {
		t.Errorf("violation names ceiling %v, want pgo.limits.maxEvery", got)
	}

	rev := f.setOverride("payment", "payment-api", enabledOverride(withEvery(3*time.Hour)))
	r.waitCache("holds the new revision", func(c *Caches) bool {
		return c.overrideSnapshot()[serviceRef{Namespace: "payment", Service: "payment-api"}].Revision == rev
	})
	r.tick()
	r.tick()
	if logged := r.logs.with(message); len(logged) != 2 {
		t.Fatalf("a second revision logged the violation %d times in total, want twice", len(logged))
	}
}

// TestSchedulerNeverCatchesUp proves a gateway that returns after three days
// creates the current slot's Collection and no others.
func TestSchedulerNeverCatchesUp(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

	r.tick()
	if got := len(f.jobKeys()); got != 1 {
		t.Fatalf("the first tick left %d records, want 1", got)
	}
	f.finishCollection("payment", "payment-api")
	r.waitCache("sees the service free again", func(c *Caches) bool { return !c.Live("payment", "payment-api") })

	r.clock.Advance(72 * time.Hour)
	r.tick()

	if got := len(f.jobKeys()); got != 2 {
		t.Fatalf("a three-day jump left %d records in total, want 2", got)
	}
	if got := len(f.keys(f.jobs, slotPrefix)); got != 2 {
		t.Fatalf("a three-day jump left %d slot keys, want 2", got)
	}
}

// TestSchedulerPolicyChangeInsideOneSlot proves a policy edited mid-slot
// cannot fire the same slot twice: the slot key carries no configuration
// revision, so the second attempt loses it.
func TestSchedulerPolicyChangeInsideOneSlot(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })
	r.tick()
	f.finishCollection("payment", "payment-api")
	r.waitCache("sees the service free again", func(c *Caches) bool { return !c.Live("payment", "payment-api") })

	rounds := 3
	rev := f.setOverride("payment", "payment-api", enabledOverride(
		withEvery(time.Hour), withJitter(0),
		func(o *PolicyOverride) { o.Sampling = &SamplingOverride{Rounds: &rounds} },
	))
	r.waitCache("holds the new revision", func(c *Caches) bool {
		return c.overrideSnapshot()[serviceRef{Namespace: "payment", Service: "payment-api"}].Revision == rev
	})
	r.clock.Advance(5 * time.Minute)
	r.tick()

	if got := len(f.jobKeys()); got != 1 {
		t.Fatalf("a policy change inside one slot left %d records, want 1", got)
	}
	if got := r.recorder.scheduleSlots()[slotLost]; got != 1 {
		t.Fatalf("the second attempt recorded %d lost rows, want 1", got)
	}
}

// TestSchedulerSlotRetention proves the slot key carries the retention its own
// every implies and outlives its slot, so a policy shortened afterwards cannot
// let a past slot be attempted again.
// Deleting the key after retainUntil is the sweeper's, and lands with it.
func TestSchedulerSlotRetention(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(24*time.Hour), withJitter(0)))
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

	firstKey := slotKey("payment", "payment-api", slotBase)
	for day := range 3 {
		r.clock.Set(slotBase.Add(time.Duration(day) * 24 * time.Hour))
		r.tick()
		f.finishCollection("payment", "payment-api")
		r.waitCache("sees the service free again", func(c *Caches) bool { return !c.Live("payment", "payment-api") })
	}

	var first slotValue
	f.getJSON(f.jobs, firstKey, &first)
	wantRetain := slotBase.Add(48 * time.Hour)
	if !first.RetainUntil.Equal(wantRetain) {
		t.Errorf("retainUntil is %s, want %s", first.RetainUntil, wantRetain)
	}

	// The clock is now two days past the first key's slot and past its
	// retainUntil, and the key is still there for the sweeper to remove.
	keys := f.keys(f.jobs, slotPrefix)
	if len(keys) != 3 {
		t.Fatalf("three days left %d slot keys, want 3", len(keys))
	}
	waitFor(t, "the cache holds every slot key", func() bool { return len(r.caches.slotEntries()) == 3 })
	cached := r.caches.slotEntries()
	if _, ok := cached[firstKey]; !ok {
		t.Fatalf("the first slot key is gone from the cache: %v", cached)
	}

	// A replica considers only the slot containing now, so the first slot is
	// never attempted again however far the clock has moved.
	r.tick()
	if got := len(f.keys(f.jobs, slotPrefix)); got != 3 {
		t.Fatalf("a further tick left %d slot keys, want 3", got)
	}
}

// TestSchedulerBusyService proves the two ways a Service that already has a
// live Collection is refused: from the cache, which spares a write, and from
// the active create itself, which is the decision.
func TestSchedulerBusyService(t *testing.T) {
	t.Run("the cache spares the write", func(t *testing.T) {
		f := startPGO(t)
		f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
		f.seedLiveCollection("payment", "payment-api", StateRunning)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("sees the service live", func(c *Caches) bool {
			return len(c.overrideSnapshot()) == 1 && c.Live("payment", "payment-api")
		})
		r.tick()

		if got := r.recorder.scheduleSlots()[slotBusy]; got != 1 {
			t.Fatalf("recorded %d busy rows, want 1", got)
		}
		if keys := f.keys(f.jobs, slotPrefix); len(keys) != 0 {
			t.Errorf("a cached-busy service wrote slot keys %v, want none", keys)
		}
		if got := len(f.jobKeys()); got != 1 {
			t.Errorf("records are %d, want the one that was already live", got)
		}
	})

	t.Run("a frozen cache falls through to a losing active create", func(t *testing.T) {
		f := startPGO(t)
		f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
		frozen := newFreezer(cacheJobs, cacheActive)
		r := f.newReplica("replica", replicaOpts{freezer: frozen})
		r.waitSynced()
		r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

		frozen.freeze()
		liveID := f.seedLiveCollection("payment", "payment-api", StateRunning)
		r.tick()

		if got := r.recorder.scheduleSlots()[slotBusy]; got != 1 {
			t.Fatalf("recorded %v, want one busy row", r.recorder.scheduleSlots())
		}
		if got := len(f.jobKeys()); got != 1 {
			t.Errorf("the lost creator left %d records, want only the live one", got)
		}
		var active activeValue
		f.getJSON(f.jobs, activeKey("payment", "payment-api"), &active)
		if active.ID != liveID {
			t.Errorf("active key names %q, want the live collection %q", active.ID, liveID)
		}
		if keys := f.keys(f.jobs, slotPrefix); len(keys) != 1 {
			t.Errorf("the slot key was not consumed: %v", keys)
		}
		frozen.release()
	})
}

// TestSchedulerDifferentEveryRevisions proves a change to every cannot run two
// Collections at once: two replicas on different revisions compute different
// slot keys and both win them, and the active key admits one Collection.
func TestSchedulerDifferentEveryRevisions(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))

	held := newFreezer(cacheOverrides)
	one := f.newReplica("replica-one", replicaOpts{clock: newFakeClock(slotBase.Add(time.Hour)), freezer: held})
	frozen := newFreezer(cacheJobs, cacheActive)
	two := f.newReplica("replica-two", replicaOpts{clock: newFakeClock(slotBase.Add(time.Hour)), freezer: frozen})
	for _, r := range []*replica{one, two} {
		r.waitSynced()
		r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })
	}

	// One replica stays on every: 1h while the other moves to every: 2h.
	held.freeze()
	rev := f.setOverride("payment", "payment-api", enabledOverride(withEvery(2*time.Hour), withJitter(0)))
	two.waitCache("holds the new every", func(c *Caches) bool {
		return c.overrideSnapshot()[serviceRef{Namespace: "payment", Service: "payment-api"}].Revision == rev
	})

	// The second replica must not see the first's publication, so its fall
	// through to the active create is the only thing that refuses it.
	frozen.freeze()
	one.tick()
	two.tick()

	wantSlots := []string{
		slotKey("payment", "payment-api", slotBase),
		slotKey("payment", "payment-api", slotBase.Add(time.Hour)),
	}
	got := f.keys(f.jobs, slotPrefix)
	if len(got) != 2 {
		t.Fatalf("two every revisions left slot keys %v, want %v", got, wantSlots)
	}
	if len(f.jobKeys()) != 1 {
		t.Fatalf("two won slots left %d records, want exactly one", len(f.jobKeys()))
	}
	if two.recorder.scheduleSlots()[slotBusy] != 1 {
		t.Errorf("the losing creator recorded %v, want one busy row", two.recorder.scheduleSlots())
	}
	held.release()
}

// TestSchedulerAndOnDemandRace releases a scheduler tick and an on-demand
// publication from one barrier against a frozen cache: one record, one active
// key, and the other creator refused.
func TestSchedulerAndOnDemandRace(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
	frozen := newFreezer(cacheJobs, cacheActive)
	r := f.newReplica("replica", replicaOpts{freezer: frozen})
	r.waitSynced()
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })
	frozen.freeze()

	var (
		start      = make(chan struct{})
		wg         sync.WaitGroup
		apiOutcome Outcome
		apiErr     error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		r.tick()
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		res, ok := r.pub.Reserve("payment", "payment-api")
		if !ok {
			apiErr = errors.New("the on-demand reservation was refused")

			return
		}
		<-start
		_, apiOutcome, apiErr = r.pub.Publish(ctx, r.jobsView(), res, PublishInput{
			Namespace: "payment", Service: "payment-api",
			Origin:  OriginAPI,
			ClaimBy: slotBase.Add(time.Hour), Policy: schedulerDefaults(t), CreatedBy: "tester",
		})
	}()
	close(start)
	wg.Wait()

	if apiErr != nil {
		t.Fatalf("the on-demand publication failed: %v", apiErr)
	}
	if got := len(f.jobKeys()); got != 1 {
		t.Fatalf("two creators left %d records, want exactly one", got)
	}
	if got := f.keys(f.jobs, activePrefix); len(got) != 1 {
		t.Fatalf("two creators left active keys %v, want exactly one", got)
	}
	won := r.recorder.scheduleSlots()[slotWon]
	if apiOutcome == OutcomeWon {
		won++
	}
	if won != 1 {
		t.Fatalf("%d creators won, want exactly one", won)
	}
}

// TestSchedulerSlotCreateUnavailable proves that a slot create whose result is
// unknown publishes nothing from that attempt, whether or not the key
// committed, and gives the reservation back either way.
func TestSchedulerSlotCreateUnavailable(t *testing.T) {
	tests := []struct {
		name      string
		hook      func(*kvHook)
		wantSlots int
	}{
		{
			name: "uncommitted",
			hook: func(h *kvHook) {
				h.before = func(op, key string) (error, bool) {
					if op == "create" && strings.HasPrefix(key, slotPrefix) {
						return natskv.ErrUnavailable, true
					}

					return nil, false
				}
			},
			wantSlots: 0,
		},
		{
			name: "committed with a lost acknowledgement",
			hook: func(h *kvHook) {
				h.after = func(op, key string, err error) error {
					if op == "create" && strings.HasPrefix(key, slotPrefix) {
						return natskv.ErrUnavailable
					}

					return err
				}
			},
			wantSlots: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
			hook := &kvHook{}
			tc.hook(hook)
			r := f.newReplica("replica", replicaOpts{
				wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
			})
			r.waitSynced()
			r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })
			r.tick()

			if got := len(f.keys(f.jobs, slotPrefix)); got != tc.wantSlots {
				t.Errorf("slot keys are %d, want %d", got, tc.wantSlots)
			}
			if got := f.jobKeys(); len(got) != 0 {
				t.Errorf("records are %v, want none", got)
			}
			if got := f.keys(f.jobs, activePrefix); len(got) != 0 {
				t.Errorf("active keys are %v, want none", got)
			}
			if got := r.pub.Reserved(); got != 0 {
				t.Errorf("reservations held are %d, want 0: no active key can exist for this attempt", got)
			}
		})
	}
}

// TestSchedulerSlotMetrics proves one pass over a won, a lost, a busy, and a
// capacity-refused Service leaves exactly those four rows.
func TestSchedulerSlotMetrics(t *testing.T) {
	f := startPGO(t)
	// Considered in this order, with one Service already live and the ceiling
	// at two, so each outcome falls to a different Service.
	for _, svc := range []string{"s1-lost", "s2-won", "s3-busy", "s4-capacity"} {
		f.setOverride("payment", svc, enabledOverride(withEvery(time.Hour), withJitter(0)))
	}
	f.seedLiveCollection("payment", "s3-busy", StateRunning)
	f.putJSON(f.jobs, slotKey("payment", "s1-lost", slotBase), slotValue{RetainUntil: slotBase.Add(48 * time.Hour)})

	frozen := newFreezer(cacheJobs, cacheActive)
	r := f.newReplica("replica", replicaOpts{
		limits:  limitsWith(func(l *config.PGOLimits) { l.MaxLiveCollections = 2 }),
		freezer: frozen,
	})
	r.waitSynced()
	r.waitCache("holds every override and the live service", func(c *Caches) bool {
		return len(c.overrideSnapshot()) == 4 && c.Live("payment", "s3-busy")
	})
	frozen.freeze()

	r.tick()

	want := map[string]int{slotLost: 1, slotWon: 1, slotBusy: 1, slotCapacity: 1}
	if got := r.recorder.scheduleSlots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("schedule slot rows are %v, want %v", got, want)
	}
}

// TestSchedulerTransitionLogs proves the publisher owns exactly the two
// transitions its writes produce, and that a refused Service produces none.
func TestSchedulerTransitionLogs(t *testing.T) {
	t.Run("a successful publication logs initializing then pending", func(t *testing.T) {
		f := startPGO(t)
		f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })
		r.tick()

		logged := r.logs.transitions()
		if len(logged) != 2 {
			t.Fatalf("one publication logged %d transitions, want 2", len(logged))
		}
		wantStates := []string{string(StateInitializing), string(StatePending)}
		for i, entry := range logged {
			if got := entry.Attrs["state"]; got != wantStates[i] {
				t.Errorf("transition %d is state %v, want %v", i, got, wantStates[i])
			}
			for key, want := range map[string]any{
				"namespace": "payment",
				"service":   "payment-api",
				"trigger":   string(OriginSchedule),
				"instance":  "replica",
				"attempt":   int64(0),
				"reason":    "",
			} {
				if got := entry.Attrs[key]; got != want {
					t.Errorf("transition %d %s is %v, want %v", i, key, got, want)
				}
			}
			id, _ := entry.Attrs["collection"].(string)
			if !ValidID(id) {
				t.Errorf("transition %d names collection %q, which is not an identifier", i, id)
			}
		}
	})

	t.Run("a refused service logs no transition", func(t *testing.T) {
		f := startPGO(t)
		f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
		f.seedLiveCollection("payment", "payment-api", StateRunning)
		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("sees the service live", func(c *Caches) bool {
			return len(c.overrideSnapshot()) == 1 && c.Live("payment", "payment-api")
		})
		r.tick()

		if logged := r.logs.transitions(); len(logged) != 0 {
			t.Fatalf("a refused service logged %d transitions, want none", len(logged))
		}
	})
}

// TestSchedulerReplayBarrier proves nothing on a replica decides from a cache
// that has not seen the bucket: a replacement publisher behind the barrier
// publishes nothing, and once its caches have replayed it counts what its
// predecessor left as live.
func TestSchedulerReplayBarrier(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
	// What a creator killed after its initializing create left behind: a
	// record no active key names, which only a nonterminal-record count sees.
	leftover := f.seedRecord("payment", "payment-api", StateInitializing)

	hook := &kvHook{}
	held := newArmedFreezer(cacheJobs, cacheActive)
	r := f.newReplica("replacement", replicaOpts{
		freezer:    held,
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

	for range 3 {
		r.tick()
	}
	if r.caches.Synced(r.client.Generation()) {
		t.Fatal("the caches reported synced while their replay was held")
	}
	if got := hook.operations(); len(got) != 0 {
		t.Fatalf("a tick behind the barrier issued %v, want no store operation", got)
	}
	if got := f.jobKeys(); len(got) != 1 {
		t.Fatalf("a tick behind the barrier left %d records, want only the leftover", len(got))
	}

	held.release()
	r.waitSynced()
	r.waitCache("counts the replayed record as live", func(c *Caches) bool { return c.Live("payment", "payment-api") })
	r.tick()
	if got := len(f.jobKeys()); got != 1 {
		t.Fatalf("a replayed nonterminal record did not refuse the service: %d records", got)
	}
	if got := r.recorder.scheduleSlots()[slotBusy]; got != 1 {
		t.Fatalf("recorded %v, want one busy row", r.recorder.scheduleSlots())
	}

	// The worker scan fails a stale initializing record and releases it; only
	// then does the Service become schedulable again.
	f.failRecord(leftover, ReasonNotPublished)
	r.waitCache("sees the service free again", func(c *Caches) bool { return !c.Live("payment", "payment-api") })
	r.tick()
	if got := len(f.jobKeys()); got != 2 {
		t.Fatalf("after the scan failed the leftover the scheduler left %d records, want 2", got)
	}
}

// TestSchedulerGenerationOutage proves the barrier follows the connection
// generation and not watch re-opening: state that changed during an outage is
// invisible until the caches have replayed under the new generation, and a
// tick that took its view before a disconnect writes nothing after it.
func TestSchedulerGenerationOutage(t *testing.T) {
	t.Run("held replay after an outage issues no store operation", func(t *testing.T) {
		f := startPGO(t)
		f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
		hook := &kvHook{}
		frozen := newFreezer(cacheOverrides, cacheJobs, cacheActive, cacheSlots)
		r := f.newReplica("replica", replicaOpts{
			freezer:    frozen,
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

		before := r.client.Generation()
		f.stopServer()
		waitFor(t, "the disconnect moved the generation", func() bool { return r.client.Generation() != before })
		frozen.freeze()
		f.restartServer()

		// Another replica's Collection and a changed override, none of which
		// this replica's held caches can see.
		f.seedLiveCollection("payment", "payment-api", StateRunning)
		f.setOverride("payment", "other-api", enabledOverride(withEvery(time.Hour), withJitter(0)))

		// The generation is read inside the predicate: a restart can produce a
		// second disconnect, and a value captured before the wait would never
		// be current again.
		waitFor(t, "the seam re-opened its watches", func() bool {
			return r.client.Synced(r.client.Generation())
		})
		gen := r.client.Generation()
		for range 3 {
			r.tick()
		}

		if !r.client.Synced(gen) {
			t.Fatal("the seam's own watches never re-synced, so the barrier was not the reason nothing happened")
		}
		if r.caches.Synced(gen) {
			t.Fatal("the caches reported synced while their replay was held")
		}
		if got := hook.operations(); len(got) != 0 {
			t.Fatalf("a tick behind the barrier issued %v, want no store operation", got)
		}
		frozen.release()
	})

	t.Run("a view taken before a disconnect writes nothing after it", func(t *testing.T) {
		f := startPGO(t)
		f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))

		var (
			hook    = &kvHook{}
			reached = make(chan struct{})
			resume  = make(chan struct{})
			once    sync.Once
		)
		hook.before = func(op, key string) (error, bool) {
			if op == "create" && strings.HasPrefix(key, slotPrefix) {
				once.Do(func() {
					close(reached)
					<-resume
				})
			}

			return nil, false
		}
		r := f.newReplica("replica", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

		done := make(chan struct{})
		go func() {
			defer close(done)
			r.tick()
		}()

		<-reached
		before := r.client.Generation()
		f.stopServer()
		waitFor(t, "the disconnect moved the generation", func() bool { return r.client.Generation() != before })
		f.restartServer()
		close(resume)
		<-done

		if got := f.keys(f.jobs, slotPrefix); len(got) != 0 {
			t.Errorf("slot keys are %v, want none: the view's generation had moved", got)
		}
		if got := f.jobKeys(); len(got) != 0 {
			t.Errorf("records are %v, want none", got)
		}
		if got := f.keys(f.jobs, activePrefix); len(got) != 0 {
			t.Errorf("active keys are %v, want none", got)
		}
	})
}

// TestSchedulerRun proves the loop ticks on its clock and stops with its context.
func TestSchedulerRun(t *testing.T) {
	f := startPGO(t)
	f.setOverride("payment", "payment-api", enabledOverride(withEvery(time.Hour), withJitter(0)))
	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the override", func(c *Caches) bool { return len(c.overrideSnapshot()) == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.sched.Run(ctx)
	}()

	waitFor(t, "the loop started its ticker", func() bool { return r.clock.tickerCount() > 0 })
	waitFor(t, "a tick published a collection", func() bool {
		r.clock.Advance(schedulerTick)

		return len(f.jobKeys()) == 1
	})

	cancel()
	select {
	case <-stopped:
	case <-time.After(fixtureTimeout):
		t.Fatal("Run did not return when its context ended")
	}
}
