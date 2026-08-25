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
}

// NewRuntime returns an unbound runtime: every session it hands out until Bind
// is natskv.ErrUnavailable.
func NewRuntime() *Runtime { return &Runtime{} }

// Bind publishes the dependencies the routes serve from.
// It is called exactly once, after natskv.Preflight succeeds.
func (r *Runtime) Bind(b Bundle) { r.bundle.Store(&b) }

// Bound reports whether Bind has run.
func (r *Runtime) Bound() bool { return r.bundle.Load() != nil }

// Session is one request's generation-bound view of the PGO stores.
// It is taken exactly as every loop takes one — the generation first, then the
// replay barrier, then the view — so a request decides from a cache that is
// complete as of a point in the stream under the generation it acts through,
// or from nothing at all.
type Session struct {
	b     *Bundle
	store natskv.Stores
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

	return &Session{b: b, store: store}, nil
}

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
func (s *Session) CachedOverride(ns, svc string) (*PolicyOverride, uint64) {
	return s.b.Caches.Override(ns, svc)
}

// Collections lists what the watched job cache holds for one Service, newest
// first and at most maxListCollections entries.
func (s *Session) Collections(ns, svc string) []CollectionView {
	return s.b.Caches.Collections(ns, svc)
}

// Live reports whether the caches already show the Service as holding a live
// Collection.
// It spares a write that would lose the active create; the create is the
// decision.
func (s *Session) Live(ns, svc string) bool { return s.b.Caches.Live(ns, svc) }

// TakeToken takes one on-demand token from this replica's bucket and reports
// whether there was one.
// An empty bucket is 429 rate_limited, and it is taken before any other work a
// request does on its way to a write.
func (s *Session) TakeToken() bool { return s.b.Bucket.Take() }

// Reserve takes one reservation against the live-Collection ceiling.
// A refusal is 429 capacity_exhausted and writes nothing.
func (s *Session) Reserve(ns, svc string) (*Reservation, bool) { return s.b.Publisher.Reserve(ns, svc) }

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

// releaseActive deletes a Service's active key when it names this Collection.
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
