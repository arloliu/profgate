package pgo

import (
	"context"
	"errors"
	"strings"
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

// TestCachesRunViewFailure covers the error path ahead of every watch.
func TestCachesRunViewFailure(t *testing.T) {
	f := startPGO(t)
	wc := newWatchedCaches(t, f)
	viewErr := errors.New("no view")

	err := wc.caches.Run(t.Context(), viewFailClient{Client: wc.client, err: viewErr})
	if !errors.Is(err, viewErr) {
		t.Fatalf("Run returned %v, want the view failure", err)
	}
	if opened := wc.hook.watchesOpened(); len(opened) != 0 {
		t.Errorf("Run opened %v, want nothing when the view fails", wc.watchPrefixes())
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
