package admit

import (
	"sync"
	"testing"
	"time"
)

func TestGate(t *testing.T) {
	t.Run("try at capacity", func(t *testing.T) {
		g := New(1)
		if _, ok := g.TryAcquire(); !ok {
			t.Fatal("first TryAcquire() ok = false, want true")
		}
		start := time.Now()
		if _, ok := g.TryAcquire(); ok {
			t.Error("second TryAcquire() ok = true, want false: the gate is at capacity")
		}
		// A gate that waits at capacity would starve interactive requests instead of refusing them.
		if waited := time.Since(start); waited >= 100*time.Millisecond {
			t.Errorf("second TryAcquire() waited %v, want an immediate refusal", waited)
		}
	})

	t.Run("release frees", func(t *testing.T) {
		g := New(1)
		release, ok := g.TryAcquire()
		if !ok {
			t.Fatal("first TryAcquire() ok = false, want true")
		}
		release()
		if _, ok := g.TryAcquire(); !ok {
			t.Error("TryAcquire() after release ok = false, want true: the slot was not returned")
		}
	})

	t.Run("release idempotent-ish", func(t *testing.T) {
		g := New(2)
		release, ok := g.TryAcquire()
		if !ok {
			t.Fatal("first TryAcquire() ok = false, want true")
		}
		if _, ok := g.TryAcquire(); !ok {
			t.Fatal("second TryAcquire() ok = false, want true")
		}
		release()
		release()
		// The second call must not hand back a slot the caller never took.
		if _, ok := g.TryAcquire(); !ok {
			t.Fatal("TryAcquire() after release ok = false, want true")
		}
		if _, ok := g.TryAcquire(); ok {
			t.Error("TryAcquire() ok = true, want false: a doubled release freed a slot twice")
		}
	})

	t.Run("race", func(t *testing.T) {
		const (
			capacity   = 8
			goroutines = 100
		)
		g := New(capacity)

		var wg sync.WaitGroup
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				release, ok := g.TryAcquire()
				if ok {
					release()
				}
			}()
		}
		wg.Wait()

		// Every slot taken was given back, so the whole capacity is available again and no more.
		var releases []func()
		for range capacity {
			release, ok := g.TryAcquire()
			if !ok {
				t.Fatalf("TryAcquire() ok = false with %d slots taken, want the full capacity of %d back", len(releases), capacity)
			}
			releases = append(releases, release)
		}
		if _, ok := g.TryAcquire(); ok {
			t.Error("TryAcquire() ok = true past capacity, want false: a release freed more than it took")
		}
		for _, release := range releases {
			release()
		}
	})
}
