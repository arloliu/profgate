package natskv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// watchReopenDelay paces the retry loop that re-opens a cut-off watch.
const watchReopenDelay = 50 * time.Millisecond

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

	// onConnectionChange mirrors connectConfig.onConnectionChange.
	onConnectionChange func(up bool)

	mu       sync.Mutex
	gen      uint64
	genMoved chan struct{} // closed and replaced when the generation moves
	watches  map[*watchState]struct{}

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
		onConnectionChange: cfg.onConnectionChange,
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
	c.mu.Lock()
	c.gen++
	close(c.genMoved)
	c.genMoved = make(chan struct{})
	gen := c.gen
	c.mu.Unlock()
	c.log.Warn("nats disconnected", "generation", gen, "error", err)
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

// failure maps timeouts and connection errors to ErrUnavailable and leaves
// every other error as it came, for the caller to wrap.
func failure(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		errors.Is(err, nats.ErrTimeout),
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case r := <-res:
		if r.err != nil {
			cancel()
			return nil, nil, r.err
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
func (c *client) runWatch(ctx context.Context, kv jetstream.KeyValue, prefix string,
	w jetstream.KeyWatcher, stop context.CancelFunc, gen uint64, moved <-chan struct{}, ch chan<- Entry, ws *watchState,
) {
	defer close(ch)
	defer c.removeWatch(ws)
	for {
		done := c.consumeWatcher(ctx, w, gen, moved, ch, ws)
		//nolint:errcheck // stopping a watcher whose subscription is already gone is fine
		_ = w.Stop()
		stop()
		if done {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			gen, moved = c.genState()
			var err error
			w, stop, err = c.openWatcher(ctx, kv, prefix)
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(watchReopenDelay):
			}
		}
	}
}

// consumeWatcher forwards one watcher's deliveries, tagged with gen,
// until ctx ends (done), the generation moves, or the subscription closes.
func (c *client) consumeWatcher(ctx context.Context, w jetstream.KeyWatcher,
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
				return false
			}
			var e Entry
			if kve == nil {
				// The marker becomes visible to Synced before it is readable
				// from the channel, so a reader that saw the marker never
				// races the barrier.
				ws.setMarker(gen)
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

func (v *objView) Put(ctx context.Context, name string, r io.Reader) error {
	if err := v.pre(); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := v.obs.Put(cctx, jetstream.ObjectMeta{Name: name}, r)
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
	// The deadline covers the whole download: nats.go reads chunks against
	// the context it was given, so cancel must wait for the reader's Close.
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	res, err := v.obs.Get(cctx, name)
	if perr := v.post(); perr != nil {
		if res != nil {
			//nolint:errcheck // the result is discarded with the stale view
			_ = res.Close()
		}
		cancel()
		return nil, perr
	}
	if err != nil {
		cancel()
		if errors.Is(err, jetstream.ErrObjectNotFound) {
			return nil, fmt.Errorf("get object %q in %s: %w", name, v.bucket, ErrObjectNotFound)
		}
		return nil, fmt.Errorf("get object %q in %s: %w", name, v.bucket, failure(err))
	}
	return &objectReader{ReadCloser: res, cancel: cancel}, nil
}

func (v *objView) Delete(ctx context.Context, name string) error {
	if err := v.pre(); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
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

// objectReader hands the object bytes through and releases the call's
// deadline context only when the caller is done reading.
type objectReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *objectReader) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}
