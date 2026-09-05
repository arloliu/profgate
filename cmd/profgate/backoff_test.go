package main

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// backoffDraws is how many waits every backoff row reads,
// which is far enough past the fifth to hold the cap for fifteen draws.
const backoffDraws = 20

// backoffBand is the kth figure of the retry schedule: the whole a draw at that place may reach.
func backoffBand(k int) time.Duration {
	return min(preflightBackoffFirst<<k, preflightBackoffCap)
}

// inBackoffBand reports whether d lies within the kth band, between half of its figure and the whole of it.
func inBackoffBand(k int, d time.Duration) bool {
	whole := backoffBand(k)

	return d >= whole/2 && d <= whole
}

// assertBackoffDraws checks each wait against the band of its place on the schedule, one case per wait.
func assertBackoffDraws(t *testing.T, draws []time.Duration) {
	t.Helper()
	if len(draws) != backoffDraws {
		t.Fatalf("waits = %d, want %d: %v", len(draws), backoffDraws, draws)
	}
	for k, d := range draws {
		t.Run(fmt.Sprintf("wait %d is in band %s", k, backoffBand(k)), func(t *testing.T) {
			if !inBackoffBand(k, d) {
				t.Fatalf("wait %d: got %s, want within [%s, %s]", k, d, backoffBand(k)/2, backoffBand(k))
			}
		})
	}
	jittered := false
	for k, d := range draws {
		if d > preflightBackoffCap {
			t.Fatalf("wait %d: got %s, above the %s cap", k, d, preflightBackoffCap)
		}
		if d < backoffBand(k) {
			jittered = true
		}
	}
	if !jittered {
		t.Fatalf("every wait is the whole of its band, so nothing was drawn: %v", draws)
	}
}

func TestBackoff(t *testing.T) {
	t.Run("twenty draws lie in their bands and reach the cap", func(t *testing.T) {
		//nolint:gosec // G404: a test's reproducible draws, seeded from a fixed pair
		b := newBackoff(rand.New(rand.NewPCG(1, 2)), nil)
		draws := make([]time.Duration, 0, backoffDraws)
		for range backoffDraws {
			draws = append(draws, b.draw())
		}
		assertBackoffDraws(t, draws)

		// Two loops that lost the same dependency at the same moment retry it at different moments.
		//nolint:gosec // G404: a test's reproducible draws, seeded from a second fixed pair
		other := newBackoff(rand.New(rand.NewPCG(3, 4)), nil)
		differs := false
		for _, d := range draws {
			if other.draw() != d {
				differs = true
			}
		}
		if !differs {
			t.Fatalf("a differently seeded backoff drew the same twenty waits: %v", draws)
		}
	})
}
