//go:build !race

package pgo

// raceEnabled reports whether the test binary was built with -race.
const raceEnabled = false
