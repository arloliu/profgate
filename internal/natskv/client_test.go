package natskv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestKV(t *testing.T) {
	t.Run("create on an existing key is ErrKeyExists", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs

		rev, err := kv.Create(ctx, "job.a", []byte("first"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if rev == 0 {
			t.Fatalf("create returned revision 0")
		}
		if _, err := kv.Create(ctx, "job.a", []byte("second")); !errors.Is(err, ErrKeyExists) {
			t.Fatalf("second create: got %v, want ErrKeyExists", err)
		}

		e, err := kv.Get(ctx, "job.a")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(e.Value) != "first" {
			t.Fatalf("value changed by a lost create: %q", e.Value)
		}
		if e.Key != "job.a" || e.Revision != rev {
			t.Fatalf("entry key/revision: got %q/%d, want job.a/%d", e.Key, e.Revision, rev)
		}
		if e.Created.IsZero() {
			t.Fatalf("entry carries no Created timestamp")
		}
		if e.Generation != f.c.Generation() {
			t.Fatalf("entry generation: got %d, want %d", e.Generation, f.c.Generation())
		}
	})

	t.Run("update with a stale revision is ErrRevisionMismatch", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs

		rev1, err := kv.Create(ctx, "job.a", []byte("v1"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		rev2, err := kv.Update(ctx, "job.a", []byte("v2"), rev1)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := kv.Update(ctx, "job.a", []byte("v3"), rev1); !errors.Is(err, ErrRevisionMismatch) {
			t.Fatalf("stale update: got %v, want ErrRevisionMismatch", err)
		}

		e, err := kv.Get(ctx, "job.a")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(e.Value) != "v2" || e.Revision != rev2 {
			t.Fatalf("value changed by a lost update: %q at %d", e.Value, e.Revision)
		}
	})

	t.Run("delete with a stale revision is ErrRevisionMismatch", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs

		rev1, err := kv.Create(ctx, "job.a", []byte("v1"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		rev2, err := kv.Update(ctx, "job.a", []byte("v2"), rev1)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if err := kv.Delete(ctx, "job.a", rev1); !errors.Is(err, ErrRevisionMismatch) {
			t.Fatalf("stale delete: got %v, want ErrRevisionMismatch", err)
		}
		e, err := kv.Get(ctx, "job.a")
		if err != nil || string(e.Value) != "v2" {
			t.Fatalf("value after a lost delete: %q, %v", e.Value, err)
		}

		if err := kv.Delete(ctx, "job.a", rev2); err != nil {
			t.Fatalf("delete at the current revision: %v", err)
		}
		if _, err := kv.Get(ctx, "job.a"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("get after delete: got %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("get of a deleted key equals get of an absent key", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs

		rev, err := kv.Create(ctx, "job.gone", []byte("x"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := kv.Delete(ctx, "job.gone", rev); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := kv.Get(ctx, "job.gone"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("get of a deleted key: got %v, want ErrKeyNotFound", err)
		}
		if _, err := kv.Get(ctx, "job.never"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("get of an absent key: got %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("keys lists live keys under the prefix", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs

		for _, key := range []string{"job.a", "job.b", "active.a"} {
			if _, err := kv.Create(ctx, key, []byte("x")); err != nil {
				t.Fatalf("create %s: %v", key, err)
			}
		}
		rev, err := kv.Get(ctx, "job.b")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := kv.Delete(ctx, "job.b", rev.Revision); err != nil {
			t.Fatalf("delete: %v", err)
		}

		keys, err := kv.Keys(ctx, "job.")
		if err != nil {
			t.Fatalf("keys: %v", err)
		}
		if len(keys) != 1 || keys[0] != "job.a" {
			t.Fatalf("keys under job.: got %v, want [job.a]", keys)
		}
	})
}

func TestWatch(t *testing.T) {
	t.Run("replay, one marker, live puts, deletes as nil, context end", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs
		gen := f.c.Generation()

		for key, val := range map[string]string{"w.a": "1", "w.b": "2", "x.c": "3"} {
			if _, err := kv.Create(ctx, key, []byte(val)); err != nil {
				t.Fatalf("seed %s: %v", key, err)
			}
		}

		wctx, cancel := context.WithCancel(ctx)
		ch, err := kv.Watch(wctx, "w.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}

		replay := map[string]string{}
		var marker Entry
		for {
			e := nextEntry(t, ch)
			if e.Synced {
				marker = e
				break
			}
			replay[e.Key] = string(e.Value)
			if e.Generation != gen {
				t.Fatalf("replay entry generation: got %d, want %d", e.Generation, gen)
			}
		}
		if len(replay) != 2 || replay["w.a"] != "1" || replay["w.b"] != "2" {
			t.Fatalf("replay delivered %v, want w.a=1 and w.b=2", replay)
		}
		if marker.Key != "" || marker.Generation != gen {
			t.Fatalf("marker: key %q generation %d, want empty key generation %d", marker.Key, marker.Generation, gen)
		}
		if !f.c.Synced(gen) {
			t.Fatalf("Synced(%d) is false after the marker was read", gen)
		}

		if _, err := kv.Create(ctx, "w.new", []byte("4")); err != nil {
			t.Fatalf("live create: %v", err)
		}
		e := nextEntry(t, ch)
		if e.Synced || e.Key != "w.new" || string(e.Value) != "4" {
			t.Fatalf("live change: got %+v, want w.new=4 and no second marker", e)
		}

		got, err := kv.Get(ctx, "w.a")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := kv.Delete(ctx, "w.a", got.Revision); err != nil {
			t.Fatalf("delete: %v", err)
		}
		e = nextEntry(t, ch)
		if e.Key != "w.a" || e.Value != nil {
			t.Fatalf("delete arrived as %+v, want key w.a with a nil value", e)
		}

		cancel()
		waitFor(t, "watch channel close", func() bool {
			select {
			case _, ok := <-ch:
				return !ok
			default:
				return false
			}
		})
	})

	t.Run("marker arrives for an empty prefix", func(t *testing.T) {
		f := startFixture(t)
		ch, err := f.view().Jobs.Watch(t.Context(), "none.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		e := nextEntry(t, ch)
		if !e.Synced || e.Key != "" {
			t.Fatalf("first entry: got %+v, want the marker", e)
		}
	})

	t.Run("a whole-key prefix delivers its create, update, and delete", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Config

		ch, err := kv.Watch(ctx, "probe.one")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if e := nextEntry(t, ch); !e.Synced {
			t.Fatalf("first entry: got %+v, want the marker", e)
		}

		rev, err := kv.Create(ctx, "probe.one", []byte("a"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if e := nextEntry(t, ch); e.Key != "probe.one" || string(e.Value) != "a" {
			t.Fatalf("create delivery: got %+v", e)
		}
		rev, err = kv.Update(ctx, "probe.one", []byte("b"), rev)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if e := nextEntry(t, ch); string(e.Value) != "b" {
			t.Fatalf("update delivery: got %+v", e)
		}
		if err := kv.Delete(ctx, "probe.one", rev); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if e := nextEntry(t, ch); e.Key != "probe.one" || e.Value != nil {
			t.Fatalf("delete delivery: got %+v, want a nil value", e)
		}
	})

	t.Run("a restart re-opens the watch and replays before a fresh marker", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		kv := f.view().Jobs
		gen := f.c.Generation()

		if _, err := kv.Create(ctx, "w.a", []byte("1")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ch, err := kv.Watch(ctx, "w.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		e := nextEntry(t, ch)
		for !e.Synced {
			e = nextEntry(t, ch)
		}

		f.stopServer()
		waitFor(t, "generation move", func() bool { return f.c.Generation() != gen })
		newGen := f.c.Generation()
		f.restartServer()

		e = nextEntry(t, ch)
		if e.Synced || e.Key != "w.a" || string(e.Value) != "1" {
			t.Fatalf("first post-restart entry: got %+v, want the w.a replay", e)
		}
		if e.Generation != newGen {
			t.Fatalf("replayed entry generation: got %d, want %d", e.Generation, newGen)
		}
		e = nextEntry(t, ch)
		if !e.Synced || e.Generation != newGen {
			t.Fatalf("fresh marker: got %+v, want Synced under generation %d", e, newGen)
		}
		if !f.c.Synced(newGen) {
			t.Fatalf("Synced(%d) is false after the fresh marker", newGen)
		}
	})
}

// The three records the seam writes about the store generation and re-opening.
const (
	genMovedRecord      = "nats store generation moved"
	reopenFailingRecord = "nats watch re-open failing"
	reopenedRecord      = "nats watches replayed"
)

func TestGeneration(t *testing.T) {
	t.Run("a watch cut under a live connection moves the generation", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		tap := newWatcherTap()
		f.c.testWatchOpened = tap.record
		held := make(chan struct{})
		defer close(held)
		f.c.testHoldReopen = func(string) { <-held }

		kv := f.view().Jobs
		gen := f.c.Generation()
		ch, err := kv.Watch(ctx, "g.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if e := nextEntry(t, ch); !e.Synced {
			t.Fatalf("first entry: got %+v, want the marker", e)
		}

		tap.cut(t, "g.")
		waitFor(t, "the generation to move", func() bool { return f.c.Generation() != gen })

		if got := f.c.Generation(); got != gen+1 {
			t.Fatalf("generation after the cut: got %d, want %d", got, gen+1)
		}
		if f.c.Synced(gen) {
			t.Fatalf("Synced(%d) is true although the watch over it was cut", gen)
		}
		if !f.c.Connected() {
			t.Fatalf("Connected() is false although the connection never went down")
		}
	})

	t.Run("the re-opened watch replays under the new generation", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		tap := newWatcherTap()
		f.c.testWatchOpened = tap.record
		held := make(chan struct{})
		f.c.testHoldReopen = func(string) { <-held }

		kv := f.view().Jobs
		gen := f.c.Generation()
		if _, err := kv.Create(ctx, "g.a", []byte("1")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ch, err := kv.Watch(ctx, "g.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		drainToMarker(t, ch)

		tap.cut(t, "g.")
		waitFor(t, "the generation to move", func() bool { return f.c.Generation() != gen })
		newGen := f.c.Generation()
		close(held)

		e := nextEntry(t, ch)
		if e.Synced || e.Key != "g.a" || string(e.Value) != "1" {
			t.Fatalf("first entry after the re-open: got %+v, want the g.a replay", e)
		}
		if e.Generation != newGen {
			t.Fatalf("replayed entry generation: got %d, want %d", e.Generation, newGen)
		}
		e = nextEntry(t, ch)
		if !e.Synced || e.Generation != newGen {
			t.Fatalf("fresh marker: got %+v, want Synced under generation %d", e, newGen)
		}
		if !f.c.Synced(newGen) {
			t.Fatalf("Synced(%d) is false after the re-opened watch replayed", newGen)
		}
	})

	t.Run("a key deleted while the watch is down is absent from the replay", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		admin, err := f.js.KeyValue(ctx, jobsBucket)
		if err != nil {
			t.Fatalf("admin bucket: %v", err)
		}
		tap := newWatcherTap()
		f.c.testWatchOpened = tap.record
		held := make(chan struct{})
		f.c.testHoldReopen = func(string) { <-held }

		kv := f.view().Jobs
		gen := f.c.Generation()
		rev, err := kv.Create(ctx, "g.gone", []byte("1"))
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		ch, err := kv.Watch(ctx, "g.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		drainToMarker(t, ch)

		tap.cut(t, "g.")
		waitFor(t, "the generation to move", func() bool { return f.c.Generation() != gen })
		newGen := f.c.Generation()
		if err := admin.Delete(ctx, "g.gone", jetstream.LastRevision(rev)); err != nil {
			t.Fatalf("delete through the admin connection: %v", err)
		}
		close(held)

		for e := nextEntry(t, ch); !e.Synced; e = nextEntry(t, ch) {
			if e.Generation != newGen {
				t.Fatalf("replayed entry generation: got %d, want %d", e.Generation, newGen)
			}
			if e.Key == "g.gone" && e.Value != nil {
				t.Fatalf("the replay carried %q, deleted while the watch was down", e.Key)
			}
		}
		if !f.c.Synced(newGen) {
			t.Fatalf("Synced(%d) is false after the re-opened watch replayed", newGen)
		}
	})

	t.Run("a cut that takes every watch down writes one failing record", func(t *testing.T) {
		f := startServerFixture(t)
		log, capture := captureLogger()
		f.c = f.connectClientLogged(log)
		ctx := t.Context()
		tap := newWatcherTap()
		f.c.testWatchOpened = tap.record
		held := make(chan struct{})
		// Every re-open names the prefix it is on before it waits,
		// so the bucket goes only once all three of the watches the cut took down are held.
		reopens := make(chan string, 16)
		f.c.testHoldReopen = func(prefix string) {
			select {
			case reopens <- prefix:
			default:
			}
			<-held
		}
		var failMu sync.Mutex
		failed := map[string]int{}
		f.c.testReopenFailed = func(prefix string) {
			failMu.Lock()
			defer failMu.Unlock()
			failed[prefix]++
		}
		// retried reports whether every watch has failed an open it made after its first,
		// which is the retry loop running rather than one failure standing still.
		retried := func() (int, bool) {
			failMu.Lock()
			defer failMu.Unlock()
			total, all := 0, true
			for _, prefix := range []string{"job.", "active.", "schedule."} {
				total += failed[prefix]
				all = all && failed[prefix] >= 2
			}

			return total, all
		}

		kv := f.view().Jobs
		prefixes := []string{"job.", "active.", "schedule."}
		for _, prefix := range prefixes {
			ch, err := kv.Watch(ctx, prefix)
			if err != nil {
				t.Fatalf("watch %q: %v", prefix, err)
			}
			drainToMarker(t, ch)
		}
		gen := f.c.Generation()
		if !f.c.Synced(gen) {
			t.Fatalf("Synced(%d) is false after all three watches replayed", gen)
		}

		// The seam re-opens every watch under a new generation,
		// so cutting one subscription takes all three of these down together.
		tap.cut(t, prefixes[0])
		waitFor(t, "the cut to move the generation", func() bool { return f.c.Generation() == gen+1 })

		waiting := map[string]bool{}
		for len(waiting) < len(prefixes) {
			select {
			case prefix := <-reopens:
				waiting[prefix] = true
			case <-time.After(fixtureTimeout):
				t.Fatalf("%d of the three watches reached their re-open: %v", len(waiting), waiting)
			}
		}

		// The bucket goes while the re-opens are held, so every one of them fails on an absent stream.
		if err := f.js.DeleteKeyValue(ctx, jobsBucket); err != nil {
			t.Fatalf("delete bucket: %v", err)
		}
		close(held)

		waitFor(t, "every watch to fail an open past its first", func() bool { _, all := retried(); return all })
		total, _ := retried()
		if n := capture.count(reopenFailingRecord); n != 1 {
			t.Fatalf("failing re-open records over %d failed opens: got %d, want 1", total, n)
		}
		if n := capture.count(genMovedRecord); n != 1 {
			t.Fatalf("generation move records for one cut subscription: got %d, want 1", n)
		}
		if n := capture.count(reopenedRecord); n != 0 {
			t.Fatalf("replayed records while the bucket is absent: got %d, want 0", n)
		}

		if _, err := f.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  jobsBucket,
			History: 1,
			Storage: jetstream.FileStorage,
		}); err != nil {
			t.Fatalf("recreate bucket: %v", err)
		}
		waitFor(t, "the watches to replay again", func() bool { return f.c.Synced(f.c.Generation()) })
		if n := capture.count(reopenedRecord); n != 1 {
			t.Fatalf("replayed records after the bucket returned: got %d, want 1", n)
		}
		if n := capture.count(reopenFailingRecord); n != 1 {
			t.Fatalf("failing re-open records once the bucket returned: got %d, want 1", n)
		}
	})

	t.Run("concurrent cuts report their moves in the order they were made", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		tap := newWatcherTap()
		f.c.testWatchOpened = tap.record

		// The generation is read inside the callback rather than passed to it,
		// because a move that reports while another is still between its increment and its own report
		// hands both of them the higher of the two.
		var mu sync.Mutex
		var seen []uint64
		f.c.onGenerationMove = func() {
			gen := f.c.Generation()
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, gen)
		}

		kv := f.view().Jobs
		prefixes := []string{"job.", "active."}
		for _, prefix := range prefixes {
			ch, err := kv.Watch(ctx, prefix)
			if err != nil {
				t.Fatalf("watch %q: %v", prefix, err)
			}
			// Every replay of every round arrives on these channels,
			// so a reader keeps them from filling and stalling the watch behind them.
			go func() {
				for range ch { //nolint:revive // the entries themselves decide nothing here
				}
			}()
		}
		first := f.c.Generation()
		waitFor(t, "both watches to replay", func() bool { return f.c.Synced(first) })

		// Both subscriptions close together in every round.
		// Whichever watcher reaches its closed channel first moves the generation.
		// The other moves it again whenever it reads its own closed channel before the broadcast the first move sent.
		const rounds = 20
		for range rounds {
			gen := f.c.Generation()
			var wg sync.WaitGroup
			start := make(chan struct{})
			for _, prefix := range prefixes {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					tap.stop(prefix)
				}()
			}
			close(start)
			wg.Wait()
			waitFor(t, "the cut to move the generation", func() bool { return f.c.Generation() > gen })
			waitFor(t, "the watches to replay again", func() bool { return f.c.Synced(f.c.Generation()) })
		}

		last := f.c.Generation()
		mu.Lock()
		defer mu.Unlock()

		if uint64(len(seen)) != last-first {
			t.Fatalf("the callback ran %d times for %d moves, want one report each", len(seen), last-first)
		}
		for i := 1; i < len(seen); i++ {
			if seen[i] <= seen[i-1] {
				t.Fatalf("the callback reported generation %d after %d: a move reported out of its order "+
					"ends the requests of the generation admitted after it", seen[i], seen[i-1])
			}
		}
		// A round in which one watcher read the broadcast before its own closed channel moved the generation once,
		// which orders nothing.
		// The count is what says the rounds above produced the concurrent moves this case is about.
		if last-first <= rounds {
			t.Fatalf("%d cuts over %d rounds moved the generation %d times, want more: "+
				"no round cut two watchers close enough together to move it twice", 2*rounds, rounds, last-first)
		}
	})

	t.Run("the disconnected callback alone moves the generation", func(t *testing.T) {
		f := startFixture(t)
		kv := f.view().Jobs
		gen := f.c.Generation()

		ch, err := kv.Watch(t.Context(), "g.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if e := nextEntry(t, ch); !e.Synced {
			t.Fatalf("first entry: got %+v, want the marker", e)
		}
		if !f.c.Synced(gen) {
			t.Fatalf("Synced(%d) is false before the outage", gen)
		}

		f.stopServer()
		waitFor(t, "generation move", func() bool { return f.c.Generation() != gen })

		newGen := f.c.Generation()
		if newGen != gen+1 {
			t.Fatalf("generation: got %d, want %d", newGen, gen+1)
		}
		if f.c.Synced(newGen) {
			t.Fatalf("Synced(%d) is true before any watch replayed under it", newGen)
		}
		if f.c.Synced(gen) {
			t.Fatalf("Synced(%d) is true for a superseded generation", gen)
		}
		if f.c.Connected() {
			t.Fatalf("Connected() is true while the server is down")
		}
		// The server never comes back in this subtest, so no watch can have
		// been re-opened: nothing arrives on the channel.
		select {
		case e := <-ch:
			t.Fatalf("watch delivered %+v with the server down", e)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("a stale view fails before reaching the server", func(t *testing.T) {
		f := startFixture(t)
		gen := f.c.Generation()
		stores := f.view()

		f.stopServer()
		waitFor(t, "generation move", func() bool { return f.c.Generation() != gen })

		if _, err := f.c.View(gen); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("View(%d): got %v, want ErrUnavailable", gen, err)
		}
		start := time.Now()
		if _, err := stores.Jobs.Get(t.Context(), "job.a"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("get on a stale view: got %v, want ErrUnavailable", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("a stale view waited %s on the server instead of failing at once", elapsed)
		}
	})

	t.Run("View for a non-current generation is ErrUnavailable", func(t *testing.T) {
		f := startFixture(t)
		if _, err := f.c.View(f.c.Generation() + 7); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("View: got %v, want ErrUnavailable", err)
		}
	})

	t.Run("a result arriving after the move is ErrUnavailable whatever the server answered", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		stores := f.view()

		// The named test hook delays the post-call check until the
		// generation has moved, after the server has already answered.
		f.c.testDelayPostCheck = func() {
			f.c.bumpGeneration(errors.New("cut between the call and its result"))
		}
		_, err := stores.Jobs.Create(ctx, "job.cut", []byte("x"))
		f.c.testDelayPostCheck = nil
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("create across a generation move: got %v, want ErrUnavailable", err)
		}

		// The server did answer: the write landed, only the result was discarded.
		e, err := f.view().Jobs.Get(ctx, "job.cut")
		if err != nil || string(e.Value) != "x" {
			t.Fatalf("the server-side write is missing: %v, %v", e, err)
		}
	})
}

func TestObjects(t *testing.T) {
	t.Run("a 40 MiB object round-trips byte for byte", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		obs := f.view().Artifacts

		data := make([]byte, 40<<20)
		for i := range data {
			data[i] = byte(i*31 + 7)
		}
		if err := obs.Put(ctx, "big.pprof", bytes.NewReader(data)); err != nil {
			t.Fatalf("put: %v", err)
		}

		r, err := obs.Get(ctx, "big.pprof")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("object bytes differ: got %d bytes, want %d", len(got), len(data))
		}
	})

	t.Run("get of an absent object is ErrObjectNotFound", func(t *testing.T) {
		f := startFixture(t)
		if _, err := f.view().Artifacts.Get(t.Context(), "absent"); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("get: got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("delete of an absent object is success", func(t *testing.T) {
		f := startFixture(t)
		if err := f.view().Artifacts.Delete(t.Context(), "absent"); err != nil {
			t.Fatalf("delete: got %v, want nil", err)
		}
	})

	t.Run("list returns every object with its mod time", func(t *testing.T) {
		f := startFixture(t)
		ctx := t.Context()
		obs := f.view().Artifacts

		for name, body := range map[string]string{"one.pprof": "aa", "two.pprof": "bbbb"} {
			if err := obs.Put(ctx, name, bytes.NewReader([]byte(body))); err != nil {
				t.Fatalf("put %s: %v", name, err)
			}
		}
		infos, err := obs.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		sizes := map[string]uint64{}
		for _, info := range infos {
			sizes[info.Name] = info.Size
			if info.ModTime.IsZero() || time.Since(info.ModTime) > time.Minute {
				t.Fatalf("object %s mod time %v is not the server's put time", info.Name, info.ModTime)
			}
		}
		if len(infos) != 2 || sizes["one.pprof"] != 2 || sizes["two.pprof"] != 4 {
			t.Fatalf("list: got %v, want one.pprof (2 bytes) and two.pprof (4 bytes)", sizes)
		}
	})

	t.Run("list of an empty bucket is empty", func(t *testing.T) {
		f := startFixture(t)
		infos, err := f.view().Artifacts.List(t.Context())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(infos) != 0 {
			t.Fatalf("list of an empty bucket: got %v", infos)
		}
	})

	t.Run("a 2 MiB object drained over ten seconds returns every byte", func(t *testing.T) {
		f := startFixture(t)
		f.c.callTimeout = time.Second
		data := f.putBytes(t, "slow.pprof", 2<<20)

		r, err := f.view().Artifacts.Get(t.Context(), "slow.pprof")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// 64 reads of 32 KiB, 150 milliseconds apart: about ten seconds for a reader whose deadline is one.
		got := make([]byte, 0, len(data))
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			got = append(got, buf[:n]...)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("read after %d bytes: %v", len(got), err)
			}
			time.Sleep(150 * time.Millisecond)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("object bytes differ: got %d bytes, want %d", len(got), len(data))
		}
	})

	t.Run("a reader that holds one chunk for ten seconds is not failed", func(t *testing.T) {
		f := startFixture(t)
		f.c.callTimeout = time.Second
		name, data, s := stalledAt(t, f, 2)

		r, err := f.view().Artifacts.Get(t.Context(), name)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		first := make([]byte, chunkSize)
		if _, err := io.ReadFull(r, first); err != nil {
			t.Fatalf("read chunk 1: %v", err)
		}
		time.Sleep(10 * time.Second)
		s.free()
		rest, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read the rest: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := append(first, rest...); !bytes.Equal(got, data) {
			t.Fatalf("object bytes differ: got %d bytes, want %d", len(got), len(data))
		}
	})

	t.Run("a store that stops delivering fails the pending read one call deadline into the wait", func(t *testing.T) {
		f := startFixture(t)
		f.c.callTimeout = time.Second
		name, _, s := stalledAt(t, f, 3)
		nuid := f.chunkNUID(t, name)

		r, err := f.view().Artifacts.Get(t.Context(), name)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		if _, err := io.ReadFull(r, make([]byte, 2*chunkSize)); err != nil {
			t.Fatalf("read chunks 1 and 2: %v", err)
		}
		<-s.written

		// The store stops delivering: the chunks left are purged while the pump has not fetched.
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		stream, err := f.js.Stream(ctx, "OBJ_"+artifactsBucket)
		if err != nil {
			t.Fatalf("artifact stream: %v", err)
		}
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject("$O."+artifactsBucket+".C."+nuid)); err != nil {
			t.Fatalf("purge chunks: %v", err)
		}

		result := make(chan error, 1)
		go func() {
			_, err := r.Read(make([]byte, chunkSize))
			result <- err
		}()
		released := time.Now()
		s.free()
		select {
		case err := <-result:
			elapsed := time.Since(released)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("pending read: got %v, want ErrUnavailable", err)
			}
			// The wait is measured from the fetch, not from the Get seconds earlier:
			// the server ends the request at the deadline and the client's own fallback a second later.
			if elapsed < f.c.callTimeout-100*time.Millisecond || elapsed > 2*f.c.callTimeout {
				t.Fatalf("pending read failed %s after the release, want between one and two call deadlines", elapsed)
			}
		case <-time.After(fixtureTimeout):
			t.Fatalf("the pending read did not fail within %s", fixtureTimeout)
		}
	})

	t.Run("a reader whose context ends mid-stream returns the pending read with the cause", func(t *testing.T) {
		f := startFixture(t)
		f.c.callTimeout = time.Second
		name, _, s := stalledAt(t, f, 2)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)

		r, err := f.view().Artifacts.Get(ctx, name)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		if _, err := io.ReadFull(r, make([]byte, chunkSize)); err != nil {
			t.Fatalf("read chunk 1: %v", err)
		}
		<-s.written

		result := make(chan error, 1)
		go func() {
			_, err := r.Read(make([]byte, chunkSize))
			result <- err
		}()
		time.Sleep(50 * time.Millisecond) // the read is pending on a pump that has not fetched
		cause := errors.New("the request left")
		cancel(cause)
		select {
		case err := <-result:
			if !errors.Is(err, cause) {
				t.Fatalf("pending read: got %v, want the cancellation's cause", err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("the pending read did not return within 100ms of the cancellation")
		}
		s.free()
		waitFor(t, "consumer release", func() bool { return f.artifactConsumers(t) == 0 })
	})

	t.Run("a reader closed mid-stream returns the pending read and leaves no consumer", func(t *testing.T) {
		f := startFixture(t)
		f.c.callTimeout = time.Second
		name, _, s := stalledAt(t, f, 2)

		r, err := f.view().Artifacts.Get(t.Context(), name)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if _, err := io.ReadFull(r, make([]byte, chunkSize)); err != nil {
			t.Fatalf("read chunk 1: %v", err)
		}
		<-s.written

		result := make(chan error, 1)
		go func() {
			_, err := r.Read(make([]byte, chunkSize))
			result <- err
		}()
		time.Sleep(50 * time.Millisecond) // the read is pending on a pump that has not fetched
		closed := make(chan error, 1)
		go func() { closed <- r.Close() }()
		select {
		case err := <-result:
			if err == nil {
				t.Fatalf("pending read returned bytes after Close")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("the pending read did not return within 100ms of Close")
		}
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("close: %v", err)
			}
		default:
			t.Fatalf("Close had not returned when the pending read did")
		}
		s.free()
		waitFor(t, "consumer release", func() bool { return f.artifactConsumers(t) == 0 })
	})

	t.Run("a consumer creation that is never answered fails the get within the call deadline and leaves nothing", func(t *testing.T) {
		f := startFixture(t, withUsers(fragmentWithout(t, "publish", "$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>")))
		f.c.callTimeout = time.Second
		f.putBytes(t, "denied.pprof", 16)

		// The denied publish is answered by an asynchronous permission error and never by a response,
		// so the create waits out its context.
		start := time.Now()
		r, err := f.view().Artifacts.Get(t.Context(), "denied.pprof")
		elapsed := time.Since(start)
		if err == nil {
			_ = r.Close()
			t.Fatalf("get with the consumer create denied returned a reader")
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("get: got %v, want ErrUnavailable", err)
		}
		if elapsed > 2*f.c.callTimeout {
			t.Fatalf("get took %s, want it within two call deadlines", elapsed)
		}
		if n := f.artifactConsumers(t); n != 0 {
			t.Fatalf("artifact stream holds %d consumers after a failed get, want 0", n)
		}
	})

	t.Run("a put that takes six seconds under a context with no deadline lands", func(t *testing.T) {
		f := startFixture(t)
		obs := f.view().Artifacts
		data := patternBytes(512 << 10)

		// Four chunks, the reader pausing two seconds after each of the first three:
		// a source slower than the seam's call deadline, under a context that carries none.
		start := time.Now()
		err := obs.Put(context.Background(), "slow-put.pprof", &pausingReader{data: data, pauses: 3, pause: 2 * time.Second})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("put after %s: %v", elapsed, err)
		}
		if elapsed < 6*time.Second {
			t.Fatalf("put took %s, the reader alone paused for six seconds", elapsed)
		}
		r, err := obs.Get(t.Context(), "slow-put.pprof")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("object bytes differ: got %d bytes, want %d", len(got), len(data))
		}
	})

	t.Run("a put cancelled mid-upload is ErrUnavailable and the name is usable again", func(t *testing.T) {
		f := startFixture(t)
		obs := f.view().Artifacts
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		gated := newGatedReader(patternBytes(512 << 10))
		result := make(chan error, 1)
		go func() { result <- obs.Put(ctx, "cancelled.pprof", gated) }()
		<-gated.held
		cancel()
		gated.free()
		released := time.Now()
		select {
		case err := <-result:
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("cancelled put: got %v, want ErrUnavailable", err)
			}
			if elapsed := time.Since(released); elapsed > time.Second {
				t.Fatalf("cancelled put returned %s after the release, want within a second", elapsed)
			}
		case <-time.After(time.Second):
			t.Fatalf("the cancelled put did not return within a second of the release")
		}

		other := []byte("the second upload under the same name")
		if err := obs.Put(t.Context(), "cancelled.pprof", bytes.NewReader(other)); err != nil {
			t.Fatalf("second put: %v", err)
		}
		r, err := obs.Get(t.Context(), "cancelled.pprof")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if !bytes.Equal(got, other) {
			t.Fatalf("object bytes are %q, want the second upload", got)
		}
	})

	t.Run("a put whose acknowledgements are withheld fails while the connection stays up", func(t *testing.T) {
		f := startServerFixture(t)
		tap := f.connectTapped()
		obs := f.view().Artifacts
		gen := f.c.Generation()
		// A cutoff a minute out, set by a timer rather than a deadline, as the work context's is.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cutoff := time.AfterFunc(time.Minute, cancel)
		defer cutoff.Stop()

		gated := newGatedReader(patternBytes(512 << 10))
		result := make(chan error, 1)
		go func() { result <- obs.Put(ctx, "withheld.pprof", gated) }()
		// One chunk is published and the metadata read has been answered;
		// from here the server's acknowledgements never reach the client.
		<-gated.held
		tap.setDropInbox(true)
		t.Cleanup(func() { tap.setDropInbox(false) })
		gated.free()
		released := time.Now()
		select {
		case err := <-result:
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("put with acknowledgements withheld: got %v, want ErrUnavailable", err)
			}
			if elapsed := time.Since(released); elapsed > 20*time.Second {
				t.Fatalf("put failed %s after the release, want within twenty seconds", elapsed)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("the put did not fail within twenty seconds of the release")
		}
		if !f.c.Connected() {
			t.Fatalf("the connection is down after a put whose acknowledgements were withheld")
		}
		if got := f.c.Generation(); got != gen {
			t.Fatalf("generation moved from %d to %d while the connection stayed up", gen, got)
		}
		if ctx.Err() != nil {
			t.Fatalf("the caller's context ended before the put failed")
		}
		tap.setDropInbox(false)
	})

	t.Run("a put whose server goes away fails before a caller's cutoff a minute out", func(t *testing.T) {
		f := startFixture(t)
		obs := f.view().Artifacts
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cutoff := time.AfterFunc(time.Minute, cancel)
		defer cutoff.Stop()

		gated := newGatedReader(patternBytes(512 << 10))
		result := make(chan error, 1)
		go func() { result <- obs.Put(ctx, "gone.pprof", gated) }()
		<-gated.held
		f.stopServer()
		gated.free()
		released := time.Now()
		select {
		case err := <-result:
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("put against a stopped server: got %v, want ErrUnavailable", err)
			}
			if elapsed := time.Since(released); elapsed > 20*time.Second {
				t.Fatalf("put failed %s after the release, want within twenty seconds", elapsed)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("the put did not fail within twenty seconds of the release")
		}
		if ctx.Err() != nil {
			t.Fatalf("the caller's context ended before the put failed")
		}
	})

	t.Run("chunks whose digest is not the metadata's fail the last read rather than EOF", func(t *testing.T) {
		f := startFixture(t)
		data := f.putBytes(t, "forged.pprof", 300<<10)
		f.rewriteDigest(t, "forged.pprof", []byte("other bytes"))

		r, err := f.view().Artifacts.Get(t.Context(), "forged.pprof")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		got, err := io.ReadAll(r)
		if len(got) != len(data) {
			t.Fatalf("read %d bytes before the check, want every one of %d", len(got), len(data))
		}
		if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("read after the last chunk: got %v, want an error naming the digest", err)
		}
	})
}

func TestUnavailable(t *testing.T) {
	t.Run("every call against a stopped server fails within its deadline", func(t *testing.T) {
		f := startFixture(t)
		gen := f.c.Generation()
		f.stopServer()
		waitFor(t, "generation move", func() bool { return f.c.Generation() != gen })
		stores := f.view() // bound to the generation that is current while the server is down

		ops := map[string]func(ctx context.Context) error{
			"kv get": func(ctx context.Context) error {
				_, err := stores.Jobs.Get(ctx, "job.a")
				return err
			},
			"kv create": func(ctx context.Context) error {
				_, err := stores.Jobs.Create(ctx, "job.a", []byte("x"))
				return err
			},
			"kv update": func(ctx context.Context) error {
				_, err := stores.Jobs.Update(ctx, "job.a", []byte("x"), 1)
				return err
			},
			"kv delete": func(ctx context.Context) error {
				return stores.Jobs.Delete(ctx, "job.a", 1)
			},
			"kv keys": func(ctx context.Context) error {
				_, err := stores.Jobs.Keys(ctx, "job.")
				return err
			},
			"kv status": func(ctx context.Context) error {
				st, ok := stores.Jobs.(Statused)
				if !ok {
					return errors.New("the KV view does not implement Statused")
				}
				_, err := st.Status(ctx)
				return err
			},
			"kv watch": func(ctx context.Context) error {
				_, err := stores.Jobs.Watch(ctx, "job.")
				return err
			},
			"object put": func(ctx context.Context) error {
				return stores.Artifacts.Put(ctx, "a", bytes.NewReader([]byte("x")))
			},
			"object get": func(ctx context.Context) error {
				_, err := stores.Artifacts.Get(ctx, "a")
				return err
			},
			"object delete": func(ctx context.Context) error {
				return stores.Artifacts.Delete(ctx, "a")
			},
			"object list": func(ctx context.Context) error {
				_, err := stores.Artifacts.List(ctx)
				return err
			},
			"object status": func(ctx context.Context) error {
				st, ok := stores.Artifacts.(Statused)
				if !ok {
					return errors.New("the Objects view does not implement Statused")
				}
				_, err := st.Status(ctx)
				return err
			},
		}

		start := time.Now()
		var wg sync.WaitGroup
		results := make(chan error, len(ops))
		for name, op := range ops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := op(t.Context())
				if !errors.Is(err, ErrUnavailable) {
					results <- fmt.Errorf("%s: got %w, want ErrUnavailable", name, err)
				}
			}()
		}
		wg.Wait()
		close(results)
		for err := range results {
			t.Error(err)
		}
		// Each call carries a 5-second deadline; running concurrently they
		// must all be back well before two deadlines have passed.
		if elapsed := time.Since(start); elapsed > 2*callTimeout {
			t.Fatalf("calls against a stopped server took %s, deadline is %s", elapsed, callTimeout)
		}
	})
}
