package pgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/arloliu/profgate/internal/natskv"
)

// ErrCapacityExhausted is what Reserve refuses with at the live-Collection ceiling.
// The scheduler counts the refusal against the slot, and the on-demand route answers 429 capacity_exhausted.
var ErrCapacityExhausted = errors.New("live collection ceiling reached")

// createdBySchedule is the createdBy of a scheduled Collection.
// A scheduled Collection has no requesting principal, and the field is a
// principal everywhere else, so it names the scheduler rather than borrowing
// an identity that did not ask for it.
const createdBySchedule = "schedule"

// Outcome is how one publication ended.
type Outcome string

// The two outcomes a publication reports without an error.
const (
	// OutcomeWon means the record exists as pending and this replica owns
	// the Service's live Collection.
	OutcomeWon Outcome = "won"
	// OutcomeBusy means the active create lost: the Service already has a
	// live Collection, whatever its origin.
	OutcomeBusy Outcome = "busy"
)

// PublishInput is one publication's inputs, the same for both creators.
type PublishInput struct {
	Namespace string
	Service   string
	Origin    Origin
	// Slot is the slot that created it; the zero time for an api Collection.
	Slot time.Time
	// ClaimBy is now + every for a scheduled Collection, now + 1h for an api one.
	ClaimBy time.Time
	// ConfigRevision is the revision of service.<ns>.<svc> the snapshot came
	// from, or 0 when no override existed.
	ConfigRevision uint64
	// Policy is the effective policy snapshot the Collection runs with.
	Policy Policy
	// CreatedBy is the requesting principal, or createdBySchedule.
	// It is also the principal the receipt is scoped to.
	CreatedBy string
	// IdempotencyKey is the Idempotency-Key the request carried,
	// and empty for a scheduled Collection, which writes no receipt.
	IdempotencyKey string
}

// Reservation is one replica's claim on a slot of the live-Collection ceiling,
// held from before the first write of a publication until the release rule
// resolves it.
type Reservation struct {
	p   *Publisher
	ref serviceRef
	// id is empty until Track binds the reservation to a published Collection.
	id string
	// done is set once the reservation has been given back, so a double
	// release cannot drive the counter negative.
	done bool
}

// Publisher performs the three writes that publish a Collection and holds the
// reservation counter of the live-Collection ceiling.
// One per replica, shared by the scheduler and POST /collections: the ceiling
// is per replica, so a second counter would let a replica exceed it.
type Publisher struct {
	caches   *Caches
	clock    Clock
	log      *slog.Logger
	instance string
	// maxLive is pgo.limits.maxLiveCollections.
	maxLive int

	mu sync.Mutex
	// held are the reservations neither observation of the release rule has
	// resolved yet, in the order they were taken.
	held []*Reservation
}

// NewPublisher returns the replica's publisher.
func NewPublisher(caches *Caches, clock Clock, maxLive int, instance string, log *slog.Logger) *Publisher {
	return &Publisher{
		caches:   caches,
		clock:    clock,
		log:      log,
		instance: instance,
		maxLive:  maxLive,
	}
}

// Reserve takes one reservation against the live-Collection ceiling, counting
// the Services the caches show as live plus this replica's publications the
// caches have not delivered anything for yet.
// At or above the ceiling it refuses with ErrCapacityExhausted and writes nothing.
// A Service the caches already show as live is refused by the caller, without
// a reservation; Reserve measures the cluster, not the Service.
// The count is a cache read and takes the caller's generation with it,
// so a caller the caches have moved past is told the count cannot be made rather than refused for want of capacity.
func (p *Publisher) Reserve(gen uint64, ns, svc string) (*Reservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	live, ok := p.caches.cachedLive(gen)
	if !ok {
		return nil, fmt.Errorf("pgo: the caches have moved past generation %d: %w", gen, natskv.ErrUnavailable)
	}
	if live+len(p.held) >= p.maxLive {
		return nil, ErrCapacityExhausted
	}
	r := &Reservation{p: p, ref: serviceRef{Namespace: ns, Service: svc}}
	p.held = append(p.held, r)

	return r, nil
}

// Track binds the reservation to the Collection its publication created.
// From then on only the release rule gives it back, because the record or the
// active key may exist even when the write that made it reported nothing.
func (r *Reservation) Track(id string) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.id = id
}

// Release gives a reservation back at once.
// Only a publication that failed before any write that could have created
// state may call it: a refused reservation, a job create that returned a
// definite error, or a lost active create whose own record it deleted with a
// definite result.
func (r *Reservation) Release() {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.releaseLocked(r)
}

// releaseLocked drops one reservation from the held list, called with p.mu held.
func (p *Publisher) releaseLocked(r *Reservation) {
	if r.done {
		return
	}
	r.done = true
	for i, held := range p.held {
		if held == r {
			p.held = append(p.held[:i], p.held[i+1:]...)

			return
		}
	}
}

// Reserved is the number of reservations this replica currently holds.
func (p *Publisher) Reserved() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.held)
}

// ReleaseResolved evaluates the release rule for every tracked reservation,
// cache first and then authoritative reads, and is called on every scheduler
// tick.
// A reservation is released as soon as either a watched cache delivers its
// job.<id> in any state or its active.<ns>.<svc> key holding that id — from
// then on the Service is counted by the caches — or authoritative reads show
// the record absent or terminal and the active key absent or holding another
// id, so nothing of the publication exists to count.
// ErrUnavailable on either read leaves the reservation held, and nothing else
// releases one; in particular claimBy passing does not.
func (p *Publisher) ReleaseResolved(ctx context.Context, jobs natskv.KV) {
	type tracked struct {
		res *Reservation
		id  string
		ref serviceRef
	}

	p.mu.Lock()
	pending := make([]tracked, 0, len(p.held))
	for _, r := range p.held {
		if r.id != "" {
			pending = append(pending, tracked{res: r, id: r.id, ref: r.ref})
		}
	}
	p.mu.Unlock()

	for _, t := range pending {
		if p.resolved(ctx, jobs, t.id, t.ref) {
			p.mu.Lock()
			p.releaseLocked(t.res)
			p.mu.Unlock()
		}
	}
}

// resolved reports whether one tracked reservation has been observed to be
// counted elsewhere or to have left nothing behind.
func (p *Publisher) resolved(ctx context.Context, jobs natskv.KV, id string, ref serviceRef) bool {
	if p.caches.hasJob(id) {
		return true
	}
	if cached, ok := p.caches.activeID(ref.Namespace, ref.Service); ok && cached == id {
		return true
	}

	rec, gone, err := readJob(ctx, jobs, id)
	if err != nil || (!gone && !terminal(rec.State)) {
		return false
	}

	e, err := jobs.Get(ctx, activeKey(ref.Namespace, ref.Service))
	switch {
	case errors.Is(err, natskv.ErrKeyNotFound):
		return true
	case err != nil:
		return false
	}
	var v activeValue
	if err := json.Unmarshal(e.Value, &v); err != nil {
		p.log.Warn("pgo: active key is not readable",
			"namespace", ref.Namespace, "service", ref.Service, "error", err)

		return false
	}

	return v.ID != id
}

// Publish performs the writes of one publication in the order that closes the publication race:
// Create job.<id> as initializing, Create active.<ns>.<svc>,
// Create the idempotency receipt when the request carried a key,
// then Update the record to pending.
// The active key therefore never exists without its record,
// so a sweeper that reads the job an active key names finds initializing or later, never nothing.
// A keyed record becomes claimable only after the store has acknowledged the receipt that binds it,
// so no Collection a caller can poll is unbound.
// The caller holds res, and Publish gives it back only where nothing can exist.
//
// OutcomeBusy means the Service already had a live Collection.
// An error means the publication did not complete: either nothing was written,
// or a write is indeterminate and the reservation stays tracked for the release
// rule, the scan, and the sweeper to resolve.
// Publish never retries a write, because a second create cannot know whether
// the first half-succeeded.
func (p *Publisher) Publish(
	ctx context.Context, jobs natskv.KV, res *Reservation, in PublishInput,
) (id string, outcome Outcome, err error) {
	id = newID()
	now := p.clock.Now().UTC()
	rec := p.record(id, now, in)

	value, err := MarshalBounded(rec)
	if err != nil {
		res.Release()

		return id, "", err
	}

	rev, err := jobs.Create(ctx, jobKey(id), value)
	switch {
	case errors.Is(err, natskv.ErrUnavailable):
		// Indeterminate: the record may exist without its acknowledgement.
		res.Track(id)

		return id, "", fmt.Errorf("pgo: create record %s: %w", id, err)
	case err != nil:
		res.Release()

		return id, "", fmt.Errorf("pgo: create record %s: %w", id, err)
	}
	logTransition(p.log, p.instance, rec, slog.String("trigger", string(rec.Origin)))

	activeRev, activeErr := p.createActive(ctx, jobs, id, now, in)
	if errors.Is(activeErr, natskv.ErrKeyExists) {
		p.discardLostRecord(ctx, jobs, res, id, rev)

		return id, OutcomeBusy, nil
	}

	// Past the lost branch the record is this replica's to account for,
	// whether or not the active create was acknowledged.
	res.Track(id)
	if activeErr != nil {
		return id, "", fmt.Errorf("pgo: create active key for %s/%s: %w", in.Namespace, in.Service, activeErr)
	}

	if in.IdempotencyKey != "" {
		if rerr := p.createReceipt(ctx, jobs, id, now, rec.SnapshotHash, in); rerr != nil {
			p.withdraw(ctx, jobs, id, rev, activeRev, in)

			return id, "", rerr
		}
	}

	rec.State = StatePending
	pending, err := MarshalBounded(rec)
	if err != nil {
		return id, "", err
	}
	if _, err := jobs.Update(ctx, jobKey(id), pending, rev); err != nil {
		return id, "", fmt.Errorf("pgo: publish record %s: %w", id, err)
	}
	logTransition(p.log, p.instance, rec, slog.String("trigger", string(rec.Origin)))

	return id, OutcomeWon, nil
}

// record builds the initializing record one publication creates.
func (p *Publisher) record(id string, now time.Time, in PublishInput) Record {
	rec := Record{
		ID:             id,
		Namespace:      in.Namespace,
		Service:        in.Service,
		Origin:         in.Origin,
		ConfigRevision: in.ConfigRevision,
		Policy:         in.Policy,
		State:          StateInitializing,
		ClaimBy:        in.ClaimBy.UTC(),
		CreatedBy:      in.CreatedBy,
		CreatedAt:      now,
		IdempotencyKey: in.IdempotencyKey,
		SnapshotHash:   SnapshotHash(in.Policy),
	}
	if !in.Slot.IsZero() {
		rec.Slot = in.Slot.UTC().Format(time.RFC3339)
	}

	return rec
}

// createActive writes active.<ns>.<svc> naming this Collection and returns the
// revision it was created at, which is the revision a withdraw deletes it at.
func (p *Publisher) createActive(
	ctx context.Context, jobs natskv.KV, id string, now time.Time, in PublishInput,
) (uint64, error) {
	value, err := json.Marshal(activeValue{ID: id, CreatedAt: now})
	if err != nil {
		return 0, fmt.Errorf("serialize active key: %w", err)
	}

	return jobs.Create(ctx, activeKey(in.Namespace, in.Service), value)
}

// discardLostRecord deletes the initializing record of a publication whose
// active create lost, at the revision the create returned.
// A definite result — deleted, or already gone — means nothing of this
// publication exists and the reservation goes back at once;
// ErrUnavailable leaves the record possibly still there, so the reservation
// stays tracked and the worker scan fails the record with not_published.
func (p *Publisher) discardLostRecord(
	ctx context.Context, jobs natskv.KV, res *Reservation, id string, rev uint64,
) {
	err := jobs.Delete(ctx, jobKey(id), rev)
	if errors.Is(err, natskv.ErrUnavailable) {
		res.Track(id)
		p.log.Warn("pgo: discard of a lost collection record is indeterminate", "collection", id, "error", err)

		return
	}
	if err != nil && !errors.Is(err, natskv.ErrKeyNotFound) && !errors.Is(err, natskv.ErrRevisionMismatch) {
		p.log.Warn("pgo: discard of a lost collection record failed", "collection", id, "error", err)
	}
	res.Release()
}

// createReceipt writes idem.<hash> naming this Collection,
// which is the write that releases a keyed publication:
// the record becomes claimable only after the store has acknowledged it.
// ErrUnavailable is retried once behind the same generation.
// A key already holding this identifier is the earlier attempt having landed, and counts as success.
// A key holding a receipt whose record a fresh read shows absent is stale:
// it is deleted at the revision that read returned, after which the receipt is written again.
// Anything else leaves the caller to withdraw.
func (p *Publisher) createReceipt(
	ctx context.Context, jobs natskv.KV, id string, now time.Time, hash string, in PublishInput,
) error {
	key := ReceiptKey(in.CreatedBy, in.Namespace, in.Service, in.IdempotencyKey)
	value, err := json.Marshal(Receipt{ID: id, SnapshotHash: hash, CreatedAt: now})
	if err != nil {
		return fmt.Errorf("pgo: serialize receipt for %s: %w", id, err)
	}

	_, err = jobs.Create(ctx, key, value)
	if errors.Is(err, natskv.ErrUnavailable) {
		_, err = jobs.Create(ctx, key, value)
	}
	if !errors.Is(err, natskv.ErrKeyExists) {
		if err != nil {
			return fmt.Errorf("pgo: create receipt for %s: %w", id, err)
		}

		return nil
	}

	existing, rev, gone, readErr := readReceipt(ctx, jobs, key)
	switch {
	case readErr != nil || gone:
		return fmt.Errorf("pgo: create receipt for %s: %w", id, err)
	case existing.ID == id:
		// The earlier attempt landed and lost its acknowledgement.
		return nil
	}
	if _, recordGone, recordErr := readJob(ctx, jobs, existing.ID); recordErr != nil || !recordGone {
		return fmt.Errorf("pgo: create receipt for %s: %w", id, err)
	}
	if delErr := jobs.Delete(ctx, key, rev); delErr != nil {
		return fmt.Errorf("pgo: delete a stale receipt for %s: %w", id, delErr)
	}
	if _, err := jobs.Create(ctx, key, value); err != nil {
		return fmt.Errorf("pgo: create receipt for %s: %w", id, err)
	}

	return nil
}

// withdraw undoes a keyed publication whose receipt never landed,
// so nothing keyed ever becomes claimable without the receipt that binds it.
// Both deletes are attempted whatever the first returns:
// a record delete lost to the scan, which failed the record not_published first,
// still leaves an active key to release.
func (p *Publisher) withdraw(
	ctx context.Context, jobs natskv.KV, id string, rev, activeRev uint64, in PublishInput,
) {
	//nolint:errcheck // the scan and the sweeper clear whatever a lost delete leaves
	_ = jobs.Delete(ctx, jobKey(id), rev)
	//nolint:errcheck // the sweeper releases an active key this delete could not
	_ = jobs.Delete(ctx, activeKey(in.Namespace, in.Service), activeRev)
	p.log.Warn("pgo: withdrew a collection whose idempotency receipt did not land",
		"namespace", in.Namespace, "service", in.Service, "collection", id)
}
