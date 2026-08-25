package natskv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const (
	// maxRecordBytes is the largest serialized record the gateway writes to a
	// KV bucket; the bucket contract requires room for it.
	// A constant, not configuration.
	maxRecordBytes = 512 << 10

	// minKVMaxBytes and minObjectMaxBytes are the bucket contract's floors
	// for a bounded bucket size.
	minKVMaxBytes     = 64 << 20
	minObjectMaxBytes = 1 << 30

	// probeTimeout is the one deadline each bucket's probe sequence runs
	// under. Wide enough that a starved scheduler never turns slow watch
	// delivery into a fatal startup error; a denied permission still fails
	// immediately, from the server's error, not from this deadline.
	probeTimeout = 30 * time.Second
)

// Preflight connects, opens the three buckets through View(Generation()),
// checks their Status against the configuration contract, and runs the
// probes: per KV bucket a watch on probe.<instanceID> opened first, then
// Create, Update, Get, and Delete of that key, with the watch required to
// deliver all three revisions; per Object Store a Put, Get, List, and Delete
// of probe-<instanceID>.
// A missing bucket, a bucket of the wrong kind, a configuration outside the
// contract, or a permission error on any probe is fatal and names the bucket
// and the operation or field.
// A connection failure is ErrUnavailable and the caller retries.
func Preflight(ctx context.Context, opts Options, instanceID string, log *slog.Logger) (Client, error) {
	c, err := connect(ctx, connectConfig{
		url:                opts.URL,
		credsFile:          opts.CredsFile,
		connectTimeout:     opts.ConnectTimeout,
		name:               "profgate",
		onConnectionChange: opts.OnConnectionChange,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("nats preflight: %w", err)
	}
	if opts.OnConnectionChange != nil {
		opts.OnConnectionChange(true)
	}
	if err := c.runPreflight(ctx, instanceID); err != nil {
		c.close()
		return nil, fmt.Errorf("nats preflight: %w", err)
	}
	return c, nil
}

// runPreflight checks the bucket contract and runs the probes on an already
// connected client.
// Preflight is its only production caller;
// tests call it directly to install the probe-watch interceptor first.
func (c *client) runPreflight(ctx context.Context, instanceID string) error {
	stores, err := c.View(c.Generation())
	if err != nil {
		return err
	}

	contract := []struct {
		bucket       string
		statused     Statused
		minMaxBytes  int64
		checkValSize bool
	}{
		{configBucket, stores.Config.(Statused), minKVMaxBytes, true},
		{jobsBucket, stores.Jobs.(Statused), minKVMaxBytes, true},
		{artifactsBucket, stores.Artifacts.(Statused), minObjectMaxBytes, false},
	}
	for _, b := range contract {
		st, err := b.statused.Status(ctx)
		if err != nil {
			return fmt.Errorf("bucket %s: status: %w", b.bucket, err)
		}
		if err := checkBucketContract(b.bucket, st, b.minMaxBytes, b.checkValSize); err != nil {
			return err
		}
	}

	// Drop any violation recorded before the probes, so each probe
	// attributes only its own.
	_ = c.takePermissionViolation()
	if err := c.probeKV(ctx, configBucket, stores.Config, instanceID); err != nil {
		return err
	}
	if err := c.probeKV(ctx, jobsBucket, stores.Jobs, instanceID); err != nil {
		return err
	}
	return c.probeObjects(ctx, artifactsBucket, stores.Artifacts, instanceID)
}

// checkBucketContract verifies one bucket's server-side configuration.
// Negative sizes mean unlimited, which the contract accepts.
func checkBucketContract(bucket string, st Status, minMaxBytes int64, checkValSize bool) error {
	if st.TTL != 0 {
		return fmt.Errorf("bucket %s: field TTL is %s, the contract requires no TTL", bucket, st.TTL)
	}
	if st.Storage != "file" {
		return fmt.Errorf("bucket %s: field storage is %q, the contract requires \"file\"", bucket, st.Storage)
	}
	if st.Discard != "new" {
		return fmt.Errorf("bucket %s: field discard is %q, the contract requires \"new\"", bucket, st.Discard)
	}
	if st.MaxBytes >= 0 && st.MaxBytes < minMaxBytes {
		return fmt.Errorf("bucket %s: field maxBytes is %d, the contract requires unlimited or at least %d",
			bucket, st.MaxBytes, minMaxBytes)
	}
	if checkValSize && st.MaxValueSize >= 0 && st.MaxValueSize < maxRecordBytes {
		return fmt.Errorf("bucket %s: field maxValueSize is %d, the contract requires unlimited or at least %d",
			bucket, st.MaxValueSize, maxRecordBytes)
	}
	return nil
}

// probeErr classifies one failed probe operation: a recorded permission
// violation is fatal and names the denied operation; everything else keeps
// its class (ErrUnavailable stays retryable for the caller).
func (c *client) probeErr(bucket, op string, err error) error {
	if perr := c.takePermissionViolation(); perr != nil {
		return fmt.Errorf("bucket %s: probe %s: %w", bucket, op, perr)
	}
	return fmt.Errorf("bucket %s: probe %s: %w", bucket, op, err)
}

// probeKV exercises one KV bucket under one probeDeadline: watch first, then
// create, update, get, and delete of the probe key, and the watch must
// deliver the create, update, and delete revisions before it is closed.
func (c *client) probeKV(ctx context.Context, bucket string, kv KV, instanceID string) error {
	key := "probe." + instanceID
	pctx, cancel := context.WithTimeout(ctx, c.probeDeadline)
	defer cancel()

	ch, err := kv.Watch(pctx, key)
	if err != nil {
		return c.probeErr(bucket, "watch of "+key, err)
	}

	createRev, err := kv.Create(pctx, key, []byte("profgate preflight probe"))
	if err != nil {
		return c.probeErr(bucket, "create of "+key, err)
	}
	updateRev, err := kv.Update(pctx, key, []byte("profgate preflight probe update"), createRev)
	if err != nil {
		c.cleanupProbeKey(kv, key)
		return c.probeErr(bucket, "update of "+key, err)
	}
	if _, err := kv.Get(pctx, key); err != nil {
		c.cleanupProbeKey(kv, key)
		return c.probeErr(bucket, "get of "+key, err)
	}
	if err := kv.Delete(pctx, key, updateRev); err != nil {
		c.cleanupProbeKey(kv, key)
		return c.probeErr(bucket, "delete of "+key, err)
	}

	// The writes prove publish permissions; the watch delivering them proves
	// subscription delivery rather than subscription creation.
	var sawCreate, sawUpdate, sawDelete bool
	for !sawCreate || !sawUpdate || !sawDelete {
		select {
		case <-pctx.Done():
			return c.probeWatchErr(bucket, key)
		case e, ok := <-ch:
			if !ok {
				return c.probeWatchErr(bucket, key)
			}
			if hook := c.testInterceptProbeWatch; hook != nil && hook(bucket, e) {
				continue
			}
			switch {
			case e.Synced || e.Key != key:
				// the replay marker, or a leftover probe from a crashed
				// instance replayed before it; both are ignored
			case e.Revision == createRev:
				sawCreate = true
			case e.Revision == updateRev:
				sawUpdate = true
			case e.Value == nil && e.Revision > updateRev:
				sawDelete = true
			}
		}
	}
	return nil
}

// probeWatchErr names the watch that did not deliver; a connection that
// dropped mid-probe stays retryable instead.
func (c *client) probeWatchErr(bucket, key string) error {
	if !c.Connected() {
		return fmt.Errorf("bucket %s: probe watch of %s: connection lost: %w", bucket, key, ErrUnavailable)
	}
	return fmt.Errorf("bucket %s: the probe watch of %s did not deliver the create, update, and delete revisions within %s",
		bucket, key, c.probeDeadline)
}

// cleanupProbeKey removes what a failed probe left behind, best effort.
func (c *client) cleanupProbeKey(kv KV, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if e, err := kv.Get(ctx, key); err == nil {
		//nolint:errcheck // best-effort cleanup; the sweeper removes leftovers
		_ = kv.Delete(ctx, key, e.Revision)
	}
}

// probeObjects exercises the Object Store under one probeDeadline:
// put, get, list (which must contain the probe), and delete.
func (c *client) probeObjects(ctx context.Context, bucket string, obs Objects, instanceID string) error {
	name := "probe-" + instanceID
	pctx, cancel := context.WithTimeout(ctx, c.probeDeadline)
	defer cancel()

	payload := []byte("profgate preflight probe")
	if err := obs.Put(pctx, name, bytes.NewReader(payload)); err != nil {
		return c.probeErr(bucket, "put of "+name, err)
	}
	r, err := obs.Get(pctx, name)
	if err != nil {
		c.cleanupProbeObject(obs, name)
		return c.probeErr(bucket, "get of "+name, err)
	}
	got, err := io.ReadAll(r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		c.cleanupProbeObject(obs, name)
		return c.probeErr(bucket, "get of "+name, err)
	}
	if !bytes.Equal(got, payload) {
		c.cleanupProbeObject(obs, name)
		return fmt.Errorf("bucket %s: probe get of %s returned %d bytes, want %d", bucket, name, len(got), len(payload))
	}
	infos, err := obs.List(pctx)
	if err != nil {
		c.cleanupProbeObject(obs, name)
		return c.probeErr(bucket, "list", err)
	}
	found := false
	for _, info := range infos {
		if info.Name == name {
			found = true
			break
		}
	}
	if !found {
		c.cleanupProbeObject(obs, name)
		return fmt.Errorf("bucket %s: probe list does not contain %s", bucket, name)
	}
	if err := obs.Delete(pctx, name); err != nil {
		return c.probeErr(bucket, "delete of "+name, err)
	}
	return nil
}

// cleanupProbeObject removes what a failed probe left behind, best effort.
func (c *client) cleanupProbeObject(obs Objects, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	//nolint:errcheck // best-effort cleanup; the sweeper removes leftovers
	_ = obs.Delete(ctx, name)
}
