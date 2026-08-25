package admit

import (
	"context"
	"errors"
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

	t.Run("acquire waits", func(t *testing.T) {
		g := New(1)
		held, ok := g.TryAcquire()
		if !ok {
			t.Fatal("TryAcquire() ok = false, want true")
		}

		acquired := make(chan error, 1)
		go func() {
			_, err := g.Acquire(context.Background())
			acquired <- err
		}()

		select {
		case err := <-acquired:
			t.Fatalf("Acquire() returned %v while the only slot was held, want it to wait", err)
		case <-time.After(50 * time.Millisecond):
		}

		held()
		select {
		case err := <-acquired:
			if err != nil {
				t.Errorf("Acquire() error = %v, want nil once the slot was released", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Acquire() did not return after the slot was released")
		}
	})

	t.Run("acquire context end", func(t *testing.T) {
		g := New(1)
		if _, ok := g.TryAcquire(); !ok {
			t.Fatal("TryAcquire() ok = false, want true")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		release, err := g.Acquire(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Acquire() error = %v, want context.DeadlineExceeded", err)
		}
		if release != nil {
			t.Error("Acquire() returned a release func alongside an error, want nil")
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
		for i := range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					release, ok := g.TryAcquire()
					if ok {
						release()
					}

					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				release, err := g.Acquire(ctx)
				if err != nil {
					return
				}
				release()
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
