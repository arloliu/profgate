package pgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
)

// schedulerTick is how often every replica considers every Service.
// A constant, not configuration: it is well under pgo.limits.minEvery, so the
// grain of the tick never decides whether a slot fires.
const schedulerTick = 10 * time.Second

// slotRetentionMargin is how long a slot key outlives its slot.
// every is at most pgo.limits.maxEvery, 24 hours, so by the time a key is
// deleted its slot ended at least a day earlier and no replica can attempt it
// again.
const slotRetentionMargin = 24 * time.Hour

// The results one scheduling attempt records.
const (
	slotWon      = "won"
	slotLost     = "lost"
	slotBusy     = "busy"
	slotCapacity = "capacity"
)

// slotFor is the start of the slot containing now for a period of every:
// floor(now / every) × every, aligned to the Unix epoch in UTC.
// Every replica computes the same slot from the same inputs.
func slotFor(now time.Time, every time.Duration) time.Time {
	period := every.Nanoseconds()
	if period <= 0 {
		return now.UTC()
	}
	n := now.UnixNano()
	rem := n % period
	if rem < 0 {
		rem += period
	}

	return time.Unix(0, n-rem).UTC()
}

// hashInput is what the jitter hash is taken over: "<ns>/<svc>/<slot>", with
// the slot as decimal Unix seconds in UTC, the same encoding the key uses.
// Two gateway versions that follow this contract contend on one key for one
// slot; an encoding left to the implementation could let two versions fire the
// same slot twice.
func hashInput(ns, svc string, slot time.Time) string {
	return fmt.Sprintf("%s/%s/%d", ns, svc, slot.Unix())
}

// offset is how far into its slot a Service fires: an FNV-1a 64 hash of the
// slot's inputs, reduced to the jitter window.
// It spreads Services across the interval without spreading replicas apart,
// because every replica derives it from the same three values.
func offset(ns, svc string, slot time.Time, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	h := fnv.New64a()
	//nolint:errcheck // hash.Hash.Write never returns an error
	_, _ = h.Write([]byte(hashInput(ns, svc, slot)))
	// The remainder is below jitter, which is a positive int64, so the
	// conversion cannot overflow.
	return time.Duration(int64(h.Sum64() % uint64(jitter))) //nolint:gosec
}

// Scheduler creates at most one Collection per Service per slot, on every
// replica, with no election: the slot key decides which replica wins, and the
// active key decides whether the Service may have another Collection at all.
type Scheduler struct {
	client   natskv.Client
	caches   *Caches
	pub      *Publisher
	defaults Policy
	limits   config.PGOLimits
	clock    Clock
	recorder metrics.Recorder
	log      *slog.Logger

	// loggedViolations remembers the override revision whose violations have
	// already been reported, so a Service that stays ineligible is logged once
	// per revision rather than every ten seconds.
	loggedViolations map[serviceRef]uint64
}

// NewScheduler returns the replica's scheduler.
func NewScheduler(
	client natskv.Client,
	caches *Caches,
	pub *Publisher,
	defaults Policy,
	limits config.PGOLimits,
	clock Clock,
	recorder metrics.Recorder,
	log *slog.Logger,
) *Scheduler {
	return &Scheduler{
		client:           client,
		caches:           caches,
		pub:              pub,
		defaults:         defaults,
		limits:           limits,
		clock:            clock,
		recorder:         recorder,
		log:              log,
		loggedViolations: make(map[serviceRef]uint64),
	}
}

// Run considers every Service every schedulerTick until ctx ends.
// Cancelling ctx stops it at the next tick; it needs no drain.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := s.clock.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			s.tick(ctx)
		}
	}
}

// tick is one pass over every Service with a stored override.
// It begins behind the replay barrier and uses one generation-bound view for
// the whole pass, so a disconnect ends the tick rather than letting it decide
// from caches that missed an outage.
func (s *Scheduler) tick(ctx context.Context) {
	gen := s.client.Generation()
	// The seam marks a watch synced when it forwards the replay marker into
	// its channel, and reports every watch synced when none is open at all, so
	// the runtime's barrier is the generation the caches themselves completed.
	if !s.client.Synced(gen) || !s.caches.Synced(gen) {
		return
	}
	stores, err := s.client.View(gen)
	if err != nil {
		return
	}
	jobs := stores.Jobs

	s.pub.ReleaseResolved(ctx, jobs)

	now := s.clock.Now().UTC()
	overrides := s.caches.overrideSnapshot()
	s.forgetStaleViolations(overrides)
	// In a stable order, so what a replica does when the ceiling refuses part
	// of a pass is reproducible rather than a property of map iteration.
	for _, ref := range sortedRefs(overrides) {
		s.consider(ctx, jobs, ref, overrides[ref], now)
	}
}

// sortedRefs orders the Services of one pass by namespace, then Service, so
// what a replica does when a ceiling refuses part of a pass is reproducible
// rather than a property of map iteration.
func sortedRefs[T any](refsOf map[serviceRef]T) []serviceRef {
	refs := make([]serviceRef, 0, len(refsOf))
	for ref := range refsOf {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b serviceRef) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}

		return strings.Compare(a.Service, b.Service)
	})

	return refs
}

// consider runs one Service through the eligibility checks and, when it is
// due, through the slot create and the publication.
func (s *Scheduler) consider(
	ctx context.Context, jobs natskv.KV, ref serviceRef, stored cachedOverride, now time.Time,
) {
	policy := Effective(s.defaults, stored.Stored.Policy)
	if !policy.Enabled {
		return
	}
	if violations := Validate(policy, s.limits); len(violations) > 0 {
		s.logViolations(ref, stored.Revision, violations)

		return
	}

	every := policy.Schedule.Every.Duration()
	slot := slotFor(now, every)
	if now.Before(slot.Add(offset(ref.Namespace, ref.Service, slot, policy.Schedule.Jitter.Duration()))) {
		return
	}

	// The cached check only spares a write that would lose; the active create
	// is the decision, and a replica whose cache lags simply loses it.
	if s.caches.Live(ref.Namespace, ref.Service) {
		s.recorder.ScheduleSlot(slotBusy)

		return
	}

	res, ok := s.pub.Reserve(ref.Namespace, ref.Service)
	if !ok {
		s.recorder.ScheduleSlot(slotCapacity)

		return
	}

	if !s.createSlot(ctx, jobs, ref, slot, every, res) {
		return
	}

	id, outcome, err := s.pub.Publish(ctx, jobs, res, PublishInput{
		Namespace:      ref.Namespace,
		Service:        ref.Service,
		Origin:         OriginSchedule,
		Trigger:        TriggerSchedule,
		Slot:           slot,
		ClaimBy:        now.Add(every),
		ConfigRevision: stored.Revision,
		Policy:         policy,
		CreatedBy:      createdBySchedule,
	})
	switch {
	case err != nil:
		// The slot is consumed and nothing runs for it.
		// A second create cannot know whether the first half-succeeded, so the
		// scan and the sweeper clear whatever it left and the next slot is at
		// most every away.
		s.log.Warn("pgo: publication after winning a slot failed",
			"namespace", ref.Namespace, "service", ref.Service, "collection", id, "error", err)
	case outcome == OutcomeBusy:
		s.recorder.ScheduleSlot(slotBusy)
	default:
		s.recorder.ScheduleSlot(slotWon)
	}
}

// createSlot writes the slot key and reports whether this replica won it.
// A lost or indeterminate create gives the reservation back, because no active
// key can exist yet for this attempt.
func (s *Scheduler) createSlot(
	ctx context.Context, jobs natskv.KV, ref serviceRef, slot time.Time, every time.Duration, res *Reservation,
) bool {
	value, err := json.Marshal(slotValue{RetainUntil: slot.Add(every + slotRetentionMargin)})
	if err != nil {
		res.Release()
		s.log.Warn("pgo: serialize slot key",
			"namespace", ref.Namespace, "service", ref.Service, "error", err)

		return false
	}

	_, err = jobs.Create(ctx, slotKey(ref.Namespace, ref.Service, slot), value)
	switch {
	case err == nil:
		return true
	case errors.Is(err, natskv.ErrKeyExists):
		res.Release()
		s.recorder.ScheduleSlot(slotLost)
	default:
		// Whether or not the key committed, this attempt writes nothing more.
		res.Release()
		s.log.Warn("pgo: slot create failed",
			"namespace", ref.Namespace, "service", ref.Service, "slot", slot.Format(time.RFC3339), "error", err)
	}

	return false
}

// logViolations reports the ceilings a stored override exceeds, once for each
// revision of that override.
func (s *Scheduler) logViolations(ref serviceRef, revision uint64, violations []Violation) {
	if logged, ok := s.loggedViolations[ref]; ok && logged == revision {
		return
	}
	s.loggedViolations[ref] = revision
	for _, v := range violations {
		s.log.Warn("pgo: policy exceeds a ceiling and the service is not scheduled",
			"namespace", ref.Namespace, "service", ref.Service, "revision", revision,
			"field", v.Field, "ceiling", v.Ceiling, "detail", v.Detail)
	}
}

// forgetStaleViolations drops what was remembered for overrides that are gone,
// so a deleted and recreated override reports its violations again.
func (s *Scheduler) forgetStaleViolations(overrides map[serviceRef]cachedOverride) {
	for ref := range s.loggedViolations {
		if _, ok := overrides[ref]; !ok {
			delete(s.loggedViolations, ref)
		}
	}
}
