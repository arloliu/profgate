package natskv

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The three buckets of the permission boundary; nothing else is reachable.
const (
	configBucket    = "PROFGATE_CONFIG"
	jobsBucket      = "PROFGATE_JOBS"
	artifactsBucket = "PROFGATE_ARTIFACTS"
)

// callTimeout is the deadline every store call carries in addition to the caller's.
const callTimeout = 5 * time.Second

// reopenFirst and reopenCap bound the wait between two attempts to re-open a cut watch:
// the wait doubles from the first to the cap,
// and each wait is drawn from the upper half of its schedule so the watches of one process,
// and the replicas of one Deployment, spread out.
const (
	reopenFirst = 50 * time.Millisecond
	reopenCap   = 30 * time.Second
)

// reopenBackoff is one watch's place on that schedule.
type reopenBackoff struct {
	next time.Duration
	rng  *mrand.Rand
}

// draw returns the wait before the next attempt and advances the schedule.
func (b *reopenBackoff) draw() time.Duration {
	d := b.next
	b.next = min(b.next*2, reopenCap)

	return d/2 + time.Duration(b.rng.Int64N(int64(d/2)+1))
}

// reset returns the schedule to its first wait, once a re-opened watch has replayed.
func (b *reopenBackoff) reset() { b.next = reopenFirst }

// watchBuffer sizes the entry channel a Watch returns.
const watchBuffer = 64

// defaultReconnectWait is the pause between reconnect attempts when
// connectConfig leaves it zero; tests shorten it.
const defaultReconnectWait = 2 * time.Second

// connectConfig carries what the unexported constructor needs.
// Preflight builds it from the loaded configuration.
type connectConfig struct {
	url            string        // comma-separated nats:// or tls:// URLs
	credsFile      string        // optional credentials file
	connectTimeout time.Duration // initial-connect timeout; nats.go default when zero
	reconnectWait  time.Duration // pause between reconnect attempts; defaultReconnectWait when zero
	name           string        // connection name reported to the server

	// onConnectionChange, when set, runs with false in the disconnected
	// callback and true in the reconnected one; Preflight fires the initial
	// true itself once the first connection is up.
	onConnectionChange func(up bool)

	// onGenerationMove, when set, runs after the store generation has moved,
	// whether the disconnected callback or a closed watch subscription moved it.
	onGenerationMove func()

	// dialer, when set, replaces the TCP dialer.
	// It exists only for the test that records every subject the seam
	// publishes to; production never sets it.
	dialer nats.CustomDialer
}

// client implements Client over one nats.Conn.
type client struct {
	nc  *nats.Conn
	js  jetstream.JetStream
	log *slog.Logger

	config    jetstream.KeyValue
	jobs      jetstream.KeyValue
	artifacts jetstream.ObjectStore

	// probeDeadline bounds each bucket's preflight probe sequence.
	// It is the spec's 10 seconds; tests shorten it to prove the
	// withheld-delivery failure without waiting the full deadline.
	probeDeadline time.Duration

	// callTimeout bounds each store call, and each wait on the store inside a transfer.
	// It is the spec's 5 seconds, the callTimeout constant;
	// tests shorten it to observe one deadline pass without waiting the full five seconds.
	callTimeout time.Duration

	// onConnectionChange mirrors connectConfig.onConnectionChange.
	onConnectionChange func(up bool)

	// onGenerationMove mirrors connectConfig.onGenerationMove.
	onGenerationMove func()

	// moveMu serializes a whole move: the increment, the record it writes, and the callback it runs.
	// Two watchers whose subscriptions close together each move the generation.
	// A report that reached the runtime out of its order would end the requests of a later generation.
	// It is taken outside c.mu and never held while c.mu is,
	// so the callback still runs with c.mu released.
	moveMu sync.Mutex

	mu       sync.Mutex
	gen      uint64
	genMoved chan struct{} // closed and replaced when the generation moves
	watches  map[*watchState]struct{}

	// reopening stands from a generation move until every watch has replayed under the new generation,
	// and reopenFailing records that one re-open has already failed and written its record.
	// Both are client-wide,
	// so a cut that took down several watches at once writes one failure record rather than one for each of them.
	reopening     bool
	reopenFailing bool

	// permMu guards permErr, the last asynchronous permission violation the
	// server reported; preflight reads it to turn a silent request timeout
	// into a fatal error naming the denied operation.
	permMu  sync.Mutex
	permErr error

	// testDelayPostCheck, when non-nil, runs between a store call returning
	// and the view's post-call generation check.
	// It exists only so a test can move the generation after the server has
	// answered, proving the result is discarded; production never sets it.
	testDelayPostCheck func()

	// testInterceptProbeWatch, when non-nil, runs on every entry the
	// preflight probe watch delivers and swallows the entry when it returns
	// true.
	// It exists only so a test can prove preflight fails when the watch
	// does not deliver even though every write succeeded; production never
	// sets it.
	testInterceptProbeWatch func(bucket string, e Entry) bool

	// newReopenRand returns the generator one watch draws its re-open waits from.
	// Production seeds one from crypto/rand for every watch, so no two watches share a generator;
	// a test seeds by prefix so a run's draws are reproducible.
	newReopenRand func(prefix string) *mrand.Rand

	// testWatchOpened, when non-nil, runs with every watcher this client opens,
	// and the client consumes the watcher it returns.
	// It exists only so a test can stop a live watcher,
	// which drives the same closed subscription a deleted stream drives,
	// or put a watcher of its own in front of the real one;
	// production never sets it.
	testWatchOpened func(prefix string, w jetstream.KeyWatcher) jetstream.KeyWatcher

	// testReopenWait, when non-nil, runs with every wait drawn before a re-open attempt,
	// and the wait is skipped when it returns true.
	// It exists only so a test can count the schedule rather than sleep through it;
	// production never sets it.
	testReopenWait func(prefix string, d time.Duration) (skip bool)

	// testHoldReopen, when non-nil, runs at the top of every re-open attempt
	// and blocks for as long as the test holds it.
	// It exists only so a test can observe the gap where the watch is down;
	// production never sets it.
	testHoldReopen func(prefix string)

	// testReopenFailed, when non-nil, runs after every re-open attempt that failed.
	// It exists only so a test can wait for a retry rather than sleep long enough for one;
	// production never sets it.
	testReopenFailed func(prefix string)

	// testBeforeFetch, when non-nil, runs before the fetch of chunk n of an object read,
	// counting from one.
	// It exists only so a test can hold a read between two chunks and act on the store meanwhile;
	// production never sets it.
	testBeforeFetch func(name string, n int)

	// testChunkWritten, when non-nil, runs after chunk n of an object read has been written into the pipe,
	// which is after the caller has drained it.
	// It exists only so a test knows a pending read is waiting on the pump and not on bytes already fetched;
	// production never sets it.
	testChunkWritten func(name string, n int)
}

var _ Client = (*client)(nil)

// connect dials NATS and opens the three buckets.
// It is unexported: Preflight is the only production entry point, and it
// calls connect before checking the bucket contract and running its probes.
func connect(ctx context.Context, cfg connectConfig, log *slog.Logger) (*client, error) {
	c := &client{
		log:                log,
		genMoved:           make(chan struct{}),
		watches:            make(map[*watchState]struct{}),
		probeDeadline:      probeTimeout,
		callTimeout:        callTimeout,
		newReopenRand:      newReopenRand,
		onConnectionChange: cfg.onConnectionChange,
		onGenerationMove:   cfg.onGenerationMove,
	}

	reconnectWait := cfg.reconnectWait
	if reconnectWait == 0 {
		reconnectWait = defaultReconnectWait
	}
	opts := []nats.Option{
		nats.Name(cfg.name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(reconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			c.bumpGeneration(err)
			if c.onConnectionChange != nil {
				c.onConnectionChange(false)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			if c.onConnectionChange != nil {
				c.onConnectionChange(true)
			}
		}),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if err == nil {
				return
			}
			if errors.Is(err, nats.ErrPermissionViolation) {
				c.recordPermissionViolation(err)
			}
			c.log.Warn("nats asynchronous error", "error", err)
		}),
	}
	if cfg.connectTimeout > 0 {
		opts = append(opts, nats.Timeout(cfg.connectTimeout))
	}
	if cfg.credsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.credsFile))
	}
	if cfg.dialer != nil {
		opts = append(opts, nats.SetCustomDialer(cfg.dialer))
	}

	// A failed dial is transient: the caller retries, so it carries
	// ErrUnavailable, unlike the fatal bucket errors below.
	nc, err := nats.Connect(cfg.url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to nats: %w: %w", ErrUnavailable, err)
	}
	c.nc = nc

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	c.js = js

	if c.config, err = openKV(ctx, js, configBucket); err != nil {
		nc.Close()
		return nil, err
	}
	if c.jobs, err = openKV(ctx, js, jobsBucket); err != nil {
		nc.Close()
		return nil, err
	}
	if c.artifacts, err = openObjects(ctx, js, artifactsBucket); err != nil {
		nc.Close()
		return nil, err
	}

	return c, nil
}

func openKV(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	kv, err := js.KeyValue(cctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open bucket %s: %w", bucket, failure(err))
	}
	return kv, nil
}

func openObjects(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.ObjectStore, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	obs, err := js.ObjectStore(cctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open bucket %s: %w", bucket, failure(err))
	}
	return obs, nil
}

// recordPermissionViolation keeps the last permission violation the server
// reported asynchronously; the denied request itself only times out.
func (c *client) recordPermissionViolation(err error) {
	c.permMu.Lock()
	defer c.permMu.Unlock()
	c.permErr = err
}

// takePermissionViolation returns and clears the recorded violation.
func (c *client) takePermissionViolation() error {
	c.permMu.Lock()
	defer c.permMu.Unlock()
	err := c.permErr
	c.permErr = nil
	return err
}

// close releases the connection.
// Tests and the preflight error path use it.
func (c *client) close() {
	c.nc.Close()
}

// bumpGeneration is the body of the nats.go disconnected callback.
// It runs before the connection is usable again, so a disconnect clears the
// replay barrier at once; the reconnected callback moves nothing.
func (c *client) bumpGeneration(err error) {
	c.moveGeneration("disconnected", "error", err)
}

// moveGeneration increments the store generation and reports the move.
// Both causes reach it: the disconnected callback, and a watch whose subscription closed under a live connection.
// reason names the cause, and the one record it writes is the record for that move.
// The whole sequence runs under moveMu, so two moves report in the order they were made
// and no report reaches the runtime naming a generation another move has already left behind.
func (c *client) moveGeneration(reason string, attrs ...any) {
	c.moveMu.Lock()
	defer c.moveMu.Unlock()

	c.mu.Lock()
	c.gen++
	close(c.genMoved)
	c.genMoved = make(chan struct{})
	c.reopening = true
	gen := c.gen
	c.mu.Unlock()
	c.log.Warn("nats store generation moved", append([]any{"reason", reason, "generation", gen}, attrs...)...)
	if c.onGenerationMove != nil {
		c.onGenerationMove()
	}
}

// reopenFailed writes the one record that says re-opening the watches has started failing.
// The state it reads is client-wide,
// so a cut that took several watches down at once writes one record rather than one for each of them,
// and the retries that follow write nothing.
func (c *client) reopenFailed(prefix string, err error) {
	c.mu.Lock()
	first := !c.reopenFailing
	c.reopenFailing = true
	c.mu.Unlock()
	if first {
		c.log.Warn("nats watch re-open failing", "prefix", prefix, "error", err)
	}
	if hook := c.testReopenFailed; hook != nil {
		hook(prefix)
	}
}

func (c *client) Connected() bool {
	return c.nc.IsConnected()
}

func (c *client) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *client) Synced(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.syncedLocked(gen)
}

// syncedLocked is Synced with c.mu already held.
func (c *client) syncedLocked(gen uint64) bool {
	if gen != c.gen {
		return false
	}
	for ws := range c.watches {
		if !ws.syncedUnder(gen) {
			return false
		}
	}

	return true
}

// markSynced records one watch's replay marker under gen.
// It takes c.mu before ws.mu, the order every path that touches both takes.
// It reports whether this marker is the one that ended a re-open:
// the last of the watches to replay again under the generation the move left them.
func (c *client) markSynced(ws *watchState, gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws.setMarker(gen)
	if !c.reopening || !c.syncedLocked(gen) {
		return false
	}
	c.reopening = false
	c.reopenFailing = false

	return true
}

func (c *client) View(gen uint64) (Stores, error) {
	c.mu.Lock()
	current := c.gen
	c.mu.Unlock()
	if gen != current {
		return Stores{}, fmt.Errorf("view generation %d is not current generation %d: %w", gen, current, ErrUnavailable)
	}
	return Stores{
		Config:    &kvView{c: c, gen: gen, kv: c.config, bucket: configBucket},
		Jobs:      &kvView{c: c, gen: gen, kv: c.jobs, bucket: jobsBucket},
		Artifacts: &objView{c: c, gen: gen, obs: c.artifacts, bucket: artifactsBucket},
	}, nil
}

// genState returns the current generation and the channel closed when it moves.
func (c *client) genState() (uint64, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen, c.genMoved
}

// genCheck returns ErrUnavailable when gen is no longer current.
func (c *client) genCheck(gen uint64) error {
	c.mu.Lock()
	current := c.gen
	c.mu.Unlock()
	if gen != current {
		return fmt.Errorf("generation %d superseded by %d: %w", gen, current, ErrUnavailable)
	}
	return nil
}

func (c *client) addWatch(ws *watchState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watches[ws] = struct{}{}
}

func (c *client) removeWatch(ws *watchState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.watches, ws)
}

// watchState records the generation a watch last delivered its replay marker under.
type watchState struct {
	mu        sync.Mutex
	marker    bool
	markerGen uint64
}

func (ws *watchState) setMarker(gen uint64) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.marker = true
	ws.markerGen = gen
}

func (ws *watchState) syncedUnder(gen uint64) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.marker && ws.markerGen == gen
}

// newReopenRand seeds one math/rand/v2 generator from crypto/rand for one watch's re-open waits.
func newReopenRand(string) *mrand.Rand {
	var seed [16]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(seed[:])
	// Not a security decision: the generator only spreads when a watch is re-opened,
	// and its seed does come from crypto/rand.
	//nolint:gosec // G404: a wait, seeded from crypto/rand
	return mrand.New(mrand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:])))
}

// failure maps timeouts and connection errors to ErrUnavailable and leaves
// every other error as it came, for the caller to wrap.
// ErrAsyncPublishTimeout is the acknowledgement timer a Put runs under a context with no deadline.
func failure(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		errors.Is(err, nats.ErrTimeout),
		errors.Is(err, jetstream.ErrAsyncPublishTimeout),
		errors.Is(err, nats.ErrConnectionClosed),
		errors.Is(err, nats.ErrConnectionDraining),
		errors.Is(err, nats.ErrNoResponders),
		errors.Is(err, jetstream.ErrNoStreamResponse):
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		return err
	}
}

// statusFromStream projects the fields of the bucket contract out of the
// backing stream's configuration.
func statusFromStream(cfg jetstream.StreamConfig) Status {
	s := Status{
		TTL:          cfg.MaxAge,
		MaxValueSize: int64(cfg.MaxMsgSize),
		MaxBytes:     cfg.MaxBytes,
	}
	switch cfg.Storage {
	case jetstream.FileStorage:
		s.Storage = "file"
	case jetstream.MemoryStorage:
		s.Storage = "memory"
	}
	switch cfg.Discard {
	case jetstream.DiscardOld:
		s.Discard = "old"
	case jetstream.DiscardNew:
		s.Discard = "new"
	}
	return s
}

// kvView is one KV bucket bound to one store generation.
type kvView struct {
	c      *client
	gen    uint64
	kv     jetstream.KeyValue
	bucket string
}

var (
	_ KV       = (*kvView)(nil)
	_ Statused = (*kvView)(nil)
)

// pre rejects the call before it reaches the server when the view is stale.
func (v *kvView) pre() error {
	return v.c.genCheck(v.gen)
}

// post rejects the result, whatever the server answered, when the generation
// moved while the call was in flight.
func (v *kvView) post() error {
	if hook := v.c.testDelayPostCheck; hook != nil {
		hook()
	}
	return v.c.genCheck(v.gen)
}

func (v *kvView) Get(ctx context.Context, key string) (Entry, error) {
	if err := v.pre(); err != nil {
		return Entry{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	kve, err := v.kv.Get(cctx, key)
	if perr := v.post(); perr != nil {
		return Entry{}, perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return Entry{}, fmt.Errorf("get %q in %s: %w", key, v.bucket, ErrKeyNotFound)
		}
		return Entry{}, fmt.Errorf("get %q in %s: %w", key, v.bucket, failure(err))
	}
	return entryFromKVE(kve, v.gen), nil
}

func (v *kvView) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := v.pre(); err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	rev, err := v.kv.Create(cctx, key, value)
	if perr := v.post(); perr != nil {
		return 0, perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return 0, fmt.Errorf("create %q in %s: %w", key, v.bucket, ErrKeyExists)
		}
		return 0, fmt.Errorf("create %q in %s: %w", key, v.bucket, failure(err))
	}
	return rev, nil
}

func (v *kvView) Update(ctx context.Context, key string, value []byte, expected uint64) (uint64, error) {
	if err := v.pre(); err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	rev, err := v.kv.Update(cctx, key, value, expected)
	if perr := v.post(); perr != nil {
		return 0, perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return 0, fmt.Errorf("update %q in %s at revision %d: %w", key, v.bucket, expected, ErrRevisionMismatch)
		}
		return 0, fmt.Errorf("update %q in %s: %w", key, v.bucket, failure(err))
	}
	return rev, nil
}

func (v *kvView) Delete(ctx context.Context, key string, expected uint64) error {
	if err := v.pre(); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	err := v.kv.Delete(cctx, key, jetstream.LastRevision(expected))
	if perr := v.post(); perr != nil {
		return perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return fmt.Errorf("delete %q in %s at revision %d: %w", key, v.bucket, expected, ErrRevisionMismatch)
		}
		return fmt.Errorf("delete %q in %s: %w", key, v.bucket, failure(err))
	}
	return nil
}

func (v *kvView) Keys(ctx context.Context, prefix string) ([]string, error) {
	if err := v.pre(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	all, err := v.kv.Keys(cctx)
	if perr := v.post(); perr != nil {
		return nil, perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("keys %q in %s: %w", prefix, v.bucket, failure(err))
	}
	keys := make([]string, 0, len(all))
	for _, k := range all {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (v *kvView) Status(ctx context.Context) (Status, error) {
	if err := v.pre(); err != nil {
		return Status{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	st, err := v.kv.Status(cctx)
	if perr := v.post(); perr != nil {
		return Status{}, perr
	}
	if err != nil {
		return Status{}, fmt.Errorf("status of %s: %w", v.bucket, failure(err))
	}
	bs, ok := st.(*jetstream.KeyValueBucketStatus)
	if !ok {
		return Status{}, fmt.Errorf("status of %s: unexpected status type %T", v.bucket, st)
	}
	return statusFromStream(bs.StreamInfo().Config), nil
}

func (v *kvView) Watch(ctx context.Context, prefix string) (<-chan Entry, error) {
	if err := v.pre(); err != nil {
		return nil, err
	}
	gen, moved := v.c.genState()
	w, stop, err := v.c.openWatcher(ctx, v.kv, prefix)
	if err != nil {
		return nil, fmt.Errorf("watch %q in %s: %w", prefix, v.bucket, failure(err))
	}
	ws := &watchState{}
	v.c.addWatch(ws)
	ch := make(chan Entry, watchBuffer)
	go v.c.runWatch(ctx, v.kv, prefix, w, stop, gen, moved, ch, ws)
	return ch, nil
}

// watchFilters maps a key prefix to the subject filters that cover it:
// the prefix itself when it is a whole key, and everything nested under it.
func watchFilters(prefix string) []string {
	if prefix == "" {
		return []string{">"}
	}
	if strings.HasSuffix(prefix, ".") {
		return []string{prefix + ">"}
	}
	return []string{prefix, prefix + ".>"}
}

// openWatcher opens one nats.go watcher, bounded by callTimeout.
// nats.go ties the subscription's lifetime to the context the watcher was
// opened under, so the open cannot simply run under a deadline context:
// the returned cancel is what tears the subscription down, and it must not
// run before the watcher is done.
func (c *client) openWatcher(ctx context.Context, kv jetstream.KeyValue, prefix string) (jetstream.KeyWatcher, context.CancelFunc, error) {
	type openResult struct {
		w   jetstream.KeyWatcher
		err error
	}
	wctx, cancel := context.WithCancel(ctx)
	res := make(chan openResult, 1)
	go func() {
		w, err := kv.WatchFiltered(wctx, watchFilters(prefix))
		res <- openResult{w: w, err: err}
	}()
	timer := time.NewTimer(c.callTimeout)
	defer timer.Stop()
	select {
	case r := <-res:
		if r.err != nil {
			cancel()
			return nil, nil, r.err
		}
		if hook := c.testWatchOpened; hook != nil {
			return hook(prefix, r.w), cancel, nil
		}
		return r.w, cancel, nil
	case <-timer.C:
		// The canceled context aborts the pending open; if it produced a
		// watcher after all, the same cancellation has already closed it.
		cancel()
		return nil, nil, fmt.Errorf("open watch: %w", context.DeadlineExceeded)
	}
}

// runWatch pumps one watch until ctx ends.
// When the generation moves, or the underlying subscription closes, it
// re-opens the watcher for the current generation: a cut-off watcher has an
// unknown gap behind it, so a fresh replay and a fresh marker follow.
// A watch cut after it replayed is re-opened at once;
// a re-open that failed, and one that was cut before its replay marker arrived,
// each wait one draw of the watch's backoff first,
// which resets once a re-opened watch has replayed.
func (c *client) runWatch(ctx context.Context, kv jetstream.KeyValue, prefix string,
	w jetstream.KeyWatcher, stop context.CancelFunc, gen uint64, moved <-chan struct{}, ch chan<- Entry, ws *watchState,
) {
	defer close(ch)
	defer c.removeWatch(ws)
	backoff := &reopenBackoff{next: reopenFirst, rng: c.newReopenRand(prefix)}
	for {
		done := c.consumeWatcher(ctx, w, prefix, gen, moved, ch, ws)
		//nolint:errcheck // stopping a watcher whose subscription is already gone is fine
		_ = w.Stop()
		stop()
		if done {
			return
		}
		// The marker under gen says the watch replayed before this cut: the schedule starts over.
		// Without it the re-open was cut before its replay, and the next attempt waits as a failed one does.
		if ws.syncedUnder(gen) {
			backoff.reset()
		} else if !c.reopenWait(ctx, prefix, backoff) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if hook := c.testHoldReopen; hook != nil {
				hook(prefix)
			}
			gen, moved = c.genState()
			var err error
			w, stop, err = c.openWatcher(ctx, kv, prefix)
			if err == nil {
				break
			}
			c.reopenFailed(prefix, err)
			if !c.reopenWait(ctx, prefix, backoff) {
				return
			}
		}
	}
}

// reopenWait waits one draw of the watch's backoff before the next re-open attempt,
// and reports false when ctx ended first.
func (c *client) reopenWait(ctx context.Context, prefix string, backoff *reopenBackoff) bool {
	d := backoff.draw()
	if hook := c.testReopenWait; hook != nil && hook(prefix, d) {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// consumeWatcher forwards one watcher's deliveries, tagged with gen,
// until ctx ends (done), the generation moves, or the subscription closes.
func (c *client) consumeWatcher(ctx context.Context, w jetstream.KeyWatcher, prefix string,
	gen uint64, moved <-chan struct{}, ch chan<- Entry, ws *watchState,
) (done bool) {
	for {
		// A moved generation wins over buffered deliveries, so a cut-off
		// watcher stops before replaying anything under a stale tag.
		select {
		case <-moved:
			return false
		default:
		}
		select {
		case <-ctx.Done():
			return true
		case <-moved:
			return false
		case kve, ok := <-w.Updates():
			if !ok {
				// A watcher the caller's context closed is this process shutting the watch down, not a cut.
				if ctx.Err() != nil {
					return true
				}
				// A subscription that ends under a live connection has the same unknown gap behind it
				// as a disconnect, so it moves the store generation too and every watch re-opens under the new one.
				// A connection that went down moves it from the disconnected callback instead.
				if c.Connected() {
					c.moveGeneration("watch subscription closed", "prefix", prefix)
				}

				return false
			}
			var e Entry
			if kve == nil {
				// The marker becomes visible to Synced before it is readable
				// from the channel, so a reader that saw the marker never
				// races the barrier.
				if c.markSynced(ws, gen) {
					c.log.Info("nats watches replayed", "generation", gen)
				}
				e = Entry{Synced: true, Generation: gen}
			} else {
				e = entryFromKVE(kve, gen)
			}
			select {
			case ch <- e:
			case <-ctx.Done():
				return true
			}
		}
	}
}

// entryFromKVE converts a nats.go entry, tagging it with the generation it
// was delivered under; a delete or purge arrives as a nil Value.
func entryFromKVE(kve jetstream.KeyValueEntry, gen uint64) Entry {
	e := Entry{
		Key:        kve.Key(),
		Revision:   kve.Revision(),
		Created:    kve.Created(),
		Generation: gen,
	}
	switch kve.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		e.Value = nil
	case jetstream.KeyValuePut:
		e.Value = kve.Value()
	}
	return e
}

// objView is the Object Store bucket bound to one store generation.
type objView struct {
	c      *client
	gen    uint64
	obs    jetstream.ObjectStore
	bucket string
}

var (
	_ Objects  = (*objView)(nil)
	_ Statused = (*objView)(nil)
)

func (v *objView) pre() error {
	return v.c.genCheck(v.gen)
}

func (v *objView) post() error {
	if hook := v.c.testDelayPostCheck; hook != nil {
		hook()
	}
	return v.c.genCheck(v.gen)
}

// Put uploads r under name, bounded by the caller's context and by nothing of the seam's.
// When that context carries no deadline,
// nats.go bounds the metadata read and each chunk's acknowledgement by its own five seconds,
// so a store that stops acknowledging still fails the upload.
// A cancelled upload is ErrUnavailable, and whether an object stands under the name is then indeterminate:
// nats.go publishes the metadata before it waits for the last acknowledgements,
// and its cleanup of the partial object runs under the same cancelled context.
// Such an object carries the attempt's name and no record names it, so the sweeper's orphan rule removes it.
func (v *objView) Put(ctx context.Context, name string, r io.Reader) error {
	if err := v.pre(); err != nil {
		return err
	}
	_, err := v.obs.Put(ctx, jetstream.ObjectMeta{Name: name}, r)
	if perr := v.post(); perr != nil {
		return perr
	}
	if err != nil {
		return fmt.Errorf("put object %q in %s: %w", name, v.bucket, failure(err))
	}
	return nil
}

func (v *objView) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := v.pre(); err != nil {
		return nil, err
	}
	// Establishment runs under the call deadline: the metadata read and the consumer's creation.
	// The bytes then follow ctx, one chunk at a time, each awaited under the same deadline,
	// so the deadline bounds every wait on the store and never the transfer.
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	info, err := v.obs.GetInfo(cctx, name)
	var cons jetstream.Consumer
	consumer := ""
	if err == nil && info.Size > 0 {
		consumer = newChunkConsumerName()
		cons, err = v.c.js.CreateConsumer(cctx, artifactsStream, chunkConsumerConfig(consumer, info.NUID))
	}
	cancel()
	if perr := v.post(); perr != nil {
		if cons != nil {
			v.c.deleteChunkConsumer(consumer)
		}
		return nil, perr
	}
	if err != nil {
		// A create that timed out may have landed; the consumer's inactive threshold removes it.
		if errors.Is(err, jetstream.ErrObjectNotFound) {
			return nil, fmt.Errorf("get object %q in %s: %w", name, v.bucket, ErrObjectNotFound)
		}
		return nil, fmt.Errorf("get object %q in %s: %w", name, v.bucket, failure(err))
	}
	return v.openChunks(ctx, name, cons, consumer, info), nil
}

// artifactsStream is the stream behind the artifact bucket, named as nats.go names it.
const artifactsStream = "OBJ_" + artifactsBucket

// chunkConsumerName is the prefix of every consumer the seam creates to read an object.
const chunkConsumerName = "profgate-get-"

// newChunkConsumerName draws a consumer name no other read uses.
func newChunkConsumerName() string {
	var b [8]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(b[:])
	return chunkConsumerName + hex.EncodeToString(b[:])
}

// chunkConsumerConfig is the consumer the seam reads an object's chunks through:
// what nats.go's ordered consumer sends the server, over the one chunk subject the object's identifier names,
// bound to a name of the seam's own so the pull never resets it.
func chunkConsumerConfig(name, nuid string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Name:              name,
		FilterSubject:     "$O." + artifactsBucket + ".C." + nuid,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		AckPolicy:         jetstream.AckNonePolicy,
		MemoryStorage:     true,
		Replicas:          1,
		InactiveThreshold: 5 * time.Minute,
	}
}

// deleteChunkConsumer removes one consumer the seam created, under a fresh call deadline.
// A delete the store does not answer leaves a consumer its inactive threshold removes.
func (c *client) deleteChunkConsumer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.callTimeout)
	defer cancel()
	if err := c.js.DeleteConsumer(ctx, artifactsStream, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		c.log.Debug("nats artifact consumer not deleted", "consumer", name, "error", err)
	}
}

// errReaderClosed is the cause an objectReader's Close ends the read with.
var errReaderClosed = errors.New("object reader closed")

// errReadEnded is the cause the pump ends the read's context with once the pipe is closed,
// so the goroutine waiting on that context exits with it.
var errReadEnded = errors.New("object read ended")

// openChunks returns a reader over an io.Pipe that a pump goroutine fills one chunk at a time.
// The read ends with ctx or with Close: either closes the pipe's write side with the cause,
// which returns a pending Read at once whatever the pump is waiting on.
// An object of size zero has no chunks, no consumer, and an empty reader.
func (v *objView) openChunks(ctx context.Context, name string, cons jetstream.Consumer, consumer string, info *jetstream.ObjectInfo) io.ReadCloser {
	rctx, cancel := context.WithCancelCause(ctx)
	pr, pw := io.Pipe()
	if cons == nil {
		//nolint:errcheck // closing with nil is EOF for the reader and cannot fail
		_ = pw.Close()
		return &objectReader{r: pr, cancel: cancel}
	}
	go func() {
		<-rctx.Done()
		//nolint:errcheck // a pipe already closed by the pump keeps the pump's result
		_ = pw.CloseWithError(context.Cause(rctx))
	}()
	go func() {
		defer v.c.deleteChunkConsumer(consumer)
		//nolint:errcheck // a pipe already closed by the cancellation keeps the cause
		_ = pw.CloseWithError(v.pumpChunks(rctx, name, cons, info, pw))
		cancel(errReadEnded)
	}()
	return &objectReader{r: pr, cancel: cancel}
}

// pumpChunks fetches the object's chunks one at a time and writes each into pw.
// Each fetch is bounded by the call deadline;
// the next fetch is issued only after the previous chunk's write has returned,
// so time spent handing bytes to the caller is outside every wait on the store.
// It returns nil once the last chunk has arrived and the bytes sum to the digest the metadata carries,
// the cause when rctx ended, and the wrapped store error otherwise.
func (v *objView) pumpChunks(rctx context.Context, name string, cons jetstream.Consumer, info *jetstream.ObjectInfo, pw *io.PipeWriter) error {
	digest := sha256.New()
	for n := 1; ; n++ {
		if hook := v.c.testBeforeFetch; hook != nil {
			hook(name, n)
		}
		if rctx.Err() != nil {
			return context.Cause(rctx)
		}
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(v.c.callTimeout))
		if err != nil {
			return fmt.Errorf("get object %q in %s: chunk %d: %w", name, v.bucket, n, failure(err))
		}
		var msg jetstream.Msg
		select {
		case msg = <-batch.Messages():
		case <-rctx.Done():
			return context.Cause(rctx)
		}
		if msg == nil {
			// A batch that ended with no message and no error waited out its expiry.
			err := batch.Error()
			if err == nil {
				err = nats.ErrTimeout
			}
			return fmt.Errorf("get object %q in %s: chunk %d: %w", name, v.bucket, n, failure(err))
		}
		data := msg.Data()
		if _, err := pw.Write(data); err != nil {
			// The write side was closed under the pump, which only the read's end does.
			if cause := context.Cause(rctx); cause != nil {
				return cause
			}
			return err
		}
		digest.Write(data)
		if hook := v.c.testChunkWritten; hook != nil {
			hook(name, n)
		}
		meta, err := msg.Metadata()
		if err != nil {
			return fmt.Errorf("get object %q in %s: chunk %d: %w", name, v.bucket, n, err)
		}
		if meta.NumPending > 0 {
			continue
		}
		want, err := jetstream.DecodeObjectDigest(info.Digest)
		if err != nil {
			return fmt.Errorf("get object %q in %s: %w", name, v.bucket, err)
		}
		if !bytes.Equal(digest.Sum(nil), want) {
			return fmt.Errorf("get object %q in %s: %w", name, v.bucket, jetstream.ErrDigestMismatch)
		}
		return nil
	}
}

func (v *objView) Delete(ctx context.Context, name string) error {
	if err := v.pre(); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	err := v.obs.Delete(cctx, name)
	if perr := v.post(); perr != nil {
		return perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrObjectNotFound) {
			return nil // deleting an absent object is success
		}
		return fmt.Errorf("delete object %q in %s: %w", name, v.bucket, failure(err))
	}
	return nil
}

func (v *objView) List(ctx context.Context) ([]ObjectInfo, error) {
	if err := v.pre(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	infos, err := v.obs.List(cctx)
	if perr := v.post(); perr != nil {
		return nil, perr
	}
	if err != nil {
		if errors.Is(err, jetstream.ErrNoObjectsFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list objects in %s: %w", v.bucket, failure(err))
	}
	out := make([]ObjectInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, ObjectInfo{
			Name:    info.Name,
			Size:    info.Size,
			ModTime: info.ModTime,
		})
	}
	return out, nil
}

func (v *objView) Status(ctx context.Context) (Status, error) {
	if err := v.pre(); err != nil {
		return Status{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.c.callTimeout)
	defer cancel()
	st, err := v.obs.Status(cctx)
	if perr := v.post(); perr != nil {
		return Status{}, perr
	}
	if err != nil {
		return Status{}, fmt.Errorf("status of %s: %w", v.bucket, failure(err))
	}
	bs, ok := st.(*jetstream.ObjectBucketStatus)
	if !ok {
		return Status{}, fmt.Errorf("status of %s: unexpected status type %T", v.bucket, st)
	}
	return statusFromStream(bs.StreamInfo().Config), nil
}

// objectReader is the read side of the pipe the pump fills.
// Close ends the read with errReaderClosed, which stops the pump and releases its consumer.
type objectReader struct {
	r      *io.PipeReader
	cancel context.CancelCauseFunc
}

func (r *objectReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *objectReader) Close() error {
	r.cancel(errReaderClosed)
	return r.r.Close()
}
