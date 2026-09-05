package pgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
)

// APIClaimGrace is how long an on-demand Collection waits to be claimed.
// A scheduled Collection uses its own every instead, which the next slot
// bounds; an on-demand one has no slot, so the bound is a constant.
const APIClaimGrace = time.Hour

// tokenRefillWindow is the denominator of the on-demand bucket's refill:
// pgo.limits.onDemandPerMinute tokens arrive over this long.
const tokenRefillWindow = time.Minute

// Bundle is everything the PGO routes decide from, fixed once at Bind.
// It is immutable after that: a request reads the pointer and works from the
// values it found, so a handler never sees half a rebind.
type Bundle struct {
	Client    natskv.Client
	Caches    *Caches
	Publisher *Publisher
	Bucket    *TokenBucket
	Defaults  Policy
	Limits    config.PGOLimits
	Clock     Clock
	Recorder  metrics.Recorder
	Instance  string
	Log       *slog.Logger
}

// Runtime is the handlers' late-bound view of the PGO machinery.
// The HTTP server starts before the NATS preflight has succeeded, because
// interactive routes stay available while NATS is unreachable, so the client,
// the publisher, and the caches cannot be constructor arguments.
// Bind publishes them once preflight has passed; until then every request
// reports the runtime unavailable and the routes answer 503 pgo_unavailable.
type Runtime struct {
	bundle atomic.Pointer[Bundle]

	mu sync.Mutex
	// moved is closed when the store generation it belongs to is left behind,
	// and replaced by the channel of the generation that follows.
	// It exists from construction,
	// because the connection can drop before the preflight that binds the runtime has passed.
	moved chan struct{}
}

// NewRuntime returns an unbound runtime: every session it hands out until Bind
// is natskv.ErrUnavailable.
func NewRuntime() *Runtime { return &Runtime{moved: make(chan struct{})} }

// MoveGeneration reports that the store generation has moved.
// It closes the channel of the generation being left behind,
// which is what ends a request waiting under it,
// and installs the channel of the next one.
// The connection's disconnect callback moves its generation before it reports,
// so a receiver always sees a generation that has already moved.
func (r *Runtime) MoveGeneration() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.moved)
	r.moved = make(chan struct{})
}

// generationMoved is the channel of the generation current now.
func (r *Runtime) generationMoved() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.moved
}

// Bind publishes the dependencies the routes serve from.
// It is called exactly once, after natskv.Preflight succeeds.
func (r *Runtime) Bind(b Bundle) { r.bundle.Store(&b) }

// bound reports whether Bind has run.
func (r *Runtime) bound() bool { return r.bundle.Load() != nil }

// Session is one request's generation-bound view of the PGO stores.
// It is taken exactly as every loop takes one — the generation first, then the
// replay barrier, then the view — so a request decides from a cache that is
// complete as of a point in the stream under the generation it acts through,
// or from nothing at all.
type Session struct {
	b     *Bundle
	store natskv.Stores
	// gen is the store generation this session passed the barrier under.
	// Every cache read it makes takes it,
	// so a read arriving after those caches were reset is refused rather than answered from a cache that was emptied.
	gen uint64
	// moved is the store-generation broadcast this session captured when it was taken,
	// and never a lookup made later.
	moved <-chan struct{}
}

// Session returns the view this request works from.
// It is natskv.ErrUnavailable while the runtime is unbound, while the
// connection is down, while the watches have not finished replaying under the
// current generation, and when the generation moved before the view was taken:
// every case the PGO routes answer 503 pgo_unavailable.
func (r *Runtime) Session() (*Session, error) {
	b := r.bundle.Load()
	if b == nil {
		return nil, fmt.Errorf("pgo: runtime is not bound: %w", natskv.ErrUnavailable)
	}

	// The broadcast is captured before the generation is read,
	// so a move between the two closes the channel this session holds.
	// The cost of that order is a session refusing a generation still current,
	// which is the direction that cannot leave a request parked over an outage.
	moved := r.generationMoved()

	gen := b.Client.Generation()
	// Both halves of the barrier: the seam marks a watch synced when it
	// forwards the replay marker into its channel, which is before the cache
	// has applied a single entry of it.
	if !b.Client.Connected() || !b.Client.Synced(gen) || !b.Caches.Synced(gen) {
		return nil, fmt.Errorf("pgo: watches have not replayed under generation %d: %w", gen, natskv.ErrUnavailable)
	}

	store, err := b.Client.View(gen)
	if err != nil {
		return nil, fmt.Errorf("pgo: view of generation %d: %w", gen, err)
	}

	return &Session{b: b, store: store, gen: gen, moved: moved}, nil
}

// Subscribe registers a channel pulsed for every job.<id> entry applied for id.
// The pulse is a hint and never an answer:
// the handler re-reads the record and decides from that read alone,
// so a pulse carries nothing
// and a full buffer drops one rather than blocking apply.
// The returned function removes the registration and is called when the request ends.
func (s *Session) Subscribe(id string) (<-chan struct{}, func()) { return s.b.Caches.Subscribe(id) }

// GenerationMoved returns the channel this session captured when it was taken:
// the one closed when the generation its view is bound to is left behind.
// It is a field of the session and not a lookup,
// so a generation that moves between a handler's read and its select still closes the channel it holds;
// a lookup would hand back the replacement and lose the signal.
func (s *Session) GenerationMoved() <-chan struct{} { return s.moved }

// NewTimer is the session's clock, which is what bounds a request that waits.
func (s *Session) NewTimer(d time.Duration) Timer { return s.b.Clock.NewTimer(d) }

// Now is the session's clock, in UTC.
func (s *Session) Now() time.Time { return s.b.Clock.Now().UTC() }

// Effective layers the overrides onto the operator's defaults, in the order
// given, and measures the result against this replica's ceilings.
// GET and PUT of the policy pass the stored override alone; POST /collections
// passes the stored override and then the request body.
func (s *Session) Effective(overrides ...*PolicyOverride) (Policy, []Violation) {
	p := s.b.Defaults
	for _, o := range overrides {
		p = Effective(p, o)
	}

	return p, Validate(p, s.b.Limits)
}

// StoredRecord is one Collection record as the bucket holds it: the parsed
// form the gateway decides from, the bytes a reader is served as stored, and
// the revision every conditional write of it is made at.
type StoredRecord struct {
	Record   Record
	Value    []byte
	Revision uint64
}

// ReadRecord reads one Collection record fresh.
// It is natskv.ErrKeyNotFound for a Collection that does not exist, which the
// routes answer 404 collection_not_found.
func (s *Session) ReadRecord(ctx context.Context, id string) (StoredRecord, error) {
	e, err := s.store.Jobs.Get(ctx, jobKey(id))
	if err != nil {
		return StoredRecord{}, fmt.Errorf("pgo: read collection %s: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return StoredRecord{}, fmt.Errorf("pgo: collection %s is not readable: %w", id, err)
	}

	return StoredRecord{Record: rec, Value: e.Value, Revision: e.Revision}, nil
}

// WriteRecord commits a state transition at the revision it was read at.
// natskv.ErrRevisionMismatch means the record moved and the caller lost.
func (s *Session) WriteRecord(ctx context.Context, rec Record, revision uint64) error {
	value, err := MarshalBounded(rec)
	if err != nil {
		return err
	}
	if _, err := s.store.Jobs.Update(ctx, jobKey(rec.ID), value, revision); err != nil {
		return fmt.Errorf("pgo: write collection %s: %w", rec.ID, err)
	}

	return nil
}

// ReadReceipt reads idem.<hash> authoritatively, past every cache,
// because a receipt decides whether a Collection is created.
// It reports the revision the receipt was read at,
// which is the one a stale receipt is deleted at,
// and natskv.ErrKeyNotFound for a key with no history.
func (s *Session) ReadReceipt(ctx context.Context, key string) (Receipt, uint64, error) {
	r, revision, gone, err := readReceipt(ctx, s.store.Jobs, key)
	switch {
	case err != nil:
		return Receipt{}, 0, err
	case gone:
		return Receipt{}, 0, fmt.Errorf("pgo: read receipt %s: %w", key, natskv.ErrKeyNotFound)
	}

	return r, revision, nil
}

// DeleteReceipt removes a stale receipt at the revision its read returned.
// Losing the delete means another writer got there first, which is what makes
// the revision the guard rather than the key's existence.
func (s *Session) DeleteReceipt(ctx context.Context, key string, revision uint64) error {
	if err := s.store.Jobs.Delete(ctx, key, revision); err != nil {
		return fmt.Errorf("pgo: delete receipt %s: %w", key, err)
	}

	return nil
}

// ReadOverride reads a Service's stored policy override fresh, with the
// revision that is its ETag.
// It is natskv.ErrKeyNotFound when the Service has no override.
func (s *Session) ReadOverride(ctx context.Context, ns, svc string) (StoredOverride, uint64, error) {
	e, err := s.store.Config.Get(ctx, overrideKey(ns, svc))
	if err != nil {
		return StoredOverride{}, 0, fmt.Errorf("pgo: read policy of %s/%s: %w", ns, svc, err)
	}
	var stored StoredOverride
	if err := json.Unmarshal(e.Value, &stored); err != nil {
		return StoredOverride{}, 0, fmt.Errorf("pgo: policy of %s/%s is not readable: %w", ns, svc, err)
	}

	return stored, e.Revision, nil
}

// CreateOverride writes a Service's first policy override.
// natskv.ErrKeyExists means another writer got there first.
func (s *Session) CreateOverride(ctx context.Context, ns, svc string, stored StoredOverride) (uint64, error) {
	value, err := MarshalBounded(stored)
	if err != nil {
		return 0, err
	}
	rev, err := s.store.Config.Create(ctx, overrideKey(ns, svc), value)
	if err != nil {
		return 0, fmt.Errorf("pgo: create policy of %s/%s: %w", ns, svc, err)
	}

	return rev, nil
}

// UpdateOverride replaces a Service's policy override at the revision the
// client's If-Match named.
func (s *Session) UpdateOverride(
	ctx context.Context, ns, svc string, stored StoredOverride, revision uint64,
) (uint64, error) {
	value, err := MarshalBounded(stored)
	if err != nil {
		return 0, err
	}
	rev, err := s.store.Config.Update(ctx, overrideKey(ns, svc), value, revision)
	if err != nil {
		return 0, fmt.Errorf("pgo: update policy of %s/%s: %w", ns, svc, err)
	}

	return rev, nil
}

// DeleteOverride removes a Service's policy override at the revision it was
// read at, returning the Service to the operator's defaults.
func (s *Session) DeleteOverride(ctx context.Context, ns, svc string, revision uint64) error {
	if err := s.store.Config.Delete(ctx, overrideKey(ns, svc), revision); err != nil {
		return fmt.Errorf("pgo: delete policy of %s/%s: %w", ns, svc, err)
	}

	return nil
}

// CachedOverride is the Service's override as the watched PROFGATE_CONFIG
// cache holds it, with the revision a Collection records as its
// configRevision.
// It is read only behind the replay barrier, so a Service that has an override
// never yields a zero revision.
// It is natskv.ErrUnavailable once the caches have moved past this session's generation.
func (s *Session) CachedOverride(ns, svc string) (*PolicyOverride, uint64, error) {
	o, revision, ok := s.b.Caches.Override(s.gen, ns, svc)
	if !ok {
		return nil, 0, s.staleCaches()
	}

	return o, revision, nil
}

// staleCaches is what a cache read answers once the caches have been reset under a newer generation.
// It wraps the error Session itself returns, so the routes map it to 503 pgo_unavailable already.
func (s *Session) staleCaches() error {
	return fmt.Errorf("pgo: caches have moved past generation %d: %w", s.gen, natskv.ErrUnavailable)
}

// Collections lists what the watched job cache holds for one Service, newest first:
// the page the query asks for,
// and whether the listing holds more entries behind it.
// It is natskv.ErrUnavailable once the caches have moved past this session's generation.
func (s *Session) Collections(ns, svc string, q CollectionQuery) ([]CollectionView, bool, error) {
	views, more, ok := s.b.Caches.Collections(s.gen, ns, svc, q)
	if !ok {
		return nil, false, s.staleCaches()
	}

	return views, more, nil
}

// Live reports whether the caches already show the Service as holding a live
// Collection.
// It spares a write that would lose the active create; the create is the
// decision.
// It is natskv.ErrUnavailable once the caches have moved past this session's generation.
func (s *Session) Live(ns, svc string) (bool, error) {
	live, ok := s.b.Caches.Live(s.gen, ns, svc)
	if !ok {
		return false, s.staleCaches()
	}

	return live, nil
}

// TakeToken takes one on-demand token from this replica's bucket and reports
// whether there was one.
// An empty bucket is 429 rate_limited, and it is taken before any other work a
// request does on its way to a write.
func (s *Session) TakeToken() bool { return s.b.Bucket.Take() }

// Reserve takes one reservation against the live-Collection ceiling.
// ErrCapacityExhausted is 429 capacity_exhausted and writes nothing.
// The count behind it is a cache read, so it takes this session's generation
// and is natskv.ErrUnavailable once the caches have moved past it.
func (s *Session) Reserve(ns, svc string) (*Reservation, error) {
	return s.b.Publisher.Reserve(s.gen, ns, svc)
}

// Publish performs the three writes that publish a Collection, exactly as the
// scheduler's publication does.
func (s *Session) Publish(ctx context.Context, res *Reservation, in PublishInput) (string, Outcome, error) {
	return s.b.Publisher.Publish(ctx, s.store.Jobs, res, in)
}

// OpenArtifact opens the stored profile of a completed Collection.
// natskv.ErrObjectNotFound means the object is gone and the record is due for
// its expired flip.
func (s *Session) OpenArtifact(ctx context.Context, object string) (io.ReadCloser, error) {
	rc, err := s.store.Artifacts.Get(ctx, object)
	if err != nil {
		return nil, fmt.Errorf("pgo: read artifact %s: %w", object, err)
	}

	return rc, nil
}

// LatestCompleted returns the newest Collection of a Service that is completed
// and whose artifact is still in the store,
// together with that artifact open for reading; the caller closes it.
// The watched cache is a candidate filter and never the authority:
// each candidate costs one authoritative Get,
// a candidate whose fresh read is not completed is dropped,
// and one whose object cannot be opened is flipped to expired
// at the revision that read returned before the walk continues.
// The open is the confirmation,
// so the reader a caller streams is the one the walk confirmed
// and no second open can find the object gone.
// It is natskv.ErrUnavailable when the caches have moved past this session's generation,
// because the candidate list is the cache read this walk begins from.
// It is natskv.ErrKeyNotFound when no candidate survives,
// and a store that cannot be read is reported rather than an older Collection:
// a store the gateway cannot read says nothing about which artifact is newest.
func (s *Session) LatestCompleted(ctx context.Context, ns, svc string) (StoredRecord, io.ReadCloser, error) {
	candidates, _, ok := s.b.Caches.Collections(s.gen, ns, svc, CollectionQuery{})
	if !ok {
		return StoredRecord{}, nil, s.staleCaches()
	}
	for _, v := range candidates {
		if v.State != StateCompleted {
			continue
		}

		stored, err := s.ReadRecord(ctx, v.ID)
		switch {
		case err == nil:
		case errors.Is(err, natskv.ErrKeyNotFound):
			// The record reached its retention while the cache still held it.
			continue
		default:
			return StoredRecord{}, nil, err
		}
		if stored.Record.State != StateCompleted {
			continue
		}

		object := ""
		if stored.Record.Artifact != nil {
			object = stored.Record.Artifact.Object
		}
		if object == "" {
			s.ExpireGoneArtifact(ctx, stored)

			continue
		}

		body, err := s.OpenArtifact(ctx, object)
		switch {
		case err == nil:
			return stored, body, nil
		case errors.Is(err, natskv.ErrObjectNotFound):
			s.ExpireGoneArtifact(ctx, stored)
		default:
			return StoredRecord{}, nil, err
		}
	}

	return StoredRecord{}, nil, fmt.Errorf("pgo: %s/%s has no completed collection with its artifact: %w",
		ns, svc, natskv.ErrKeyNotFound)
}

// ExpireGoneArtifact flips a completed record whose object is no longer in the store,
// and reports whether the flip landed.
// The conditional update at the revision the fresh read returned is what decides:
// the reader that wins it owns the transition's log record and its metric row,
// exactly as the sweeper owns the same transition on its own path,
// so one flip is never counted twice.
// A lost update is another reader's flip and needs nothing from this one.
// Any other failure is logged at warn and counted under op="expire";
// whether the update landed is then indeterminate,
// and the next reader or the next sweep observes what stands.
func (s *Session) ExpireGoneArtifact(ctx context.Context, stored StoredRecord) bool {
	rec := stored.Record
	rec.State = StateExpired
	if err := s.WriteRecord(ctx, rec, stored.Revision); err != nil {
		if !errors.Is(err, natskv.ErrRevisionMismatch) {
			s.b.Log.Warn("pgo: expired flip failed", "collection", rec.ID, "error", err)
			s.b.Recorder.StoreFailure(storeOpExpire)
		}

		return false
	}
	s.RecordTransition(rec)

	return true
}

// ReleaseActive frees the Service the moment its Collection ends.
func (s *Session) ReleaseActive(ctx context.Context, rec Record) {
	releaseActive(ctx, s.store.Jobs, rec)
}

// RecordTransition emits the one log record and the one metric row for a
// transition this request committed.
// Whichever conditional update wins owns them, so no transition is counted
// twice however many replicas raced for it.
func (s *Session) RecordTransition(rec Record) {
	logTransition(s.b.Log, s.b.Instance, rec)
	s.b.Recorder.Collection(string(rec.State))
}

// logTransition emits the one transition record for a state its caller has
// committed.
// Every transition is logged by whichever component commits it and by nothing
// else, so no transition is recorded twice.
// extra carries what a single caller adds: the publisher names what asked for
// the publication, and no other caller passes anything.
func logTransition(log *slog.Logger, instance string, rec Record, extra ...slog.Attr) {
	args := []any{
		"collection", rec.ID,
		"namespace", rec.Namespace,
		"service", rec.Service,
		"state", string(rec.State),
		"attempt", rec.Attempt,
		"reason", rec.Reason,
		"instance", instance,
	}
	for _, a := range extra {
		args = append(args, a)
	}
	log.Info("collection transition", args...)
}

// readJob reads job.<id> fresh, past every cache, and reports whether the
// record is gone.
// gone is true only for a record that no longer exists, which is the one
// absence a caller may act on;
// an error means the read said nothing at all, and every caller keeps what it
// holds, so a record it could not read never gives up a reservation, an active
// key, or an artifact.
func readJob(ctx context.Context, jobs natskv.KV, id string) (Record, bool, error) {
	e, err := jobs.Get(ctx, jobKey(id))
	if errors.Is(err, natskv.ErrKeyNotFound) {
		return Record{}, true, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var rec Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return Record{}, false, fmt.Errorf("pgo: read record %s: %w", id, err)
	}

	return rec, false, nil
}

// readReceipt reads idem.<hash> fresh, past every cache,
// and reports the revision it was read at and whether the key is gone.
// No watch is opened on the prefix and no cache holds it,
// so every read of a receipt is the store's answer;
// an error means the read said nothing at all,
// and a caller that could not read a receipt deletes nothing.
func readReceipt(ctx context.Context, jobs natskv.KV, key string) (Receipt, uint64, bool, error) {
	e, err := jobs.Get(ctx, key)
	if errors.Is(err, natskv.ErrKeyNotFound) {
		return Receipt{}, 0, true, nil
	}
	if err != nil {
		return Receipt{}, 0, false, err
	}
	var r Receipt
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return Receipt{}, 0, false, fmt.Errorf("pgo: read receipt %s: %w", key, err)
	}

	return r, e.Revision, false, nil
}

// releaseActive frees the Service the moment its Collection ends: it deletes
// the active key when it names this Collection.
// It can never release a successor's claim, and a release that fails is not
// retried: the sweeper covers it on its next pass.
func releaseActive(ctx context.Context, jobs natskv.KV, rec Record) {
	e, err := jobs.Get(ctx, activeKey(rec.Namespace, rec.Service))
	if err != nil {
		return
	}
	var v activeValue
	if err := json.Unmarshal(e.Value, &v); err != nil || v.ID != rec.ID {
		return
	}
	//nolint:errcheck // a lost delete is done; the sweeper releases the key instead
	_ = jobs.Delete(ctx, activeKey(rec.Namespace, rec.Service), e.Revision)
}

// ResolveVersion is round 0's version rule, the one both the round loop and
// the on-demand handler's advisory pre-check apply:
// eligible targets are those carrying a version, narrowed to the policy's pin
// when it has one, and they must agree on exactly one.
// The reason is empty when they do, and otherwise the reason a Collection ends
// with: ReasonVersionMissing when none carries a version, ReasonVersionConflict
// when they disagree.
func ResolveVersion(targets []k8s.Target, pin string) (version string, reason string) {
	eligible := filterTargets(targets, func(t k8s.Target) bool { return t.Version != "" })
	if pin != "" {
		eligible = filterTargets(eligible, func(t k8s.Target) bool { return t.Version == pin })
	}

	switch versions := distinctVersions(eligible); len(versions) {
	case 0:
		return "", ReasonVersionMissing
	case 1:
		return versions[0], ""
	default:
		return "", ReasonVersionConflict
	}
}

// TokenBucket is one replica's on-demand creation rate limit:
// pgo.limits.onDemandPerMinute tokens, refilled continuously at that rate.
// It bounds creations at replicas × onDemandPerMinute per minute, so a caller
// with pgo.collect over many Services cannot outrun the workers.
type TokenBucket struct {
	clock     Clock
	capacity  float64
	perSecond float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewTokenBucket returns a full bucket of perMinute tokens.
func NewTokenBucket(perMinute int, clock Clock) *TokenBucket {
	capacity := float64(perMinute)

	return &TokenBucket{
		clock:     clock,
		capacity:  capacity,
		perSecond: capacity / tokenRefillWindow.Seconds(),
		tokens:    capacity,
		last:      clock.Now(),
	}
}

// Take removes one token and reports whether there was one to remove.
func (b *TokenBucket) Take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed.Seconds()*b.perSecond)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--

	return true
}
