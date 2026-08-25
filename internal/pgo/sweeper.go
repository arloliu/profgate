package pgo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
)

// sweeperTick is how often every replica runs one pass.
// A constant, not configuration.
const sweeperTick = 60 * time.Second

// orphanAge is how long an object or a preflight probe must have sat
// unreferenced before a pass removes it.
// It is what lets a slow Put finish before the completed update that names it.
// A constant, not configuration.
const orphanAge = 10 * time.Minute

// The names preflight leaves behind when it dies between a probe's create and
// its delete: a key in each KV bucket, an object in the artifact bucket.
const (
	probeKeyPrefix    = "probe."
	probeObjectPrefix = "probe-"
)

// artifactSuffix ends the name of every merged profile: <id>-<attempt>.pprof.
const artifactSuffix = ".pprof"

// The kinds profgate_sweeper_deletes_total carries.
const (
	sweepArtifact = "artifact"
	sweepRecord   = "record"
	sweepSlot     = "slot"
	sweepActive   = "active"
	sweepOrphan   = "orphan"
	sweepProbe    = "probe"
)

// Sweeper removes what Collections leave behind: expired artifacts, retired
// records, spent slot keys, objects no record names, active keys whose
// Collection has ended, and preflight probes a crash stranded.
// Every replica runs one; nothing elects it, and every write is conditional,
// so two replicas sweeping the same key cost one lost update and nothing else.
type Sweeper struct {
	client natskv.Client
	caches *Caches
	// jobRetention is pgo.jobRetention: how long a terminal record outlives
	// the Collection it describes.
	jobRetention time.Duration
	clock        Clock
	recorder     metrics.Recorder
	log          *slog.Logger
	owner        Owner
}

// NewSweeper returns the replica's sweeper.
func NewSweeper(
	client natskv.Client,
	caches *Caches,
	cfg config.PGOConfig,
	owner Owner,
	clock Clock,
	recorder metrics.Recorder,
	log *slog.Logger,
) *Sweeper {
	return &Sweeper{
		client:       client,
		caches:       caches,
		jobRetention: cfg.JobRetention,
		clock:        clock,
		recorder:     recorder,
		log:          log,
		owner:        owner,
	}
}

// Run sweeps every sweeperTick until ctx ends.
// Cancelling ctx stops it at the next tick; it needs no drain, because every
// pass is a sequence of independent conditional writes.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := s.clock.NewTicker(sweeperTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			s.sweep(ctx)
		}
	}
}

// sweep is one pass over everything the caches and one artifact listing show.
// It begins behind the replay barrier and uses one generation-bound view for
// the whole pass: a cache that has not finished replaying has an unknown gap
// behind it, and a sweeper that trusted it would delete live state.
func (s *Sweeper) sweep(ctx context.Context) {
	gen := s.client.Generation()
	if !s.client.Synced(gen) || !s.caches.Synced(gen) {
		return
	}
	stores, err := s.client.View(gen)
	if err != nil {
		return
	}

	now := s.clock.Now().UTC()
	jobs := s.caches.jobEntries()

	// One listing answers three conditions.
	// A listing that failed is not an empty bucket: without it a completed
	// record would look like one whose object has gone, so the rows that read
	// it are skipped for this pass rather than guessed at.
	objects, err := stores.Artifacts.List(ctx)
	listed := err == nil
	if !listed {
		s.log.Warn("pgo: listing the artifact bucket failed", "error", err)
	}

	s.expire(ctx, stores, jobs, objects, listed, now)
	s.retire(ctx, stores.Jobs, jobs, now)
	s.sweepSlots(ctx, stores.Jobs, now)
	if listed {
		s.sweepOrphans(ctx, stores, jobs, objects, now)
	}
	s.sweepActive(ctx, stores.Jobs)
	s.sweepProbes(ctx, stores, objects, listed, now)
}

// expire turns a completed Collection whose retention has run out, or whose
// object has gone, into an expired one.
//
// The object goes first and the record second, so a completed record always
// names an object that exists or one already on its way out.
// Objects.Delete of an absent name is success, and a pass that dies between the
// two leaves a completed record whose object has gone, which the second
// condition finishes on the next pass.
func (s *Sweeper) expire(
	ctx context.Context,
	stores natskv.Stores,
	jobs map[string]cachedJob,
	objects []natskv.ObjectInfo,
	listed bool,
	now time.Time,
) {
	stored := objectNames(objects)
	for _, key := range slices.Sorted(maps.Keys(jobs)) {
		job := jobs[key]
		if job.State != StateCompleted {
			continue
		}
		switch {
		case job.ExpiresAt != nil && job.ExpiresAt.Add(skewMargin).Before(now):
			if !s.dropArtifact(ctx, stores.Artifacts, job.Artifact) {
				continue
			}
		case listed && job.Artifact != "" && !stored[job.Artifact] &&
			s.objectGone(ctx, stores.Artifacts, job.Artifact):
			// The listing only nominates: an object put after it was taken
			// would be missing from it while the record that names it is
			// perfectly live, so the condition is confirmed against the store.
		default:
			continue
		}
		s.flipExpired(ctx, stores.Jobs, key, job)
	}
}

// dropArtifact removes the object an expiring record names and reports whether
// the record may now be flipped.
// A delete that failed leaves the record completed: flipping it first would
// leave an expired record naming an object nothing ever deletes, because the
// orphan rule keeps whatever a record names.
func (s *Sweeper) dropArtifact(ctx context.Context, artifacts natskv.Objects, object string) bool {
	if object == "" {
		return true
	}
	if err := artifacts.Delete(ctx, object); err != nil {
		s.log.Warn("pgo: deleting an expired artifact failed", "object", object, "error", err)

		return false
	}
	s.recorder.SweeperDelete(sweepArtifact)

	return true
}

// objectGone reports whether an object the listing did not show is really
// absent.
// An unavailable store answers no, so the record keeps its artifact.
func (s *Sweeper) objectGone(ctx context.Context, artifacts natskv.Objects, object string) bool {
	rc, err := artifacts.Get(ctx, object)
	if errors.Is(err, natskv.ErrObjectNotFound) {
		return true
	}
	if err == nil {
		//nolint:errcheck // the object is there either way; closing releases the read
		_ = rc.Close()
	}

	return false
}

// flipExpired writes the expired state at the revision the cache carries.
// The body comes from a fresh read because the record keeps everything but its
// state — a reader of an expired Collection still gets its manifest — and the
// conditional update is what decides: a record that moved since the cache saw
// it costs one lost update and nothing else.
func (s *Sweeper) flipExpired(ctx context.Context, jobs natskv.KV, key string, job cachedJob) {
	e, err := jobs.Get(ctx, key)
	if err != nil {
		return
	}
	var rec Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		s.log.Warn("pgo: collection record is not readable", "key", key, "error", err)

		return
	}
	if rec.State != StateCompleted {
		return
	}
	rec.State = StateExpired

	value, err := MarshalBounded(rec)
	if err != nil {
		s.log.Warn("pgo: serialize expired record", "collection", rec.ID, "error", err)

		return
	}
	if _, err := jobs.Update(ctx, key, value, job.Revision); err != nil {
		return
	}
	logTransition(s.log, s.owner.Instance, rec)
	s.recorder.Collection(string(StateExpired))
}

// retire deletes a terminal record once it has outlived pgo.jobRetention.
// A completed record is never deleted here: it becomes expired first, which
// deletes its object, and configuration validation keeps jobRetention above
// maxRetention, so a record always outlives its artifact.
func (s *Sweeper) retire(ctx context.Context, jobs natskv.KV, cached map[string]cachedJob, now time.Time) {
	for _, key := range slices.Sorted(maps.Keys(cached)) {
		job := cached[key]
		if !retirable(job.State) || job.FinishedAt == nil {
			continue
		}
		if !job.FinishedAt.Add(s.jobRetention + skewMargin).Before(now) {
			continue
		}
		if err := jobs.Delete(ctx, key, job.Revision); err != nil {
			continue
		}
		s.recorder.SweeperDelete(sweepRecord)
	}
}

// retirable reports whether a record is one the sweeper may delete outright.
func retirable(s State) bool {
	switch s {
	case StateExpired, StateFailed, StateCancelled:
		return true
	case StateInitializing, StatePending, StateRunning, StateCompleted:
		return false
	default:
		// An unknown state comes from a newer gateway version; leaving it in
		// place is the conservative reading.
		return false
	}
}

// sweepSlots deletes a slot key after the retainUntil its own value carries.
// The value decides, not the current policy: lowering every after the fact
// cannot shorten the retention of a key created under a longer one.
func (s *Sweeper) sweepSlots(ctx context.Context, jobs natskv.KV, now time.Time) {
	slots := s.caches.slotEntries()
	for _, key := range slices.Sorted(maps.Keys(slots)) {
		slot := slots[key]
		if !slot.RetainUntil.Add(skewMargin).Before(now) {
			continue
		}
		if err := jobs.Delete(ctx, key, slot.Revision); err != nil {
			continue
		}
		s.recorder.SweeperDelete(sweepSlot)
	}
}

// sweepOrphans removes the objects of attempts that lost and of Collections
// whose record is gone.
//
// The cache is a candidate filter and never the authority: a watch has no
// freshness bound, so after an outage a listing can arrive before the job
// watch has replayed a completed record, and a sweeper that trusted its cache
// would delete a live artifact.
// Every candidate therefore costs one fresh read of the Collection its name
// encodes, and only a record that is absent, or terminal and naming something
// else, gives the object up.
func (s *Sweeper) sweepOrphans(
	ctx context.Context,
	stores natskv.Stores,
	jobs map[string]cachedJob,
	objects []natskv.ObjectInfo,
	now time.Time,
) {
	named := namedArtifacts(jobs)
	for _, o := range sortedObjects(objects) {
		if named[o.Name] {
			continue
		}
		if !o.ModTime.Add(orphanAge + skewMargin).Before(now) {
			continue
		}
		id, ok := collectionOf(o.Name)
		if !ok {
			// A name this version does not write is left alone.
			continue
		}
		rec, gone, err := readJob(ctx, stores.Jobs, id)
		if err != nil {
			continue
		}
		if !gone && (!terminal(rec.State) || (rec.Artifact != nil && rec.Artifact.Object == o.Name)) {
			continue
		}
		if err := stores.Artifacts.Delete(ctx, o.Name); err != nil {
			s.log.Warn("pgo: deleting an orphaned artifact failed", "object", o.Name, "error", err)

			continue
		}
		s.recorder.SweeperDelete(sweepOrphan)
	}
}

// sweepActive releases a Service whose Collection has ended without releasing
// its own key, which is what a replica that died mid-transition leaves.
// The same freshness rule governs it: a key goes only after a fresh read shows
// its Collection terminal or gone.
// The record is created before the key, so a fresh read never finds "gone" for
// a key still being published; a key left by a creator that died is freed once
// the scan has failed its initializing record.
func (s *Sweeper) sweepActive(ctx context.Context, jobs natskv.KV) {
	active := s.caches.activeEntries()
	for _, ref := range sortedRefs(active) {
		a := active[ref]
		rec, gone, err := readJob(ctx, jobs, a.ID)
		if err != nil || (!gone && !terminal(rec.State)) {
			continue
		}
		if err := jobs.Delete(ctx, activeKey(ref.Namespace, ref.Service), a.Revision); err != nil {
			continue
		}
		s.recorder.SweeperDelete(sweepActive)
	}
}

// sweepProbes removes what a preflight that died between a probe's create and
// its delete left in each bucket.
// A probe belongs to no Collection, so age alone decides and nothing is looked
// up; a probe younger than orphanAge may belong to a preflight still running.
func (s *Sweeper) sweepProbes(
	ctx context.Context, stores natskv.Stores, objects []natskv.ObjectInfo, listed bool, now time.Time,
) {
	for _, kv := range []natskv.KV{stores.Config, stores.Jobs} {
		keys, err := kv.Keys(ctx, probeKeyPrefix)
		if err != nil {
			continue
		}
		slices.Sort(keys)
		for _, key := range keys {
			e, err := kv.Get(ctx, key)
			if err != nil {
				continue
			}
			if !e.Created.Add(orphanAge + skewMargin).Before(now) {
				continue
			}
			if err := kv.Delete(ctx, key, e.Revision); err != nil {
				continue
			}
			s.recorder.SweeperDelete(sweepProbe)
		}
	}

	if !listed {
		return
	}
	for _, o := range sortedObjects(objects) {
		if !strings.HasPrefix(o.Name, probeObjectPrefix) {
			continue
		}
		if !o.ModTime.Add(orphanAge + skewMargin).Before(now) {
			continue
		}
		if err := stores.Artifacts.Delete(ctx, o.Name); err != nil {
			continue
		}
		s.recorder.SweeperDelete(sweepProbe)
	}
}

// objectNames is the set of names one listing showed.
func objectNames(objects []natskv.ObjectInfo) map[string]bool {
	out := make(map[string]bool, len(objects))
	for _, o := range objects {
		out[o.Name] = true
	}

	return out
}

// namedArtifacts is the set of objects the cached completed records name.
func namedArtifacts(jobs map[string]cachedJob) map[string]bool {
	out := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		if job.State == StateCompleted && job.Artifact != "" {
			out[job.Artifact] = true
		}
	}

	return out
}

// sortedObjects orders a listing by name, so a pass that a ceiling or an
// outage cuts short is reproducible rather than a property of listing order.
func sortedObjects(objects []natskv.ObjectInfo) []natskv.ObjectInfo {
	out := slices.Clone(objects)
	slices.SortFunc(out, func(a, b natskv.ObjectInfo) int { return strings.Compare(a.Name, b.Name) })

	return out
}

// collectionOf takes the Collection out of an artifact name,
// <id>-<attempt>.pprof, and reports false for anything else.
func collectionOf(object string) (string, bool) {
	base, ok := strings.CutSuffix(object, artifactSuffix)
	if !ok {
		return "", false
	}
	id, attempt, ok := strings.Cut(base, "-")
	if !ok || attempt == "" || !ValidID(id) {
		return "", false
	}
	for _, r := range attempt {
		if r < '0' || r > '9' {
			return "", false
		}
	}

	return id, true
}
