//go:build race

package pgo

// raceEnabled reports whether the test binary was built with -race.
// The heap-delta guard is meaningless under the race detector's allocator
// accounting, so it reads this and skips.
const raceEnabled = true
