package pgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
)

// skewMargin is how far apart two gateway clocks, and the NATS server's, are
// assumed to be at most.
// Every comparison between a timestamp one replica wrote and the time another
// reads carries it in the direction that makes the reader more conservative:
// a claimer waits this much longer, an owner gives up this much earlier.
// A constant, not configuration.
const skewMargin = 5 * time.Second

// publishGrace is how long an initializing record may sit before the scan
// gives up on the creator that left it.
const publishGrace = time.Minute

// renewCallTimeout is the longest a renewal waits for NATS;
// the committed lease shortens it further as it runs out.
const renewCallTimeout = 5 * time.Second

// progressBuffer is how many progress reports the owner loop queues.
// The work goroutine never blocks on it: the owner carries the latest value it
// has into its next renewal, and a dropped one is informational.
const progressBuffer = 8

// workInput is what the work goroutine gets: the claimed record, the Object
// Store of the view the claim ran under, and a way to report progress.
// It never touches KV.
type workInput struct {
	Record    Record
	Artifacts natskv.Objects
	// Progress reports how far the work has got. It never blocks.
	Progress func(Progress)
}

// workResult is what the work goroutine hands back for the owner loop to
// commit. Reason is empty on success and a failure reason otherwise.
type workResult struct {
	Reason          string
	Object          string
	Bytes           int64
	Manifest        *Manifest
	ResolvedVersion string
	Progress        Progress
}

// runFunc is the work goroutine's body: rounds, sampling, merging, and the
// Put. Everything that can block for long runs here rather than in the owner
// loop, so a long merge can never hold the renewal timer hostage.
type runFunc func(ctx context.Context, in workInput) workResult

// inFlight is one Collection this replica owns, for Drain to wait on.
// cutoff is the one absolute time an owner's authority ends at, committedLeaseUntil - skewMargin of the lease it last committed:
// a renewal moves it, the work is cancelled at it, the final update is gated on it, and the drain waits to it,
// so no two of them can disagree.
// install and cancel are the only writes, both under mu, so a renewal's result and a cancellation are ordered one way:
// a result installed first moves the cutoff the timer then re-reads,
// and a cancellation declared first refuses the result, which the owner then treats as a lease it cannot use.
// The work context is cancelled under the same lock, and cancelledCh is closed only once that call has returned,
// so whoever reads the channel finds the work cancelled rather than about to be.
type inFlight struct {
	id          string
	done        chan struct{} // closed when the owner loop exits
	cancelledCh chan struct{} // closed once cancel or abort has cancelled the work

	mu        sync.Mutex
	cutoff    time.Time
	cancelled bool
}

// newInFlight is the entry for a Collection claimed under a lease.
func newInFlight(id string, lease time.Time) *inFlight {
	return &inFlight{
		id:          id,
		done:        make(chan struct{}),
		cancelledCh: make(chan struct{}),
		cutoff:      lease.Add(-skewMargin),
	}
}

// install records the cutoff of a lease the owner has just committed, unless the work has been cancelled already.
func (fl *inFlight) install(cutoff time.Time) bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.cancelled {
		return false
	}
	fl.cutoff = cutoff

	return true
}

// cancel cancels the work through cancelWork when now is not before the cutoff, and reports whether this call did so.
// A timer that finds the cutoff moved re-arms for the new one.
func (fl *inFlight) cancel(now time.Time, cancelWork context.CancelFunc) bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.cancelled || now.Before(fl.cutoff) {
		return false
	}
	fl.cancelled = true
	cancelWork()
	close(fl.cancelledCh)

	return true
}

// abort cancels the work through cancelWork whatever the cutoff, for the lost record and the lapsed lease.
func (fl *inFlight) abort(cancelWork context.CancelFunc) bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.cancelled {
		return false
	}
	fl.cancelled = true
	cancelWork()
	close(fl.cancelledCh)

	return true
}

// state is the cutoff and whether the work was cancelled, read together.
func (fl *inFlight) state() (time.Time, bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()

	return fl.cutoff, fl.cancelled
}

// Worker claims Collections and owns them until they end.
// Every replica runs one; nothing elects it, and the record's revision decides
// every conflict.
type Worker struct {
	client   natskv.Client
	caches   *Caches
	limits   config.PGOLimits
	leaseTTL time.Duration
	// maxAttempts is pgo.maxAttempts: the claim that would exceed it fails the
	// record with attempts_exhausted instead.
	maxAttempts int
	// maxActive is pgo.limits.maxActiveCollections: this replica's own ceiling.
	maxActive int
	clock     Clock
	recorder  metrics.Recorder
	log       *slog.Logger
	owner     Owner
	run       runFunc
	// wrapCancel, when set, wraps the function that cancels a work context.
	// A test holds a cancellation open with it and checks what waits for the cancellation to complete.
	wrapCancel func(context.CancelFunc) context.CancelFunc

	// draining is closed by Drain and stops every owner loop renewing its lease,
	// so each owner ends at the cutoff of the lease it last committed rather than whenever its work finishes.
	draining chan struct{}

	mu     sync.Mutex
	active int
	// stopped is set by Drain, and refuses every claim after it:
	// the set of Collections the drain waits for never grows once it has
	// started, which is what lets the drain end.
	stopped  bool
	inFlight map[string]*inFlight
	// claims counts the claims between the capacity reservation and the
	// inFlight entry that reservation leads to.
	// Drain waits for it once no further claim can begin,
	// so a claim already inside that window when the drain started is waited
	// for rather than missed by a snapshot taken before it registered.
	claims sync.WaitGroup
}

// NewWorker returns the replica's worker.
// rounds is the work goroutine's body: everything that can block for long runs
// there rather than in the owner loop.
func NewWorker(
	client natskv.Client,
	caches *Caches,
	cfg config.PGOConfig,
	owner Owner,
	rounds *Rounds,
	clock Clock,
	recorder metrics.Recorder,
	log *slog.Logger,
) *Worker {
	return &Worker{
		client:      client,
		caches:      caches,
		limits:      cfg.Limits,
		leaseTTL:    cfg.LeaseTTL,
		maxAttempts: cfg.MaxAttempts,
		maxActive:   cfg.Limits.MaxActiveCollections,
		clock:       clock,
		recorder:    recorder,
		log:         log,
		owner:       owner,
		run:         rounds.run,
		draining:    make(chan struct{}),
		inFlight:    make(map[string]*inFlight),
	}
}

// Run scans and claims until ctx ends; ending ctx stops new claims and leaves
// the Collections this replica already owns to their owner loops.
// A pass runs on the timer and again whenever the job cache delivers, because
// time passing writes no KV revision: after an owner dies the last thing any
// watch delivered was a valid lease, and nothing else would ever revisit the
// record.
// Both entry points run the same pass; every termination in it is gated on a
// time that has already passed, so a delivery cannot end a record early.
func (w *Worker) Run(ctx context.Context) {
	ticker := w.clock.NewTicker(w.leaseTTL / 2)
	defer ticker.Stop()
	changes := w.caches.jobChanges()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			w.scan(ctx)
		case <-changes:
			w.scan(ctx)
		}
	}
}

// Drain stops every owner loop renewing its lease,
// and returns once each Collection this replica owns has committed or reached its cutoff.
// It refuses every later claim first and then waits for the claims already
// past their capacity check, because a claim whose conditional write is still
// in flight owns nothing a snapshot can see yet.
// A pass that waited for anything looks again before it returns,
// so a Collection registered while the pass was waiting is not left behind;
// the passes end because no claim can begin after the first line.
// Merge, Compact, and Write take no context and run to completion once entered,
// so a work goroutine may still be running when Drain returns.
// It commits nothing: its owner's lease has lapsed,
// and the next scan on another replica reclaims the record as a new attempt.
// The wait is therefore at most leaseTTL - skewMargin from each owner's last renewal,
// whatever that owner's work is still doing.
func (w *Worker) Drain(ctx context.Context) error {
	w.stopWork()
	w.claims.Wait()

	waited := make(map[string]struct{})
	for {
		fresh := false
		for _, fl := range w.inFlightSnapshot() {
			if _, seen := waited[fl.id]; seen {
				continue
			}
			waited[fl.id] = struct{}{}
			fresh = true
			if err := w.waitCollection(ctx, fl); err != nil {
				return err
			}
		}
		if !fresh {
			break
		}
	}

	return nil
}

// waitCollection waits for one Collection's owner loop to exit,
// or for its work to be cancelled at the cutoff of the lease that owner last committed.
// It re-reads the cutoff when its timer fires rather than trusting the one it armed for:
// a renewal already in the store when the drain began can still land and move it,
// and the owner is then waited for again, to the new cutoff.
// Once the cutoff has passed it waits for the cancellation itself,
// so it returns only once the work has been cancelled at the cutoff the entry holds then or the owner has committed.
func (w *Worker) waitCollection(ctx context.Context, fl *inFlight) error {
	for {
		// An owner that has already exited is never left behind,
		// however long an earlier one in this pass held the wait.
		select {
		case <-fl.done:
			return nil
		default:
		}
		cutoff, _ := fl.state()
		wait := cutoff.Sub(w.clock.Now())
		if wait <= 0 {
			break
		}
		timer := w.clock.NewTimer(wait)
		select {
		case <-fl.done:
			timer.Stop()

			return nil
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("pgo: drain: %w", ctx.Err())
		}
	}
	select {
	case <-fl.done:
		return nil
	case <-fl.cancelledCh:
		w.log.Warn("pgo: collection left for another replica at its lease cutoff", "collection", fl.id)

		return nil
	case <-ctx.Done():
		return fmt.Errorf("pgo: drain: %w", ctx.Err())
	}
}

// stopWork refuses every claim from here on,
// and stops every owner loop renewing its lease.
func (w *Worker) stopWork() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	close(w.draining)
}

// stopping reports whether the drain has begun.
func (w *Worker) stopping() bool {
	select {
	case <-w.draining:
		return true
	default:
		return false
	}
}

// inFlightSnapshot lists what this replica owns, in a stable order.
func (w *Worker) inFlightSnapshot() []*inFlight {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*inFlight, 0, len(w.inFlight))
	for _, fl := range w.inFlight {
		out = append(out, fl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })

	return out
}

// scan is one pass over the records this replica's cache shows as nonterminal.
// It begins behind the replay barrier and uses one view for the whole pass.
// Only the candidates due now are read fresh,
// so one pass costs the store what is due rather than what is stored.
// Whether a slot is free is read once per pass and not per candidate:
// a claim inside the pass changes it, and the claim's own reserveLocalSlot is what refuses the next.
func (w *Worker) scan(ctx context.Context) {
	gen := w.client.Generation()
	if !w.client.Synced(gen) || !w.caches.Synced(gen) {
		return
	}
	stores, err := w.client.View(gen)
	if err != nil {
		return
	}
	now := w.clock.Now().UTC()
	w.mu.Lock()
	slotFree := w.active < w.maxActive
	w.mu.Unlock()
	for _, c := range w.caches.nonterminalJobs() {
		if !due(c.job, now, slotFree) {
			continue
		}
		w.visit(ctx, stores, c.id)
	}
}

// due reports whether a cached record may need acting on now,
// so a pass reads fresh only the records it may act on and one delivery costs the store what is due, not what is stored.
// A cached lease is never later than the store's, because a renewal only extends it and the cache lags the store,
// so lag can make a pass read a record it need not and never skip one whose lease has lapsed;
// the fresh read still precedes every write, and the write alone decides.
func due(c cachedJob, now time.Time, slotFree bool) bool {
	switch c.State {
	case StateInitializing:
		return c.CreatedAt.Add(publishGrace + skewMargin).Before(now)
	case StatePending:
		return slotFree || c.ClaimBy.Add(skewMargin).Before(now)
	case StateRunning:
		return (c.LeaseUntil != nil && c.LeaseUntil.Add(skewMargin).Before(now)) ||
			(c.Deadline != nil && c.Deadline.Add(skewMargin).Before(now))
	case StateCompleted, StateFailed, StateCancelled, StateExpired:
		return false
	default:
		return false
	}
}

// visit reads one candidate fresh and does whatever its state and the clock
// call for. The Get is what makes the revision the replica compares against
// the most recent one; it is not a pre-check, and the Update alone decides.
func (w *Worker) visit(ctx context.Context, stores natskv.Stores, id string) {
	jobs := stores.Jobs
	e, err := jobs.Get(ctx, jobKey(id))
	if err != nil {
		return
	}
	var rec Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		w.log.Warn("pgo: collection record is not readable", "collection", id, "error", err)

		return
	}

	now := w.clock.Now().UTC()
	switch {
	case rec.State == StateInitializing && rec.CreatedAt.Add(publishGrace+skewMargin).Before(now):
		w.terminate(ctx, jobs, rec, e.Revision, ReasonNotPublished)
	case rec.State == StatePending && rec.ClaimBy.Add(skewMargin).Before(now):
		w.terminate(ctx, jobs, rec, e.Revision, ReasonNotClaimed)
	case rec.State == StateRunning && rec.Deadline != nil && rec.Deadline.Add(skewMargin).Before(now):
		// A wedged owner is failed rather than reclaimed: it has not finished
		// inside a deadline computed from the ceilings, and its own next
		// renewal observes the mismatch and stops.
		w.terminate(ctx, jobs, rec, e.Revision, ReasonDeadlineExceeded)
	case claimable(rec, now):
		w.claim(ctx, stores, rec, e.Revision)
	}
}

// claimable reports whether a record is free for the taking.
func claimable(rec Record, now time.Time) bool {
	switch rec.State {
	case StatePending:
		return true
	case StateRunning:
		return rec.LeaseUntil != nil && rec.LeaseUntil.Add(skewMargin).Before(now)
	case StateInitializing, StateCompleted, StateFailed, StateCancelled, StateExpired:
		return false
	default:
		return false
	}
}

// terminate ends a Collection this replica does not own, at the revision it
// just read, and releases the Service on success.
// The loser of the conditional update is done: whoever won it owns the record.
func (w *Worker) terminate(ctx context.Context, jobs natskv.KV, rec Record, rev uint64, reason string) {
	now := w.clock.Now().UTC()
	rec.State = StateFailed
	rec.Reason = reason
	rec.FinishedAt = &now

	value, err := MarshalBounded(rec)
	if err != nil {
		w.log.Warn("pgo: serialize terminal record", "collection", rec.ID, "error", err)

		return
	}
	if _, err := jobs.Update(ctx, jobKey(rec.ID), value, rev); err != nil {
		return
	}
	logTransition(w.log, w.owner.Instance, rec)
	w.recorder.Collection(string(StateFailed))
	releaseActive(ctx, jobs, rec)
}

// claim takes ownership of a claimable record and starts its owner loop.
// Work starts only on a successful Update with the revision that Update
// returned: a worker that ran without one would either profile without owning
// the record or own it without knowing the revision every later conditional
// write needs.
func (w *Worker) claim(ctx context.Context, stores natskv.Stores, rec Record, rev uint64) {
	jobs := stores.Jobs

	// A record carries the policy snapshot it was created under, and the
	// ceilings that validated it then are not this replica's ceilings now.
	// The check comes before any local capacity is reserved, so such a record
	// never reserves a slot and never samples.
	if violations := Validate(rec.Policy, w.limits); len(violations) > 0 {
		for _, v := range violations {
			w.log.Warn("pgo: collection policy exceeds a ceiling of this replica",
				"collection", rec.ID, "field", v.Field, "ceiling", v.Ceiling, "detail", v.Detail)
		}
		w.terminate(ctx, jobs, rec, rev, ReasonLimitExceeded)

		return
	}

	if !w.reserveLocalSlot() {
		return
	}
	// The claim window closes when this call returns: by then the record is
	// either registered for the drain to wait on or the slot is back.
	defer w.claims.Done()
	if rec.Attempt >= w.maxAttempts {
		w.releaseLocalSlot()
		w.terminate(ctx, jobs, rec, rev, ReasonAttemptsExhausted)

		return
	}

	now := w.clock.Now().UTC()
	lease := now.Add(w.leaseTTL)
	rec.State = StateRunning
	rec.Attempt++
	owner := w.owner
	rec.Owner = &owner
	rec.LeaseUntil = &lease
	if rec.StartedAt == nil {
		started := now
		deadline := Deadline(now, rec.Policy, w.limits)
		rec.StartedAt = &started
		rec.Deadline = &deadline
	}

	value, err := MarshalBounded(rec)
	if err != nil {
		w.releaseLocalSlot()
		w.log.Warn("pgo: serialize claim", "collection", rec.ID, "error", err)

		return
	}
	newRev, err := jobs.Update(ctx, jobKey(rec.ID), value, rev)
	if err != nil {
		// ErrRevisionMismatch: another replica moved it.
		// ErrUnavailable: the outcome is unknown, and a claim that did commit
		// carries a lease this replica will never renew, so another scan
		// reclaims it after leaseTTL + skewMargin as it would after a crash.
		w.releaseLocalSlot()

		return
	}

	logTransition(w.log, w.owner.Instance, rec)
	w.recorder.CollectionsActive(1)
	w.startOwner(stores, rec, newRev)
}

// reserveLocalSlot takes one of this replica's maxActiveCollections, and
// opens the claim window Drain waits for.
// A drained worker reserves nothing: the Collection would have no one left to
// finish it.
func (w *Worker) reserveLocalSlot() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.active >= w.maxActive {
		return false
	}
	w.active++
	w.claims.Add(1)

	return true
}

// releaseLocalSlot gives one back.
func (w *Worker) releaseLocalSlot() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active--
}

// startOwner runs one Collection's owner loop.
// The local slot and the memory it stands for stay held until the work
// goroutine has exited, so a replacement Collection on this replica waits.
func (w *Worker) startOwner(stores natskv.Stores, rec Record, rev uint64) {
	fl := newInFlight(rec.ID, *rec.LeaseUntil)
	w.mu.Lock()
	w.inFlight[rec.ID] = fl
	w.mu.Unlock()

	go func() {
		defer close(fl.done)
		defer func() {
			// A replica can hold two owners for one identifier in turn:
			// one that aborted at its cutoff and is still draining its work,
			// and the one its own scan started by reclaiming the record.
			// An owner removes only the entry it registered;
			// a successor's entry is the drain's to wait for.
			w.mu.Lock()
			if w.inFlight[rec.ID] == fl {
				delete(w.inFlight, rec.ID)
			}
			w.mu.Unlock()
			// The gauge falls before the slot does, so an observer that sees
			// the slot free sees the gauge already down.
			w.recorder.CollectionsActive(-1)
			w.releaseLocalSlot()
		}()
		w.ownerLoop(fl, stores, rec, rev)
	}()
}

// ownerLoop is the one goroutine that writes to a Collection's record after
// the claim. It holds the current revision and the committed lease, the value
// the last successful Update stored, so renewal and finish are serialized by
// construction and the owner can never lose a conditional update to its own
// newer revision.
func (w *Worker) ownerLoop(fl *inFlight, stores natskv.Stores, entry Record, rev uint64) {
	jobs := stores.Jobs
	committed := *entry.LeaseUntil
	deadline := *entry.Deadline

	workCtx, cancelWork := context.WithCancel(context.Background())
	if w.wrapCancel != nil {
		cancelWork = w.wrapCancel(cancelWork)
	}
	defer cancelWork()

	progressCh := make(chan Progress, progressBuffer)
	results := make(chan workResult, 1)
	// The work goroutine gets the record as it was claimed and nothing later:
	// the owner alone owns entry from here on, and it edits it on every renewal.
	input := workInput{
		Record:    entry,
		Artifacts: stores.Artifacts,
		Progress: func(p Progress) {
			select {
			case progressCh <- p:
			default:
			}
		},
	}
	go func() {
		results <- w.run(workCtx, input)
	}()

	cutoffDone := make(chan struct{})
	defer close(cutoffDone)
	go w.runCutoff(fl, cancelWork, cutoffDone)

	ticker := w.clock.NewTicker(w.leaseTTL / 3)
	defer ticker.Stop()

	aborted := false
	current := entry.Progress
	for {
		select {
		case p := <-progressCh:
			current = p
		case res := <-results:
			if aborted {
				// A late result is rejected whatever state the work was in.
				w.discardArtifact(stores.Artifacts, res)

				return
			}
			w.finish(fl, stores, entry, rev, deadline, res)

			return
		case <-ticker.C():
			if aborted {
				continue
			}
			if w.stopping() {
				// The drain stopped renewing.
				// This owner commits if its work returns inside the lease it last committed,
				// and writes nothing if it does not.
				continue
			}
			newRev, lease, err := w.renew(jobs, entry, rev, current, committed)
			switch {
			case err == nil:
				if !fl.install(lease.Add(-skewMargin)) {
					// The work was cancelled at the cutoff while this renewal was in the store.
					// The lease it committed is one this owner cannot use, as a lapsed one is;
					// the owner writes nothing more, and another replica reclaims the record once that lease lapses.
					aborted = true
					fl.abort(cancelWork)
					w.log.Warn("pgo: collection lease renewed after its work was cancelled at the cutoff",
						"collection", entry.ID, "instance", w.owner.Instance)

					continue
				}
				rev = newRev
				entry.LeaseUntil = &lease
				entry.Progress = current
				committed = lease
			case errors.Is(err, natskv.ErrRevisionMismatch):
				// Cancelled by the API, or reclaimed after a stall: the work
				// context is cancelled at once, without waiting for the cutoff.
				aborted = true
				fl.abort(cancelWork)
				w.logLostRecord(jobs, entry)
			case errors.Is(err, errLeaseLapsed):
				aborted = true
				fl.abort(cancelWork)
				w.log.Warn("pgo: collection lease lapsed", "collection", entry.ID, "instance", w.owner.Instance)
			default:
				// ErrUnavailable changes nothing; the next tick tries again
				// with what remains of the committed lease.
				w.log.Warn("pgo: lease renewal failed", "collection", entry.ID, "error", err)
			}
		}
	}
}

// errLeaseLapsed reports that there is no lease left to renew inside.
var errLeaseLapsed = errors.New("lease lapsed")

// renew proposes a new lease and commits it only when the Update succeeds.
// The proposed lease lives in a local copy until then, so the cutoff the owner
// enforces is always a lease another replica can also see.
// The call deadline shrinks as the committed lease runs out, so a renewal
// blocked by a slow NATS can never return success after the lease it was
// renewing has lapsed.
func (w *Worker) renew(
	jobs natskv.KV, entry Record, rev uint64, current Progress, committed time.Time,
) (uint64, time.Time, error) {
	now := w.clock.Now().UTC()
	callDeadline := min(renewCallTimeout, committed.Sub(now)-skewMargin)
	if callDeadline <= 0 {
		return 0, time.Time{}, errLeaseLapsed
	}

	proposed := entry
	lease := now.Add(w.leaseTTL)
	proposed.LeaseUntil = &lease
	proposed.Progress = current

	value, err := MarshalBounded(proposed)
	if err != nil {
		return 0, time.Time{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	defer cancel()
	newRev, err := jobs.Update(ctx, jobKey(entry.ID), value, rev)
	if err != nil {
		return 0, time.Time{}, err
	}

	return newRev, lease, nil
}

// finish issues the one final update,
// gated on the entry's cutoff and cancellation, the deadline, and the state the owner alone can read.
// The cutoff and deadline are re-checked at the last moment
// because the Put may have taken long enough for a scan elsewhere to have lawfully reclaimed the record;
// an owner whose lease has lapsed by then has nothing to commit and removes what it wrote.
// The cutoff is the entry's, the value the drain reads, so its gate and the drain's read one value.
func (w *Worker) finish(
	fl *inFlight, stores natskv.Stores, entry Record, rev uint64, deadline time.Time, res workResult,
) {
	now := w.clock.Now().UTC()
	cutoff, cancelled := fl.state()
	if cancelled || !now.Before(cutoff) || !now.Before(deadline.Add(-skewMargin)) {
		w.discardArtifact(stores.Artifacts, res)
		w.log.Warn("pgo: final update skipped, the lease or the deadline had passed",
			"collection", entry.ID, "instance", w.owner.Instance)

		return
	}

	proposed := entry
	proposed.FinishedAt = &now
	proposed.Progress = res.Progress
	if res.ResolvedVersion != "" {
		proposed.ResolvedVersion = res.ResolvedVersion
	}
	proposed.Manifest = res.Manifest
	if res.Reason == "" {
		expires := now.Add(entry.Policy.Artifact.Retention.Duration())
		proposed.State = StateCompleted
		proposed.Artifact = &ArtifactRef{Object: res.Object, Bytes: res.Bytes}
		proposed.ExpiresAt = &expires
	} else {
		proposed.State = StateFailed
		proposed.Reason = res.Reason
	}

	value, err := MarshalBounded(proposed)
	if errors.Is(err, ErrRecordTooLarge) {
		// The object goes first, so the terminal record names nothing that is
		// about to be garbage, and the smaller record's own Update cannot fail
		// for the same reason.
		w.discardArtifact(stores.Artifacts, res)
		proposed = proposed.terminalTooLarge(now)
		value, err = MarshalBounded(proposed)
	}
	if err != nil {
		w.log.Warn("pgo: serialize final record", "collection", entry.ID, "error", err)

		return
	}

	if _, err := stores.Jobs.Update(context.Background(), jobKey(entry.ID), value, rev); err != nil {
		// A lost final update records nothing; its object is this attempt's
		// alone, so deleting it cannot touch the winner's.
		w.discardArtifact(stores.Artifacts, res)

		return
	}

	logTransition(w.log, w.owner.Instance, proposed)
	w.recorder.Collection(string(proposed.State))
	if proposed.State == StateCompleted && proposed.StartedAt != nil {
		w.recorder.CollectionDuration(now.Sub(*proposed.StartedAt))
	}
	releaseActive(context.Background(), stores.Jobs, proposed)
}

// discardArtifact removes the object this attempt wrote, and only that one.
// The name carries the attempt, so a stale owner can never delete the bytes a
// reclaimed attempt committed.
func (w *Worker) discardArtifact(artifacts natskv.Objects, res workResult) {
	if res.Object == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), renewCallTimeout)
	defer cancel()
	//nolint:errcheck // best effort; the sweeper removes an object no record names
	_ = artifacts.Delete(ctx, res.Object)
}

// logLostRecord re-reads a record once, to say why the owner stopped.
func (w *Worker) logLostRecord(kv natskv.KV, entry Record) {
	ctx, cancel := context.WithTimeout(context.Background(), renewCallTimeout)
	defer cancel()
	e, err := kv.Get(ctx, jobKey(entry.ID))
	if err != nil {
		w.log.Warn("pgo: collection moved under its owner", "collection", entry.ID, "error", err)

		return
	}
	var rec Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		w.log.Warn("pgo: collection moved under its owner", "collection", entry.ID, "error", err)

		return
	}
	w.log.Info("pgo: collection moved under its owner",
		"collection", entry.ID, "state", string(rec.State), "reason", rec.Reason, "instance", w.owner.Instance)
}

// runCutoff cancels the work when the committed lease is about to lapse, whatever the owner loop is doing.
// It reads the entry's cutoff when its timer fires rather than being told a new one:
// a renewal moves the value, and a timer that finds it moved re-arms for the new one,
// so the work is cancelled at the cutoff the entry holds then and never at an older one.
// It returns once it has cancelled the work, once the owner loop has closed done,
// or once its timer fires on an entry the owner loop aborted itself.
func (w *Worker) runCutoff(fl *inFlight, cancelWork context.CancelFunc, done <-chan struct{}) {
	for {
		cutoff, cancelled := fl.state()
		if cancelled {
			return
		}
		timer := w.clock.NewTimer(cutoff.Sub(w.clock.Now()))
		select {
		case <-done:
			timer.Stop()

			return
		case <-timer.C():
			if fl.cancel(w.clock.Now(), cancelWork) {
				return
			}
		}
	}
}
