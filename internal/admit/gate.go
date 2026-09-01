// Package admit holds the one admission gate that bounds how many profiles run at once.
// A caller takes a slot without waiting and is refused when none is free.
// Collection sampling does not pass through it:
// what bounds a Collection is its own maxParallel, per Collection.
package admit

import "sync"

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

// releaser returns the release func for one held slot.
// The sync.Once is per acquisition, so a caller that releases twice
// gives back the one slot it took and not a slot another holder owns.
func (g *Gate) releaser() func() {
	var once sync.Once

	return func() { once.Do(func() { <-g.slots }) }
}
