package pgo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/profgate/internal/natskv"
)

// Key prefixes and formats.
// Every key uses only [-/_=.a-zA-Z0-9], the set NATS accepts:
// namespace and Service names are DNS-1123 labels, identifiers are Crockford base32,
// and a slot is decimal digits, so every key qualifies by construction.
const (
	overridePrefix = "service."
	jobPrefix      = "job."
	activePrefix   = "active."
	slotPrefix     = "schedule."
)

// overrideKey is the policy override of one Service in PROFGATE_CONFIG.
func overrideKey(ns, svc string) string { return overridePrefix + ns + "." + svc }

// jobKey is the Collection record of one Collection in PROFGATE_JOBS.
func jobKey(id string) string { return jobPrefix + id }

// activeKey is the one-live-Collection-per-Service key in PROFGATE_JOBS.
func activeKey(ns, svc string) string { return activePrefix + ns + "." + svc }

// slotKey is the one-Collection-per-slot key in PROFGATE_JOBS.
// The slot is its start as decimal Unix seconds in UTC, with no padding.
func slotKey(ns, svc string, slot time.Time) string {
	return fmt.Sprintf("%s%s.%s.%d", slotPrefix, ns, svc, slot.Unix())
}

// splitServiceKey takes the namespace and Service out of a key whose suffix is
// "<ns>.<svc>" after prefix; it reports false for anything else, so a probe key
// or a key a future version adds is skipped rather than misread.
func splitServiceKey(prefix, key string) (ns, svc string, ok bool) {
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return "", "", false
	}
	ns, svc, found = strings.Cut(rest, ".")
	if !found || ns == "" || svc == "" || strings.Contains(svc, ".") {
		return "", "", false
	}

	return ns, svc, true
}

// serviceRef names one Service; it is the key of every per-Service map here.
type serviceRef struct {
	Namespace string
	Service   string
}

// activeValue is the value at active.<ns>.<svc>: the Collection that holds it.
type activeValue struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

// slotValue is the value at schedule.<ns>.<svc>.<slot>.
// RetainUntil is computed from the every that created the key, so lowering
// every afterwards cannot shorten the key's life.
type slotValue struct {
	RetainUntil time.Time `json:"retainUntil"`
}

// cachedOverride is one stored policy override with the revision it was read at.
type cachedOverride struct {
	Revision uint64
	Stored   StoredOverride
}

// cachedJob is the part of a Collection record the caches answer from:
// what the scheduler counts, what the worker scans for, and the three fields
// the sweeper's conditions are stated in.
// The manifest and the policy snapshot stay out of it; a sweeper that has to
// write a record reads the body it is about to change.
type cachedJob struct {
	Namespace string
	Service   string
	State     State
	Revision  uint64
	// Origin, Attempt, ResolvedVersion, and CreatedAt are what the Collection
	// listing shows; the listing is answered from this cache, so they live in
	// it rather than costing one read per entry.
	Origin          Origin
	Attempt         int
	ResolvedVersion string
	CreatedAt       time.Time
	// Artifact is artifact.object, empty until the completed update names one.
	Artifact   string
	FinishedAt *time.Time
	ExpiresAt  *time.Time
}

// cachedActive is one active key.
type cachedActive struct {
	ID       string
	Revision uint64
	Created  time.Time
}

// cachedSlot is one slot key.
type cachedSlot struct {
	RetainUntil time.Time
	Revision    uint64
}

// terminal reports whether a state is one a Collection never leaves.
func terminal(s State) bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateExpired:
		return true
	case StateInitializing, StatePending, StateRunning:
		return false
	default:
		// An unknown state comes from a newer gateway version; counting it as
		// live is the conservative reading, because the alternative is
		// publishing a second Collection for a Service that already has one.
		return false
	}
}

// Caches are the four watched views the PGO runtime decides from:
// service.* in PROFGATE_CONFIG, and job.*, active.*, and schedule.* in
// PROFGATE_JOBS.
// Each is rebuilt from its replay rather than patched, and carries the
// connection generation its contents were delivered under, so a cache is
// either complete as of a point in the stream under the current generation or
// not consulted at all.
type Caches struct {
	log *slog.Logger

	mu        sync.Mutex
	overrides map[string]cachedOverride // key: service.<ns>.<svc>
	jobs      map[string]cachedJob      // key: job.<id>
	active    map[serviceRef]cachedActive
	slots     map[string]cachedSlot // key: schedule.<ns>.<svc>.<slot>

	// gen and synced track the replay barrier from the consumer's side.
	// natskv marks a watch synced when it forwards the marker into its
	// channel, which is before this cache has applied a single entry, so the
	// runtime's barrier is the generation the caches themselves have completed.
	gen    [cacheCount]uint64
	synced [cacheCount]bool

	// jobPulse carries one wake-up after every job.* entry applied, so the
	// worker can attempt a claim on delivery rather than waiting for its scan.
	// It is buffered to one and never blocks the cache: a busy consumer sees
	// one wake-up for whatever it missed.
	jobPulse chan struct{}

	// applyGate, when set, runs before every entry is applied.
	// A test freezes one cache's delivery with it while the seam's own watch
	// stays synced.
	applyGate func(which cacheKind, e natskv.Entry)
}

// cacheKind names one of the four watched caches.
type cacheKind int

// The four watched caches, in the order the runtime opens them.
const (
	cacheOverrides cacheKind = iota
	cacheJobs
	cacheActive
	cacheSlots
	cacheCount
)

// NewCaches returns caches with nothing in them; Run fills them.
func NewCaches(log *slog.Logger) *Caches {
	return &Caches{
		log:       log,
		overrides: make(map[string]cachedOverride),
		jobs:      make(map[string]cachedJob),
		active:    make(map[serviceRef]cachedActive),
		slots:     make(map[string]cachedSlot),
		jobPulse:  make(chan struct{}, 1),
	}
}

// JobChanges receives one value after every job.* entry the cache applies.
func (c *Caches) JobChanges() <-chan struct{} { return c.jobPulse }

// Run opens the four watches and consumes them until ctx ends.
// It returns once every watch channel has closed.
// The seam re-opens a watch that was cut off and replays it under the new
// generation, so Run neither retries nor reconnects.
// Opening can still fail when the connection drops between preflight and the
// first watch; Run returns that error and the caller must call it again,
// because the replay barrier stays closed until the watches exist.
func (c *Caches) Run(ctx context.Context, client natskv.Client) error {
	gen := client.Generation()
	stores, err := client.View(gen)
	if err != nil {
		return fmt.Errorf("pgo: open watched caches: %w", err)
	}

	sources := []struct {
		kind   cacheKind
		kv     natskv.KV
		prefix string
	}{
		{cacheOverrides, stores.Config, overridePrefix},
		{cacheJobs, stores.Jobs, jobPrefix},
		{cacheActive, stores.Jobs, activePrefix},
		{cacheSlots, stores.Jobs, slotPrefix},
	}

	var wg sync.WaitGroup
	for _, s := range sources {
		ch, err := s.kv.Watch(ctx, s.prefix)
		if err != nil {
			return fmt.Errorf("pgo: watch %s: %w", s.prefix, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.consume(s.kind, ch)
		}()
	}
	wg.Wait()

	return nil
}

// consume applies one watch's entries until its channel closes.
func (c *Caches) consume(kind cacheKind, ch <-chan natskv.Entry) {
	for e := range ch {
		if gate := c.applyGate; gate != nil {
			gate(kind, e)
		}
		c.apply(kind, e)
	}
}

// apply folds one entry into its cache, dropping everything the cache held
// under an older generation first: a watch that was cut off has an unknown gap
// behind it, so its replay is a rebuild and not a patch.
func (c *Caches) apply(kind cacheKind, e natskv.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e.Generation != c.gen[kind] {
		c.gen[kind] = e.Generation
		c.synced[kind] = false
		c.reset(kind)
	}
	if e.Synced {
		c.synced[kind] = true

		return
	}

	switch kind {
	case cacheOverrides:
		c.applyOverride(e)
	case cacheJobs:
		c.applyJob(e)
		select {
		case c.jobPulse <- struct{}{}:
		default:
		}
	case cacheActive:
		c.applyActive(e)
	case cacheSlots:
		c.applySlot(e)
	case cacheCount:
	}
}

// reset empties one cache, called with c.mu held.
func (c *Caches) reset(kind cacheKind) {
	switch kind {
	case cacheOverrides:
		clear(c.overrides)
	case cacheJobs:
		clear(c.jobs)
	case cacheActive:
		clear(c.active)
	case cacheSlots:
		clear(c.slots)
	case cacheCount:
	}
}

// applyOverride records one service.<ns>.<svc> entry, called with c.mu held.
func (c *Caches) applyOverride(e natskv.Entry) {
	if _, _, ok := splitServiceKey(overridePrefix, e.Key); !ok {
		return
	}
	if e.Value == nil {
		delete(c.overrides, e.Key)

		return
	}
	var stored StoredOverride
	if err := json.Unmarshal(e.Value, &stored); err != nil {
		c.log.Warn("pgo: policy override is not readable", "key", e.Key, "revision", e.Revision, "error", err)
		delete(c.overrides, e.Key)

		return
	}
	c.overrides[e.Key] = cachedOverride{Revision: e.Revision, Stored: stored}
}

// applyJob records one job.<id> entry, called with c.mu held.
func (c *Caches) applyJob(e natskv.Entry) {
	id, ok := strings.CutPrefix(e.Key, jobPrefix)
	if !ok || !ValidID(id) {
		return
	}
	if e.Value == nil {
		delete(c.jobs, e.Key)

		return
	}
	var rec Record
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		c.log.Warn("pgo: collection record is not readable", "key", e.Key, "revision", e.Revision, "error", err)
		delete(c.jobs, e.Key)

		return
	}
	job := cachedJob{
		Namespace:       rec.Namespace,
		Service:         rec.Service,
		State:           rec.State,
		Revision:        e.Revision,
		Origin:          rec.Origin,
		Attempt:         rec.Attempt,
		ResolvedVersion: rec.ResolvedVersion,
		CreatedAt:       rec.CreatedAt,
		FinishedAt:      rec.FinishedAt,
		ExpiresAt:       rec.ExpiresAt,
	}
	if rec.Artifact != nil {
		job.Artifact = rec.Artifact.Object
	}
	c.jobs[e.Key] = job
}

// applyActive records one active.<ns>.<svc> entry, called with c.mu held.
func (c *Caches) applyActive(e natskv.Entry) {
	ns, svc, ok := splitServiceKey(activePrefix, e.Key)
	if !ok {
		return
	}
	ref := serviceRef{Namespace: ns, Service: svc}
	if e.Value == nil {
		delete(c.active, ref)

		return
	}
	var v activeValue
	if err := json.Unmarshal(e.Value, &v); err != nil {
		c.log.Warn("pgo: active key is not readable", "key", e.Key, "revision", e.Revision, "error", err)
		delete(c.active, ref)

		return
	}
	c.active[ref] = cachedActive{ID: v.ID, Revision: e.Revision, Created: e.Created}
}

// applySlot records one schedule.<ns>.<svc>.<slot> entry, called with c.mu held.
func (c *Caches) applySlot(e natskv.Entry) {
	if !strings.HasPrefix(e.Key, slotPrefix) {
		return
	}
	if e.Value == nil {
		delete(c.slots, e.Key)

		return
	}
	var v slotValue
	if err := json.Unmarshal(e.Value, &v); err != nil {
		c.log.Warn("pgo: slot key is not readable", "key", e.Key, "revision", e.Revision, "error", err)
		delete(c.slots, e.Key)

		return
	}
	c.slots[e.Key] = cachedSlot{RetainUntil: v.RetainUntil, Revision: e.Revision}
}

// Synced reports whether all four caches have applied their replay marker
// under gen: the PGO runtime's half of the replay barrier.
func (c *Caches) Synced(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for kind := range cacheCount {
		if !c.synced[kind] || c.gen[kind] != gen {
			return false
		}
	}

	return true
}

// overrideSnapshot returns a copy of the policy overrides, keyed by Service.
// The scheduler walks it every tick, so it is a snapshot rather than a lock
// held across policy layering and store calls.
func (c *Caches) overrideSnapshot() map[serviceRef]cachedOverride {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[serviceRef]cachedOverride, len(c.overrides))
	for key, o := range c.overrides {
		ns, svc, ok := splitServiceKey(overridePrefix, key)
		if !ok {
			continue
		}
		out[serviceRef{Namespace: ns, Service: svc}] = o
	}

	return out
}

// Live reports whether the caches show a Service as holding a live Collection:
// an active key, or a nonterminal record.
// It is what spares a write that would lose the active create; it decides
// nothing, because the create itself is the decision.
func (c *Caches) Live(ns, svc string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.liveLocked(serviceRef{Namespace: ns, Service: svc})
}

// liveLocked answers Live with c.mu held.
func (c *Caches) liveLocked(ref serviceRef) bool {
	if _, ok := c.active[ref]; ok {
		return true
	}
	for _, j := range c.jobs {
		if j.Namespace == ref.Namespace && j.Service == ref.Service && !terminal(j.State) {
			return true
		}
	}

	return false
}

// CachedLive is the number of Services the caches show as live: the
// cluster-wide live-Collection count as far as this replica has seen it.
func (c *Caches) CachedLive() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	live := make(map[serviceRef]struct{}, len(c.active)+len(c.jobs))
	for ref := range c.active {
		live[ref] = struct{}{}
	}
	for _, j := range c.jobs {
		if !terminal(j.State) {
			live[serviceRef{Namespace: j.Namespace, Service: j.Service}] = struct{}{}
		}
	}

	return len(live)
}

// nonterminalJobIDs lists the Collections the cache shows in a state they can
// still leave: the worker scan's candidates.
// The sweeper's candidates are the other ones, and it reads them through
// jobEntries.
func (c *Caches) nonterminalJobIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.jobs))
	for key, j := range c.jobs {
		if terminal(j.State) {
			continue
		}
		if id, ok := strings.CutPrefix(key, jobPrefix); ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)

	return out
}

// HasJob reports whether the job cache holds job.<id> in any state.
// The release rule's first observation.
func (c *Caches) HasJob(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.jobs[jobKey(id)]

	return ok
}

// CollectionView is one entry of the Collection listing, as the watched job
// cache holds it.
type CollectionView struct {
	ID              string
	Origin          Origin
	State           State
	Attempt         int
	ResolvedVersion string
	CreatedAt       time.Time
	FinishedAt      *time.Time
	ExpiresAt       *time.Time
}

// maxListCollections is the longest Collection listing one Service answers
// with; there is no pagination behind it.
const maxListCollections = 100

// Collections lists one Service's Collections newest first, at most
// maxListCollections of them.
// The cache is the listing's source, so a Collection appears once its watch
// has delivered it and not before.
func (c *Caches) Collections(ns, svc string) []CollectionView {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]CollectionView, 0, len(c.jobs))
	for key, j := range c.jobs {
		if j.Namespace != ns || j.Service != svc {
			continue
		}
		id, ok := strings.CutPrefix(key, jobPrefix)
		if !ok {
			continue
		}
		out = append(out, CollectionView{
			ID:              id,
			Origin:          j.Origin,
			State:           j.State,
			Attempt:         j.Attempt,
			ResolvedVersion: j.ResolvedVersion,
			CreatedAt:       j.CreatedAt,
			FinishedAt:      j.FinishedAt,
			ExpiresAt:       j.ExpiresAt,
		})
	}
	// Newest first, and by identifier where two share an instant, so one
	// listing does not reorder itself between two reads.
	slices.SortFunc(out, func(a, b CollectionView) int {
		if cmp := b.CreatedAt.Compare(a.CreatedAt); cmp != 0 {
			return cmp
		}

		return strings.Compare(a.ID, b.ID)
	})

	return out[:min(len(out), maxListCollections)]
}

// Override returns a Service's stored policy override and the revision it was
// delivered at, which is the configRevision a Collection created from it
// records.
func (c *Caches) Override(ns, svc string) (*PolicyOverride, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	o, ok := c.overrides[overrideKey(ns, svc)]
	if !ok {
		return nil, 0
	}

	return o.Stored.Policy, o.Revision
}

// ActiveID returns the Collection named by active.<ns>.<svc> in the cache.
func (c *Caches) ActiveID(ns, svc string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.active[serviceRef{Namespace: ns, Service: svc}]

	return a.ID, ok
}

// jobEntries returns a copy of the Collection records the cache holds, keyed
// by job.<id>.
// The sweeper decides its candidates from it, and every write it then makes is
// conditional on the revision the copy carries.
func (c *Caches) jobEntries() map[string]cachedJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]cachedJob, len(c.jobs))
	for k, j := range c.jobs {
		out[k] = j
	}

	return out
}

// activeEntries returns a copy of the active keys, keyed by Service.
// The sweeper releases one only after a fresh read of the Collection it names.
func (c *Caches) activeEntries() map[serviceRef]cachedActive {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[serviceRef]cachedActive, len(c.active))
	for ref, a := range c.active {
		out[ref] = a
	}

	return out
}

// slotEntries returns a copy of the slot keys and their retention.
// The sweeper deletes a key only after its own retainUntil has passed.
func (c *Caches) slotEntries() map[string]cachedSlot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]cachedSlot, len(c.slots))
	for k, v := range c.slots {
		out[k] = v
	}

	return out
}
