package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand/v2"
	"time"
)

// backoff is the wait between two attempts of one retry loop:
// the wait doubles from preflightBackoffFirst to preflightBackoffCap,
// and each wait is drawn from the upper half of its schedule
// so replicas that lost one dependency at one moment do not retry it in step afterwards.
type backoff struct {
	next  time.Duration
	rng   *mrand.Rand
	sleep func(ctx context.Context, d time.Duration) error
}

// newBackoff returns one loop's place on that schedule.
// A nil rng seeds a generator of this backoff's own from crypto/rand,
// and a nil sleep waits on a timer.
// A generator serves one goroutine at a time, so every loop builds a backoff of its own.
func newBackoff(rng *mrand.Rand, sleep func(ctx context.Context, d time.Duration) error) *backoff {
	if rng == nil {
		rng = newBackoffRand()
	}
	if sleep == nil {
		sleep = sleepFor
	}

	return &backoff{next: preflightBackoffFirst, rng: rng, sleep: sleep}
}

// draw returns the wait before the next attempt and advances the schedule.
func (b *backoff) draw() time.Duration {
	d := b.next
	b.next = min(b.next*2, preflightBackoffCap)

	return d/2 + time.Duration(b.rng.Int64N(int64(d/2)+1))
}

// wait sleeps the drawn wait, and returns the context's error when ctx ends first.
func (b *backoff) wait(ctx context.Context, d time.Duration) error {
	return b.sleep(ctx, d)
}

// sleepFor is the wait a loop makes in production.
func sleepFor(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newBackoffRand seeds one math/rand/v2 generator from crypto/rand for one retry loop's waits.
func newBackoffRand() *mrand.Rand {
	var seed [16]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(seed[:])
	// Not a security decision: the generator only spreads a retry over its band,
	// and its seed does come from crypto/rand.
	//nolint:gosec // G404: a wait, seeded from crypto/rand
	return mrand.New(mrand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:])))
}
