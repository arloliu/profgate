// Package admit holds the one admission gate that bounds how many profiles run at once.
// Interactive requests take a slot without waiting and are refused when none is free;
// Collection samples wait for one until their context ends.
// A single Gate is constructed in cmd/profgate and injected into every caller,
// because two semaphores with the same capacity would let Collections hold every slot.
package admit

import (
	"context"
	"sync"
)

// Gate is a counting semaphore over a buffered channel.
// The zero value is not usable; call New.
type Gate struct {
	slots chan struct{}
}

// New returns a Gate that admits capacity holders at once.
func New(capacity int) *Gate {
	return &Gate{slots: make(chan struct{}, capacity)}
}

// TryAcquire takes a slot without waiting; interactive requests use it (429 when ok is false).
// The returned release gives the slot back and is safe to call more than once.
func (g *Gate) TryAcquire() (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
		return g.releaser(), true
	default:
		return nil, false
	}
}

// Acquire waits for a slot until ctx ends; Collection samples use it.
// It returns the context's error and no release when the wait runs out.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case g.slots <- struct{}{}:
		return g.releaser(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// releaser returns the release func for one held slot.
// The sync.Once is per acquisition, so a caller that releases twice
// gives back the one slot it took and not a slot another holder owns.
func (g *Gate) releaser() func() {
	var once sync.Once

	return func() { once.Do(func() { <-g.slots }) }
}
