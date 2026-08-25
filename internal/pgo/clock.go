package pgo

import "time"

// Clock is the time seam the scheduler, the worker, and the sweeper share.
// A fake drives every time-based test, so no test waits on wall-clock time.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// Timer fires once, and can be reset or stopped before it does.
type Timer interface {
	// C is the channel the timer fires on.
	C() <-chan time.Time
	// Reset restarts the timer for d and reports whether it was still active.
	Reset(d time.Duration) bool
	// Stop prevents the timer from firing and reports whether it was still active.
	Stop() bool
}

// Ticker fires every interval until it is stopped.
type Ticker interface {
	// C is the channel the ticker fires on.
	C() <-chan time.Time
	// Stop releases the ticker; it never closes C.
	Stop()
}

// SystemClock is the process clock, the only Clock outside tests.
type SystemClock struct{}

var _ Clock = SystemClock{}

// Now is the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// NewTimer wraps time.NewTimer.
func (SystemClock) NewTimer(d time.Duration) Timer { return systemTimer{t: time.NewTimer(d)} }

// NewTicker wraps time.NewTicker.
func (SystemClock) NewTicker(d time.Duration) Ticker { return systemTicker{t: time.NewTicker(d)} }

type systemTimer struct{ t *time.Timer }

func (s systemTimer) C() <-chan time.Time        { return s.t.C }
func (s systemTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }
func (s systemTimer) Stop() bool                 { return s.t.Stop() }

type systemTicker struct{ t *time.Ticker }

func (s systemTicker) C() <-chan time.Time { return s.t.C }
func (s systemTicker) Stop()               { s.t.Stop() }
