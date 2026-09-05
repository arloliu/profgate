package pgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/natskv"
)

// errWatchOpen is what a test makes one source's Watch return.
var errWatchOpen = errors.New("watch open refused")

// watchedCaches is one Caches over a client whose Watch calls a test can fail,
// so a failed open can be driven at a chosen source
// and what the attempt left behind can be counted.
type watchedCaches struct {
	t      *testing.T
	caches *Caches
	client natskv.Client
	hook   *kvHook
}

// newWatchedCaches connects one client to the fixture's server
// and puts the hook in front of both buckets.
func newWatchedCaches(t *testing.T, f *pgoFixture) *watchedCaches {
	t.Helper()

	logs := newLogCapture()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	client, err := natskv.Preflight(ctx, natskv.Options{
		URL:            f.url(),
		ConnectTimeout: 5 * time.Second,
	}, "caches", logs.logger())
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}

	hook := &kvHook{}

	return &watchedCaches{t: t, caches: NewCaches(logs.logger()), client: newHookClient(client, hook), hook: hook}
}

// failWatch makes the watch on prefix fail to open, until liftWatchFailure.
func (w *watchedCaches) failWatch(prefix string) {
	w.hook.setBefore(func(op, key string) (error, bool) {
		if op == "watch" && key == prefix {
			return errWatchOpen, true
		}

		return nil, false
	})
}

// liftWatchFailure lets every later watch open.
func (w *watchedCaches) liftWatchFailure() { w.hook.setBefore(nil) }

// failWatchAfterReplay makes the watch on prefix fail to open,
// but only once every named cache has marked itself synced,
// so the attempt that fails leaves exactly those flags standing.
func (w *watchedCaches) failWatchAfterReplay(prefix string, kinds ...cacheKind) {
	w.hook.setBefore(func(op, key string) (error, bool) {
		if op != "watch" || key != prefix {
			return nil, false
		}
		// The wait runs on the goroutine Run is on,
		// so it ends on a deadline rather than failing the test from here;
		// the caller asserts the flags it wanted.
		deadline := time.Now().Add(fixtureTimeout)
		for !w.replayed(kinds...) && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}

		return errWatchOpen, true
	})
}

// replayed reports whether every named cache has marked itself synced,
// read the way Caches.Synced reads it and without its generation check.
func (w *watchedCaches) replayed(kinds ...cacheKind) bool {
	w.caches.mu.Lock()
	defer w.caches.mu.Unlock()
	for _, kind := range kinds {
		if !w.caches.synced[kind] {
			return false
		}
	}

	return true
}

// run starts Run and returns a channel carrying its result.
func (w *watchedCaches) run(ctx context.Context) <-chan error {
	out := make(chan error, 1)
	go func() { out <- w.caches.Run(ctx, w.client) }()

	return out
}

// awaitRun returns what Run returned, failing the test if it does not return.
func (w *watchedCaches) awaitRun(result <-chan error) error {
	w.t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(fixtureTimeout):
		w.t.Fatal("Run did not return")

		return nil
	}
}

// watchPrefixes is the prefix of every watch opened so far, in order.
func (w *watchedCaches) watchPrefixes() []string {
	var out []string
	for _, rec := range w.hook.watchesOpened() {
		out = append(out, rec.Prefix)
	}

	return out
}

// TestCachesRunFailedWatchOpen covers what an attempt leaves behind
// when one source's watch fails to open:
// the error names the prefix,
// the watches the attempt had already opened are finished before it returns,
// and a later attempt over the same Caches runs on four watches rather than adding to them.
func TestCachesRunFailedWatchOpen(t *testing.T) {
	f := startPGO(t)
	wc := newWatchedCaches(t, f)
	// The third of the four sources, so the attempt has two watches open.
	wc.failWatch(activePrefix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := wc.awaitRun(wc.run(ctx))
	if err == nil {
		t.Fatal("Run returned no error when a watch failed to open")
	}
	if !errors.Is(err, errWatchOpen) {
		t.Errorf("Run returned %v, which does not carry the open failure", err)
	}
	if !strings.Contains(err.Error(), activePrefix) {
		t.Errorf("Run returned %q, which does not name the prefix %q", err, activePrefix)
	}

	opened := wc.hook.watchesOpened()
	if len(opened) != 2 {
		t.Fatalf("the attempt opened %d watches (%v), want the two ahead of the failure", len(opened), wc.watchPrefixes())
	}
	for _, rec := range opened {
		if rec.Ctx.Err() == nil {
			t.Errorf("watch %s is still open under a live context after Run returned", rec.Prefix)
		}
		if !rec.Ended() {
			t.Errorf("watch %s was still delivering when Run returned", rec.Prefix)
		}
	}
	if live := wc.hook.watchesLive(); live != 0 {
		t.Errorf("%d watches outlived the failed attempt, want none", live)
	}
	if wc.caches.Synced(wc.client.Generation()) {
		t.Error("the barrier is open although the attempt failed partway through the four watches")
	}

	// The caller answers a failure by calling Run again over the same Caches.
	wc.liftWatchFailure()
	result := wc.run(ctx)
	waitFor(t, "the second attempt opens all four watches", func() bool {
		return len(wc.hook.watchesOpened()) == 6
	})
	if live := wc.hook.watchesLive(); live != 4 {
		t.Errorf("%d watches are live under the second attempt, want the four it opened", live)
	}
	waitFor(t, "the caches complete their replay", func() bool {
		return wc.caches.Synced(wc.client.Generation())
	})

	cancel()
	if err := wc.awaitRun(result); err != nil {
		t.Errorf("the second attempt returned %v, want nil on a cancelled context", err)
	}
}

// TestCachesRunClearsSyncedFlagsPerAttempt covers the barrier across two attempts.
// An attempt that fails partway through the four watches leaves the flags of the watches it opened,
// and the next attempt starts from none of them,
// so the barrier stays shut until a whole set has replayed under it.
func TestCachesRunClearsSyncedFlagsPerAttempt(t *testing.T) {
	f := startPGO(t)
	wc := newWatchedCaches(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The third of the four sources fails only once the two ahead of it have replayed,
	// so the first attempt leaves exactly two flags standing.
	wc.failWatchAfterReplay(activePrefix, cacheOverrides, cacheJobs)
	if err := wc.awaitRun(wc.run(ctx)); !errors.Is(err, errWatchOpen) {
		t.Fatalf("the first attempt returned %v, want the open failure", err)
	}
	if !wc.replayed(cacheOverrides, cacheJobs) {
		t.Fatal("the first attempt marked neither watch it opened synced, so it left nothing for the second to clear")
	}

	// The two caches the first attempt filled are held through the whole second attempt,
	// so only active and slots can mark themselves synced under it.
	// The gate is installed between the attempts, with no consumer of the first one left running.
	release := make(chan struct{})
	wc.caches.applyGate = func(which cacheKind, _ natskv.Entry) {
		if which == cacheOverrides || which == cacheJobs {
			<-release
		}
	}

	wc.liftWatchFailure()
	result := wc.run(ctx)
	waitFor(t, "the second attempt replays the two caches it is not holding", func() bool {
		return wc.replayed(cacheActive, cacheSlots)
	})
	if wc.caches.Synced(wc.client.Generation()) {
		t.Error("the barrier is open although two of the four caches have not replayed under this attempt")
	}

	close(release)
	waitFor(t, "the caches complete their replay", func() bool {
		return wc.caches.Synced(wc.client.Generation())
	})

	cancel()
	if err := wc.awaitRun(result); err != nil {
		t.Errorf("the second attempt returned %v, want nil on a cancelled context", err)
	}
}

// TestCachesRunViewFailure covers the error path ahead of every watch.
func TestCachesRunViewFailure(t *testing.T) {
	f := startPGO(t)
	wc := newWatchedCaches(t, f)
	viewErr := errors.New("no view")

	// A whole attempt runs first, so all four flags stand when the failing attempt starts.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := wc.run(ctx)
	waitFor(t, "the caches complete their replay", func() bool {
		return wc.caches.Synced(wc.client.Generation())
	})
	cancel()
	if err := wc.awaitRun(result); err != nil {
		t.Fatalf("the first attempt returned %v, want nil on a cancelled context", err)
	}
	opened := len(wc.hook.watchesOpened())

	err := wc.caches.Run(t.Context(), viewFailClient{Client: wc.client, err: viewErr})
	if !errors.Is(err, viewErr) {
		t.Fatalf("Run returned %v, want the view failure", err)
	}
	if len(wc.hook.watchesOpened()) != opened {
		t.Errorf("Run opened %v, want nothing when the view fails", wc.watchPrefixes())
	}
	if wc.caches.Synced(wc.client.Generation()) {
		t.Error("the barrier is open although the failing attempt never reached a watch")
	}
}

// TestCachesRunShutdown covers the healthy path:
// the barrier lifts once every watch has replayed,
// and a cancelled caller ends every consumer.
func TestCachesRunShutdown(t *testing.T) {
	f := startPGO(t)
	wc := newWatchedCaches(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := wc.run(ctx)
	waitFor(t, "the caches complete their replay", func() bool {
		return wc.caches.Synced(wc.client.Generation())
	})
	if opened := wc.hook.watchesOpened(); len(opened) != 4 {
		t.Fatalf("Run opened %d watches (%v), want four", len(opened), wc.watchPrefixes())
	}

	cancel()
	if err := wc.awaitRun(result); err != nil {
		t.Fatalf("Run returned %v, want nil on a cancelled context", err)
	}
	for _, rec := range wc.hook.watchesOpened() {
		if !rec.Ended() {
			t.Errorf("watch %s was still delivering when Run returned", rec.Prefix)
		}
	}
}

// viewFailClient is a client whose View never succeeds.
type viewFailClient struct {
	natskv.Client
	err error
}

func (c viewFailClient) View(uint64) (natskv.Stores, error) { return natskv.Stores{}, c.err }

// seamWatchBuffer sizes one fake watch's channel, which the seam's own watchBuffer mirrors.
const seamWatchBuffer = 64

// errSeamWatchOnly is what every seamStore call but Watch answers.
var errSeamWatchOnly = errors.New("the seam fake serves watches")

// seamClient is the store seam as Caches consumes it: a generation, two buckets, and the watches open over them.
// It exists because the cut this file is about cannot be driven against a server from here.
// The real cut is a subscription closing under a live connection,
// which internal/natskv drives through an unexported field of an unexported type and proves at the seam;
// what internal/pgo owns is the rebuild that cut must cause,
// so this stands in for the seam and delivers exactly what the seam promises:
// the generation moves, the move is reported, and every watch replays under the new generation.
type seamClient struct {
	config *seamStore
	jobs   *seamStore

	// onMove is Options.OnGenerationMove as cmd/profgate wires it.
	onMove func()

	// buffer sizes each watch's channel; seamWatchBuffer when zero.
	// Watch replays the whole bucket into that channel before it returns,
	// so a bucket larger than the buffer needs one sized for it.
	buffer int

	mu       sync.Mutex
	gen      uint64
	revision uint64
	watches  []*seamWatch
}

// seamStore is one bucket: the keys it holds, reached only through a watch.
type seamStore struct {
	c    *seamClient
	keys map[string][]byte
}

// seamWatch is one open watch: the prefix it covers and the channel it delivers on.
type seamWatch struct {
	store  *seamStore
	prefix string
	ch     chan natskv.Entry
}

// newSeamClient returns a connection on generation 1 with two empty buckets.
func newSeamClient() *seamClient {
	c := &seamClient{gen: 1}
	c.config = &seamStore{c: c, keys: map[string][]byte{}}
	c.jobs = &seamStore{c: c, keys: map[string][]byte{}}

	return c
}

func (c *seamClient) Connected() bool { return true }

func (c *seamClient) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.gen
}

// Synced is the seam's own half of the barrier, which this fake holds open:
// what the case is about is the half the caches themselves answer.
func (c *seamClient) Synced(gen uint64) bool { return gen == c.Generation() }

func (c *seamClient) View(gen uint64) (natskv.Stores, error) {
	if gen != c.Generation() {
		return natskv.Stores{}, fmt.Errorf("view of generation %d: %w", gen, natskv.ErrUnavailable)
	}

	return natskv.Stores{Config: c.config, Jobs: c.jobs}, nil
}

// put writes one value and delivers it to every watch that covers its key,
// which is another replica's write reaching this one.
func (c *seamClient) put(t *testing.T, store *seamStore, key string, value any) {
	t.Helper()

	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revision++
	store.keys[key] = body
	for _, w := range c.watches {
		if w.store == store && strings.HasPrefix(key, w.prefix) {
			w.ch <- natskv.Entry{Key: key, Value: body, Revision: c.revision, Generation: c.gen}
		}
	}
}

// remove takes one key out of a bucket and delivers nothing.
// It is the change a cut watch misses:
// no tombstone reaches the cache, so only a rebuild from the replay can lose the key.
func (c *seamClient) remove(store *seamStore, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(store.keys, key)
}

// tombstone delivers a nil value for one key to every watch that covers it,
// which is another replica's delete reaching this one.
// The key itself leaves the bucket through remove.
func (c *seamClient) tombstone(store *seamStore, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revision++
	for _, w := range c.watches {
		if w.store == store && strings.HasPrefix(key, w.prefix) {
			w.ch <- natskv.Entry{Key: key, Revision: c.revision, Generation: c.gen}
		}
	}
}

// cut is a watch subscription closing while the connection stays up.
// The seam answers it by moving the store generation and reporting the move,
// and every watch then re-opens and replays under the new generation, which replay delivers.
func (c *seamClient) cut() {
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
	if c.onMove != nil {
		c.onMove()
	}
}

// replay is one re-opened watch delivering the bucket as it stands, then its fresh marker.
// One prefix at a time, because the barrier closes only when the last of the four has replayed.
func (c *seamClient) replay(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.watches {
		if w.prefix != prefix {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(w.store.keys)) {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			c.revision++
			w.ch <- natskv.Entry{Key: key, Value: w.store.keys[key], Revision: c.revision, Generation: c.gen}
		}
		w.ch <- natskv.Entry{Synced: true, Generation: c.gen}
	}
}

func (s *seamStore) Get(context.Context, string) (natskv.Entry, error) {
	return natskv.Entry{}, errSeamWatchOnly
}

func (s *seamStore) Create(context.Context, string, []byte) (uint64, error) {
	return 0, errSeamWatchOnly
}

func (s *seamStore) Update(context.Context, string, []byte, uint64) (uint64, error) {
	return 0, errSeamWatchOnly
}

func (s *seamStore) Delete(context.Context, string, uint64) error { return errSeamWatchOnly }

func (s *seamStore) Keys(context.Context, string) ([]string, error) { return nil, errSeamWatchOnly }

// Watch registers one watch, replays the bucket into it, and ends it with the marker.
func (s *seamStore) Watch(ctx context.Context, prefix string) (<-chan natskv.Entry, error) {
	c := s.c
	buffer := c.buffer
	if buffer == 0 {
		buffer = seamWatchBuffer
	}
	c.mu.Lock()
	w := &seamWatch{store: s, prefix: prefix, ch: make(chan natskv.Entry, buffer)}
	c.watches = append(c.watches, w)
	c.mu.Unlock()

	c.replay(prefix)

	go func() {
		<-ctx.Done()
		c.mu.Lock()
		defer c.mu.Unlock()
		close(w.ch)
		c.watches = slices.DeleteFunc(c.watches, func(open *seamWatch) bool { return open == w })
	}()

	return w.ch, nil
}

// cachePrefixes names the prefix each of the four caches is filled from, in the order Run opens them.
var cachePrefixes = [cacheCount]string{overridePrefix, jobPrefix, activePrefix, slotPrefix}

// TestCachesRebuildOnAWatchCut covers what a cut under a live connection leaves in the four caches.
// The generation moves, the move is reported, and every watch replays under the new one,
// so each cache is dropped and rebuilt rather than patched:
// a key deleted while the watches were down is gone from the cache once the barrier is open again,
// although no tombstone was ever delivered for it.
// An apply that overlaid the replay onto what the cache already held would keep every deleted key
// and pass every other assertion here.
// The barrier is the other half: it stays shut until the last of the four markers,
// so a reader never sees three rebuilt caches beside one that is still filling.
func TestCachesRebuildOnAWatchCut(t *testing.T) {
	client := newSeamClient()
	rt := NewRuntime()
	client.onMove = rt.MoveGeneration

	logs := newLogCapture()
	caches := NewCaches(logs.logger())

	kept := newID()
	gone := newID()
	record := func(id string) Record {
		return Record{
			ID: id, Namespace: "payment", Service: "payment-api",
			Origin: OriginSchedule, State: StateCompleted, CreatedAt: slotBase,
		}
	}
	stored := StoredOverride{Policy: &PolicyOverride{}, UpdatedBy: "tester", UpdatedAt: slotBase}
	client.put(t, client.config, overrideKey("payment", "payment-api"), stored)
	client.put(t, client.config, overrideKey("payment", "other-api"), stored)
	client.put(t, client.jobs, jobKey(kept), record(kept))
	client.put(t, client.jobs, jobKey(gone), record(gone))
	client.put(t, client.jobs, activeKey("payment", "payment-api"), activeValue{ID: kept, CreatedAt: slotBase})
	client.put(t, client.jobs, slotKey("payment", "payment-api", slotBase), slotValue{RetainUntil: slotBase})
	client.put(t, client.jobs, slotKey("payment", "other-api", slotBase), slotValue{RetainUntil: slotBase})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- caches.Run(ctx, client) }()

	before := client.Generation()
	waitFor(t, "the four caches to replay", func() bool { return caches.Synced(before) })

	// The gap: three keys leave the buckets and no tombstone is delivered for any of them.
	client.remove(client.jobs, jobKey(gone))
	client.remove(client.jobs, activeKey("payment", "payment-api"))
	client.remove(client.config, overrideKey("payment", "other-api"))
	client.remove(client.jobs, slotKey("payment", "other-api", slotBase))

	moved := rt.generationMoved()
	client.cut()
	after := client.Generation()
	select {
	case <-moved:
	default:
		t.Fatal("the cut did not report the move to the runtime")
	}
	if caches.Synced(after) {
		t.Fatalf("the barrier is open under generation %d before any watch replayed under it", after)
	}

	// One watch at a time, so the barrier can be seen to wait for the last of them.
	for i, prefix := range cachePrefixes {
		kind := cacheKind(i)
		client.replay(prefix)
		waitFor(t, "the replay of "+prefix, func() bool {
			caches.mu.Lock()
			defer caches.mu.Unlock()

			return caches.gen[kind] == after && caches.synced[kind]
		})
		if last := kind == cacheCount-1; caches.Synced(after) != last {
			t.Fatalf("the barrier under generation %d is %v with %d of the four caches replayed",
				after, !last, i+1)
		}
	}

	if caches.Synced(before) {
		t.Fatalf("the barrier is still open under generation %d, which the cut left behind", before)
	}

	// What the replay no longer carried is gone, and what it carried is there.
	if _, revision, ok := caches.Override(after, "payment", "other-api"); !ok || revision != 0 {
		t.Errorf("the override cache holds other-api at revision %d (ok=%v), deleted while the watch was down",
			revision, ok)
	}
	if _, revision, ok := caches.Override(after, "payment", "payment-api"); !ok || revision == 0 {
		t.Errorf("the override cache lost payment-api, which the replay carried (revision %d, ok=%v)", revision, ok)
	}
	views, _, ok := caches.Collections(after, "payment", "payment-api", CollectionQuery{})
	if !ok {
		t.Fatal("the collection listing is refused under the generation the caches replayed under")
	}
	if len(views) != 1 || views[0].ID != kept {
		t.Errorf("the listing is %+v, want the one collection the replay carried", views)
	}
	if live, ok := caches.Live(after, "payment", "payment-api"); !ok || live {
		t.Errorf("the caches show payment-api live (ok=%v), with its active key deleted and its record completed",
			ok)
	}
	if slots := caches.slotEntries(); len(slots) != 1 {
		t.Errorf("the slot cache holds %d keys, want the one the replay carried", len(slots))
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on a cancelled context, want nil", err)
	}
}

// TestCachesCarryTheScanFields is an inventory:
// a cached record carries the lease, the claim deadline, the deadline, and the creation time as the store wrote them,
// because the worker scan decides from the cache alone which records it reads fresh.
func TestCachesCarryTheScanFields(t *testing.T) {
	client := newSeamClient()
	logs := newLogCapture()
	caches := NewCaches(logs.logger())

	id := newID()
	lease := slotBase.Add(time.Minute)
	deadline := slotBase.Add(time.Hour)
	client.put(t, client.jobs, jobKey(id), Record{
		ID: id, Namespace: "payment", Service: "payment-api",
		Origin: OriginSchedule, State: StateRunning, Attempt: 1,
		CreatedAt:  slotBase,
		ClaimBy:    slotBase.Add(30 * time.Second),
		LeaseUntil: &lease,
		Deadline:   &deadline,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- caches.Run(ctx, client) }()
	waitFor(t, "the four caches to replay", func() bool { return caches.Synced(client.Generation()) })

	got, ok := caches.jobEntries()[jobKey(id)]
	if !ok {
		t.Fatalf("the job cache does not hold %s", jobKey(id))
	}
	if !got.CreatedAt.Equal(slotBase) {
		t.Errorf("createdAt is %s, want %s", got.CreatedAt, slotBase)
	}
	if !got.ClaimBy.Equal(slotBase.Add(30 * time.Second)) {
		t.Errorf("claimBy is %s, want %s", got.ClaimBy, slotBase.Add(30*time.Second))
	}
	if got.LeaseUntil == nil || !got.LeaseUntil.Equal(lease) {
		t.Errorf("leaseUntil is %v, want %s", got.LeaseUntil, lease)
	}
	if got.Deadline == nil || !got.Deadline.Equal(deadline) {
		t.Errorf("deadline is %v, want %s", got.Deadline, deadline)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on a cancelled context, want nil", err)
	}
}

// TestCachesCollectionsCostTheServiceAlone holds what a one-Service listing costs the cache:
// the entries it visits and the bytes it allocates are the Service's own records and not every record the bucket holds.
// A listing that walked the whole job map under the lock visited every record of every Service
// and sized its page for all of them.
func TestCachesCollectionsCostTheServiceAlone(t *testing.T) {
	const others = 10_000
	client := newSeamClient()
	client.buffer = 20_000
	logs := newLogCapture()
	caches := NewCaches(logs.logger())

	record := func(id, svc string) Record {
		return Record{
			ID: id, Namespace: "payment", Service: svc,
			Origin: OriginSchedule, State: StateCompleted, CreatedAt: slotBase,
		}
	}
	for range 3 {
		id := newID()
		client.put(t, client.jobs, jobKey(id), record(id, "payment-api"))
	}
	for range others {
		id := newID()
		client.put(t, client.jobs, jobKey(id), record(id, "other-api"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- caches.Run(ctx, client) }()
	gen := client.Generation()
	waitFor(t, "the four caches to replay", func() bool { return caches.Synced(gen) })

	var mu sync.Mutex
	visited := 0
	caches.listVisited = func(string) {
		mu.Lock()
		defer mu.Unlock()
		visited++
	}

	views, more, ok := caches.Collections(gen, "payment", "payment-api", CollectionQuery{})
	if !ok || more || len(views) != 3 {
		t.Fatalf("the listing is %d entries (more=%v, ok=%v), want the three of payment-api", len(views), more, ok)
	}
	mu.Lock()
	got := visited
	mu.Unlock()
	if got != 3 {
		t.Errorf("the listing visited %d entries, want the three of payment-api and none of the %d others", got, others)
	}

	result := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			caches.Collections(gen, "payment", "payment-api", CollectionQuery{})
		}
	})
	if bytes := result.AllocedBytesPerOp(); bytes >= 4096 {
		t.Errorf("the listing allocates %d bytes, want under 4 KiB for three entries", bytes)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on a cancelled context, want nil", err)
	}
}

// TestCachesServiceIndexFollowsTheRecord holds the per-Service index to the job map:
// a delivered record is indexed under its Service, a tombstone removes it and its Service's emptied set,
// and a replay under a new generation rebuilds the index with the map rather than keeping what the cut left behind.
func TestCachesServiceIndexFollowsTheRecord(t *testing.T) {
	client := newSeamClient()
	logs := newLogCapture()
	caches := NewCaches(logs.logger())

	id := newID()
	ref := serviceRef{Namespace: "payment", Service: "payment-api"}
	client.put(t, client.jobs, jobKey(id), Record{
		ID: id, Namespace: ref.Namespace, Service: ref.Service,
		Origin: OriginSchedule, State: StateCompleted, CreatedAt: slotBase,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- caches.Run(ctx, client) }()
	before := client.Generation()
	waitFor(t, "the four caches to replay", func() bool { return caches.Synced(before) })

	indexed := func() (map[serviceRef]map[string]struct{}, int) {
		caches.mu.Lock()
		defer caches.mu.Unlock()
		index := make(map[serviceRef]map[string]struct{}, len(caches.byService))
		for svc, keys := range caches.byService {
			index[svc] = maps.Clone(keys)
		}

		return index, len(caches.jobs)
	}

	index, jobs := indexed()
	if _, ok := index[ref][jobKey(id)]; !ok || len(index) != 1 || len(index[ref]) != 1 || jobs != 1 {
		t.Fatalf("after one delivery the index is %v beside %d records, want %s alone under %v", index, jobs, jobKey(id), ref)
	}

	client.remove(client.jobs, jobKey(id))
	client.tombstone(client.jobs, jobKey(id))
	waitFor(t, "the tombstone to apply", func() bool { _, jobs := indexed(); return jobs == 0 })
	if index, _ := indexed(); len(index) != 0 {
		t.Fatalf("after the tombstone the index is %v, want the Service's set gone", index)
	}

	client.put(t, client.jobs, jobKey(id), Record{
		ID: id, Namespace: ref.Namespace, Service: ref.Service,
		Origin: OriginSchedule, State: StateCompleted, CreatedAt: slotBase,
	})
	waitFor(t, "the record to return", func() bool { index, _ := indexed(); return len(index[ref]) == 1 })

	// The record leaves while the watches are down, so no tombstone is delivered for it.
	client.remove(client.jobs, jobKey(id))
	client.cut()
	after := client.Generation()
	for _, prefix := range cachePrefixes {
		client.replay(prefix)
	}
	waitFor(t, "the replay under the new generation", func() bool { return caches.Synced(after) })
	if index, jobs := indexed(); len(index) != 0 || jobs != 0 {
		t.Fatalf("after the replay the index is %v beside %d records, want both empty under generation %d", index, jobs, after)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on a cancelled context, want nil", err)
	}
}
