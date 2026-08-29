package pgo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/pprof/profile"

	"github.com/arloliu/profgate/internal/admit"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/natskv"
)

// runRounds drives the work body once and returns its result.
func runRounds(t *testing.T, r *Rounds, in *runInput) workResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()

	return r.run(ctx, in.input())
}

// sampleResults is the result the manifest recorded, keyed by Pod.
func sampleResults(man *Manifest) map[string]string {
	out := make(map[string]string, len(man.Samples))
	for _, s := range man.Samples {
		out[s.Pod] = s.Result
	}

	return out
}

// parseFixture decodes one fixture for a comparison.
func parseFixture(t *testing.T, b []byte) *profile.Profile {
	t.Helper()
	p, err := profile.ParseData(b)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	return p
}

// TestRoundsMergesEverySample proves the happy path end to end: every Pod is
// confirmed and fetched, the samples merge, and the stored object parses with
// the sample count of the fixtures put together.
func TestRoundsMergesEverySample(t *testing.T) {
	fixtureA := fixtureProfile(t, "cpu-a.pprof")
	fixtureB := fixtureProfile(t, "cpu-b.pprof")
	podA := newPodServer(t, "pod-a", "1.42.3", fixtureA)
	podB := newPodServer(t, "pod-b", "1.42.3", fixtureB)

	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(podA.target, podB.target)})
	in := newRunInput(t)

	res := runRounds(t, r, in)
	if res.Reason != "" {
		t.Fatalf("the collection failed %q, want a success", res.Reason)
	}
	if res.ResolvedVersion != "1.42.3" {
		t.Errorf("resolvedVersion is %q, want 1.42.3", res.ResolvedVersion)
	}
	if res.Manifest.Truncated {
		t.Error("the manifest is truncated with two pods under the cap")
	}
	if got := res.Progress; got.SamplesOK != 2 || got.SamplesFailed != 0 {
		t.Errorf("progress is %+v, want two successful samples", got)
	}

	want := fmt.Sprintf("%s-1.pprof", in.record.ID)
	if res.Object != want {
		t.Fatalf("object is %q, want %q", res.Object, want)
	}
	stored, ok := in.artifacts.object(want)
	if !ok {
		t.Fatalf("nothing was stored under %s", want)
	}
	if res.Bytes != int64(len(stored)) {
		t.Errorf("result reports %d bytes, stored %d", res.Bytes, len(stored))
	}

	merged, err := profile.Parse(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the merged object does not parse: %v", err)
	}
	if got, want := len(merged.Sample), len(parseFixture(t, fixtureA).Sample)+len(parseFixture(t, fixtureB).Sample); got != want {
		t.Fatalf("the merged profile has %d samples, want %d", got, want)
	}
}

// TestRoundsVersionIdentity proves round 0 settles the version the whole
// Collection runs with, and refuses to profile a fleet that cannot agree.
func TestRoundsVersionIdentity(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")

	t.Run("no labelled pod is version_missing", func(t *testing.T) {
		pod := newTrapServer(t, "pod-a", "")
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target)})
		if got := runRounds(t, r, newRunInput(t)).Reason; got != ReasonVersionMissing {
			t.Fatalf("reason is %q, want %q", got, ReasonVersionMissing)
		}
	})

	t.Run("two versions are version_conflict", func(t *testing.T) {
		one := newTrapServer(t, "pod-a", "1.42.3")
		two := newTrapServer(t, "pod-b", "1.43.0")
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(one.target, two.target)})
		if got := runRounds(t, r, newRunInput(t)).Reason; got != ReasonVersionConflict {
			t.Fatalf("reason is %q, want %q", got, ReasonVersionConflict)
		}
	})

	t.Run("a new version appearing later is excluded", func(t *testing.T) {
		old := newPodServer(t, "pod-old", "1.42.3", fixture)
		fresh := newTrapServer(t, "pod-new", "1.43.0")
		discovery := newRollingDiscovery(
			[]k8s.Target{old.target},
			[]k8s.Target{old.target, fresh.target},
		)
		r := newTestRounds(t, roundsOpts{discovery: discovery})
		in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.Rounds = 2 })

		res := runRounds(t, r, in)
		if res.Reason != "" {
			t.Fatalf("the collection failed %q, want a success", res.Reason)
		}
		perPod := make(map[string]int)
		for _, s := range res.Manifest.Samples {
			perPod[s.Pod]++
		}
		if len(perPod) != 1 || perPod["pod-old"] != 2 {
			t.Fatalf("samples are %v, want the resolved version's pod twice and nothing else", perPod)
		}
		if res.Manifest.ResolvedVersion != "1.42.3" {
			t.Errorf("manifest resolvedVersion is %q, want 1.42.3", res.Manifest.ResolvedVersion)
		}
	})

	t.Run("every pod rolled is no_targets", func(t *testing.T) {
		old := newPodServer(t, "pod-old", "1.42.3", fixture)
		fresh := newTrapServer(t, "pod-new", "1.43.0")
		discovery := newRollingDiscovery(
			[]k8s.Target{old.target},
			[]k8s.Target{fresh.target},
		)
		r := newTestRounds(t, roundsOpts{discovery: discovery})
		in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.Rounds = 2 })
		if got := runRounds(t, r, in).Reason; got != ReasonNoTargets {
			t.Fatalf("reason is %q, want %q", got, ReasonNoTargets)
		}
	})
}

// TestRoundsTargetSelection proves the shuffle, the replica count, and the cap.
func TestRoundsTargetSelection(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")

	t.Run("replicas two over five pods covers the fleet across rounds", func(t *testing.T) {
		pods := make([]k8s.Target, 0, 5)
		for i := range 5 {
			pods = append(pods, newPodServer(t, fmt.Sprintf("pod-%d", i), "1.42.3", fixture).target)
		}
		// A fixed rotation: each round starts one position further along.
		round := 0
		shuffle := func(n int, swap func(i, j int)) {
			for range round % n {
				for i := range n - 1 {
					swap(i, i+1)
				}
			}
			round++
		}
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pods...), shuffle: shuffle})
		in := newRunInput(t, func(rec *Record) {
			rec.Policy.Sampling.Rounds = 5
			rec.Policy.Sampling.Replicas = ReplicaCount(2)
		})

		res := runRounds(t, r, in)
		if res.Reason != "" {
			t.Fatalf("the collection failed %q, want a success", res.Reason)
		}

		perRound := make(map[int]map[string]struct{})
		union := make(map[string]struct{})
		for _, s := range res.Manifest.Samples {
			if perRound[s.Round] == nil {
				perRound[s.Round] = make(map[string]struct{})
			}
			perRound[s.Round][s.Pod] = struct{}{}
			union[s.Pod] = struct{}{}
		}
		for round, sampled := range perRound {
			if len(sampled) != 2 {
				t.Errorf("round %d sampled %d distinct pods, want 2", round, len(sampled))
			}
		}
		if len(union) != 5 {
			t.Fatalf("five rounds covered %d of the five pods", len(union))
		}
	})

	t.Run("replicas all is capped at maxTargetsPerRound", func(t *testing.T) {
		const limit = 4
		pods := make([]k8s.Target, 0, limit+3)
		for i := range limit + 3 {
			pods = append(pods, newPodServer(t, fmt.Sprintf("pod-%d", i), "1.42.3", fixture).target)
		}
		r := newTestRounds(t, roundsOpts{
			discovery: newFakeDiscovery(pods...),
			limits:    limitsWith(func(l *config.PGOLimits) { l.MaxTargetsPerRound = limit }),
		})
		res := runRounds(t, r, newRunInput(t))
		if res.Reason != "" {
			t.Fatalf("the collection failed %q, want a success", res.Reason)
		}
		if got := len(res.Manifest.Samples); got != limit {
			t.Fatalf("a round sampled %d pods, want the cap of %d", got, limit)
		}
		if !res.Manifest.Truncated {
			t.Fatal("the manifest is not truncated with more eligible pods than the cap")
		}
	})

	t.Run("two production workers order five pods differently", func(t *testing.T) {
		pods := make([]k8s.Target, 0, 5)
		for i := range 5 {
			pods = append(pods, k8s.Target{Pod: fmt.Sprintf("pod-%d", i), UID: fmt.Sprintf("uid-%d", i)})
		}
		one, two := newSeededShuffle(), newSeededShuffle()
		same := 0
		for range 20 {
			a := append([]k8s.Target(nil), pods...)
			b := append([]k8s.Target(nil), pods...)
			one(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
			two(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
			if reflect.DeepEqual(a, b) {
				same++
			}
		}
		// Identical orders 20 times running has probability 120^-20.
		if same == 20 {
			t.Fatal("two production-seeded workers produced the same order 20 times; the seed is constant")
		}
	})
}

// TestRoundsNeverNameAPort proves a Collection resolves every round with the
// zero PortSelection: PGO samples the configured pprof port and offers no
// client selection.
func TestRoundsNeverNameAPort(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")
	pod := newPodServer(t, "pod-a", "1.42.3", fixture)
	discovery := newFakeDiscovery(pod.target)
	r := newTestRounds(t, roundsOpts{discovery: discovery})
	in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.Rounds = 2 })

	if res := runRounds(t, r, in); res.Reason != "" {
		t.Fatalf("the collection failed %q, want a success", res.Reason)
	}
	want := []k8s.PortSelection{{}, {}}
	if got := discovery.selected(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rounds resolved targets with %+v, want one zero selection per round %+v", got, want)
	}
}

// TestRoundsSampleFailures proves the reason vocabulary one failed sample
// records, and that a round survives every one of them.
func TestRoundsSampleFailures(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")

	tests := []struct {
		name       string
		pod        func(t *testing.T) *podServer
		confirmErr error
		want       string
	}{
		{
			name: "an upstream error status passes through",
			pod: func(t *testing.T) *podServer {
				return newPodHandler(t, "pod-bad", "1.42.3", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				})
			},
			want: "upstream_500",
		},
		{
			name: "an unparseable body is parse_failed",
			pod: func(t *testing.T) *podServer {
				return newPodServer(t, "pod-bad", "1.42.3", []byte("not a profile at all"))
			},
			want: ReasonParseFailed,
		},
		{
			name: "a body over the limit is sample_too_large",
			pod: func(t *testing.T) *podServer {
				return newPodServer(t, "pod-bad", "1.42.3", bytes.Repeat([]byte{0x42}, 4096))
			},
			want: ReasonSampleTooLarge,
		},
		{
			name: "gzip inside gzip is sample_malformed",
			pod: func(t *testing.T) *podServer {
				return newPodServer(t, "pod-bad", "1.42.3", gzipBytes(t, gzipBytes(t, fixture)))
			},
			want: ReasonSampleMalformed,
		},
		{
			name:       "a replaced pod is target_changed",
			pod:        func(t *testing.T) *podServer { return newTrapServer(t, "pod-bad", "1.42.3") },
			confirmErr: k8s.ErrTargetChanged,
			want:       ReasonTargetChanged,
		},
		{
			name:       "a cluster that cannot answer is discovery_unavailable",
			pod:        func(t *testing.T) *podServer { return newTrapServer(t, "pod-bad", "1.42.3") },
			confirmErr: k8s.ErrDiscoveryUnavailable,
			want:       ReasonDiscoveryUnavailable,
		},
		{
			// Anything the gateway cannot classify means the API server did
			// not vouch for the target, never that the Pod was replaced.
			name:       "an unclassified confirmation failure is discovery_unavailable",
			pod:        func(t *testing.T) *podServer { return newTrapServer(t, "pod-bad", "1.42.3") },
			confirmErr: errors.New("the api server closed the connection"),
			want:       ReasonDiscoveryUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			good := newPodServer(t, "pod-good", "1.42.3", fixture)
			bad := tc.pod(t)
			discovery := newFakeDiscovery(good.target, bad.target)
			if tc.confirmErr != nil {
				discovery.confirmErr[bad.target.Pod] = tc.confirmErr
			}
			r := newTestRounds(t, roundsOpts{
				discovery: discovery,
				limits:    limitsWith(func(l *config.PGOLimits) { l.MaxSampleBytes = 2048 }),
			})

			res := runRounds(t, r, newRunInput(t))
			if res.Reason != "" {
				t.Fatalf("one failed sample failed the collection %q; the round continues", res.Reason)
			}
			got := sampleResults(res.Manifest)
			if got["pod-bad"] != tc.want {
				t.Fatalf("the failed sample is %q, want %q", got["pod-bad"], tc.want)
			}
			if got["pod-good"] != sampleResultOK {
				t.Fatalf("the good sample is %q, want ok", got["pod-good"])
			}
			if res.Object == "" {
				t.Fatal("nothing was stored; one successful sample is enough")
			}
		})
	}
}

// TestRoundsGzipExpandingPastTheLimit proves the second bound: a small body
// that expands past maxSampleBytes is refused before it is decoded.
func TestRoundsGzipExpandingPastTheLimit(t *testing.T) {
	// Highly compressible: a few hundred bytes on the wire, a megabyte after.
	body := gzipBytes(t, bytes.Repeat([]byte{0x00}, 1<<20))
	if len(body) > 4<<10 {
		t.Fatalf("the compressed body is %d bytes, want under 4 KiB", len(body))
	}

	pod := newPodServer(t, "pod-a", "1.42.3", body)
	r := newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(pod.target),
		limits:    limitsWith(func(l *config.PGOLimits) { l.MaxSampleBytes = 64 << 10 }),
	})
	decoded := 0
	r.decode = func([]byte) (*profile.Profile, error) {
		decoded++

		return nil, errors.New("the decoder must never see these bytes")
	}

	res := runRounds(t, r, newRunInput(t))
	if got := sampleResults(res.Manifest)["pod-a"]; got != ReasonSampleTooLarge {
		t.Fatalf("the sample is %q, want %q", got, ReasonSampleTooLarge)
	}
	if decoded != 0 {
		t.Fatalf("the decoder ran %d times, want never: the limit stops it first", decoded)
	}
}

// TestRoundsNestedGzipNeverDecoded proves nested gzip stops before the decoder.
func TestRoundsNestedGzipNeverDecoded(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")
	pod := newPodServer(t, "pod-a", "1.42.3", gzipBytes(t, gzipBytes(t, fixture)))
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target)})
	decoded := 0
	r.decode = func(b []byte) (*profile.Profile, error) {
		decoded++

		return profile.ParseData(b)
	}

	res := runRounds(t, r, newRunInput(t))
	if got := sampleResults(res.Manifest)["pod-a"]; got != ReasonSampleMalformed {
		t.Fatalf("the sample is %q, want %q", got, ReasonSampleMalformed)
	}
	if decoded != 0 {
		t.Fatalf("the decoder ran %d times, want never", decoded)
	}
}

// fanOutWindow is how long the fan-out is watched for a decoder that must not
// exist, once every sampler a pool of maxParallel can run is held.
// A pool produces nothing more however long the window is; one goroutine per
// target produces the rest within microseconds, because every Pod here is a
// local httptest server that answers at once.
const fanOutWindow = 200 * time.Millisecond

// TestRoundsDecoderInputBounded proves the input side of the memory figure:
// what the decoder holds at once never exceeds maxParallel × maxSampleBytes,
// and the fan-out that bounds it is a pool of maxParallel samplers rather than
// one goroutine per target.
// The byte figure alone would not prove the second: six small fixtures fit
// inside the bound as comfortably as two do, so the count of decoders running
// at once is what pins the pool.
func TestRoundsDecoderInputBounded(t *testing.T) {
	const (
		targets     = 6
		maxParallel = 2
	)
	fixture := fixtureProfile(t, "cpu-a.pprof")
	pods := make([]k8s.Target, 0, targets)
	for i := range targets {
		pods = append(pods, newPodServer(t, fmt.Sprintf("pod-%d", i), "1.42.3", fixture).target)
	}

	limits := limitsWith(func(l *config.PGOLimits) { l.MaxSampleBytes = 1 << 20 })
	// The gate is wider than the target count, so what bounds the fan-out here
	// is the pool and never an admission slot.
	r := newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(pods...), limits: limits, gate: admit.New(targets + 1),
	})

	var (
		mu          sync.Mutex
		held        int64
		peakHeld    int64
		running     int
		peakRunning int
	)
	// Buffered past the target count, so announcing an arrival never blocks a
	// decoder and the count a mutation produces is not hidden by that block.
	arrived := make(chan struct{}, targets)
	hold := make(chan struct{})
	r.decode = func(b []byte) (*profile.Profile, error) {
		mu.Lock()
		held += int64(len(b))
		running++
		peakHeld = max(peakHeld, held)
		peakRunning = max(peakRunning, running)
		mu.Unlock()
		arrived <- struct{}{}
		// Every decoder waits, so the peak is what the fan-out really allows.
		<-hold
		defer func() {
			mu.Lock()
			held -= int64(len(b))
			running--
			mu.Unlock()
		}()

		return profile.ParseData(b)
	}

	in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.MaxParallel = maxParallel })
	done := make(chan workResult, 1)
	go func() { done <- runRounds(t, r, in) }()

	for range maxParallel {
		select {
		case <-arrived:
		case <-time.After(fixtureTimeout):
			t.Fatalf("fewer than %d decoders ran at once within %s", maxParallel, fixtureTimeout)
		}
	}
	// With maxParallel decoders held, a pool has no sampler left to run one
	// more; a goroutine per target has four.
	select {
	case <-arrived:
		t.Fatalf("another decoder ran while %d were held: the fan-out is not bounded by maxParallel",
			maxParallel)
	case <-time.After(fanOutWindow):
	}

	close(hold)
	res := <-done
	if res.Reason != "" {
		t.Fatalf("the collection failed %q", res.Reason)
	}
	if got := len(res.Manifest.Samples); got != targets {
		t.Fatalf("the manifest carries %d samples, want every one of the %d targets", got, targets)
	}
	if bound := int64(maxParallel) * limits.MaxSampleBytes; peakHeld > bound {
		t.Fatalf("the decoder held %d bytes at once, want at most %d", peakHeld, bound)
	}
	if peakRunning != maxParallel {
		t.Fatalf("%d decoders ran at once, want exactly maxParallel %d", peakRunning, maxParallel)
	}
}

// TestRoundsFirstSampleSkipsMerge proves the first success becomes the running
// profile without a Merge call, because Merge reads its first argument's
// header before anything else.
func TestRoundsFirstSampleSkipsMerge(t *testing.T) {
	podA := newPodServer(t, "pod-a", "1.42.3", fixtureProfile(t, "cpu-a.pprof"))
	podB := newPodServer(t, "pod-b", "1.42.3", fixtureProfile(t, "cpu-b.pprof"))
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(podA.target, podB.target)})

	var calls [][]*profile.Profile
	r.merge = func(srcs []*profile.Profile) (*profile.Profile, error) {
		if len(srcs) == 0 || srcs[0] == nil {
			t.Errorf("Merge was given a nil running profile: %v", srcs)
		}
		calls = append(calls, srcs)

		return profile.Merge(srcs)
	}

	if res := runRounds(t, r, newRunInput(t)); res.Reason != "" {
		t.Fatalf("the collection failed %q", res.Reason)
	}
	if len(calls) != 1 {
		t.Fatalf("Merge ran %d times for two samples, want once", len(calls))
	}
	if len(calls[0]) != 2 {
		t.Fatalf("Merge was given %d profiles, want the running one and the sample", len(calls[0]))
	}
}

// TestRoundsIncompatibleProfile proves a sample of a different shape is
// refused and the running profile is left exactly as it was.
func TestRoundsIncompatibleProfile(t *testing.T) {
	cpu := fixtureProfile(t, "cpu-a.pprof")
	good := newPodServer(t, "pod-a", "1.42.3", cpu)
	odd := newPodServer(t, "pod-b", "1.42.3", fixtureProfile(t, "alloc.pprof"))

	// One at a time, so the cpu sample is always the running profile first.
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(good.target, odd.target)})
	in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.MaxParallel = 1 })

	res := runRounds(t, r, in)
	if res.Reason != "" {
		t.Fatalf("an incompatible sample failed the collection %q; the round continues", res.Reason)
	}
	if got := sampleResults(res.Manifest)["pod-b"]; got != ReasonIncompatibleProfile {
		t.Fatalf("the odd sample is %q, want %q", got, ReasonIncompatibleProfile)
	}

	stored, ok := in.artifacts.object(res.Object)
	if !ok {
		t.Fatal("nothing was stored")
	}
	merged, err := profile.Parse(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the stored object does not parse: %v", err)
	}
	if got, want := len(merged.Sample), len(parseFixture(t, cpu).Sample); got != want {
		t.Fatalf("the running profile changed: %d samples, want the cpu fixture's %d", got, want)
	}
}

// TestRoundsMergedTooLarge proves the running profile is measured after every
// merge, and the Collection stops before the next sample is merged.
func TestRoundsMergedTooLarge(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-large.pprof")
	targets := make([]k8s.Target, 0, 3)
	for i := range 3 {
		targets = append(targets, newPodServer(t, fmt.Sprintf("pod-%d", i), "1.42.3", fixture).target)
	}

	r := newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(targets...),
		// Under one fixture's serialized size, so the first sample already
		// overruns it.
		limits: limitsWith(func(l *config.PGOLimits) { l.MaxMergedBytes = 1 << 10 }),
	})
	merges := 0
	r.merge = func(srcs []*profile.Profile) (*profile.Profile, error) {
		merges++

		return profile.Merge(srcs)
	}
	in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.MaxParallel = 1 })

	res := runRounds(t, r, in)
	if res.Reason != ReasonMergedTooLarge {
		t.Fatalf("reason is %q, want %q", res.Reason, ReasonMergedTooLarge)
	}
	if merges != 0 {
		t.Fatalf("Merge ran %d times, want none: the first sample already crossed the limit", merges)
	}
	if in.artifacts.count() != 0 {
		t.Fatal("an object was stored for a collection that failed")
	}
}

// TestRoundsNoSamples proves a round in which nothing succeeded fails the
// Collection.
func TestRoundsNoSamples(t *testing.T) {
	one := newPodServer(t, "pod-a", "1.42.3", []byte("garbage"))
	two := newPodServer(t, "pod-b", "1.42.3", []byte("garbage"))
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(one.target, two.target)})

	res := runRounds(t, r, newRunInput(t))
	if res.Reason != ReasonNoSamples {
		t.Fatalf("reason is %q, want %q", res.Reason, ReasonNoSamples)
	}
	if res.Progress.SamplesFailed != 2 {
		t.Errorf("progress records %d failed samples, want 2", res.Progress.SamplesFailed)
	}
}

// TestRoundsFinishFailures proves the two ways finishing fails, and that
// neither leaves an object behind.
func TestRoundsFinishFailures(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")

	t.Run("a writer that fails is serialize_failed", func(t *testing.T) {
		pod := newPodServer(t, "pod-a", "1.42.3", fixture)
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target)})
		in := newRunInput(t)

		// The size check writes too, so only the final serialization fails.
		calls := 0
		r.write = func(p *profile.Profile, w io.Writer) error {
			calls++
			if calls == 1 {
				return p.Write(w)
			}

			return errors.New("the writer gave up")
		}

		if got := runRounds(t, r, in).Reason; got != ReasonSerializeFailed {
			t.Fatalf("reason is %q, want %q", got, ReasonSerializeFailed)
		}
		if in.artifacts.count() != 0 {
			t.Fatal("an object was stored for a collection that could not serialize")
		}
	})

	t.Run("a store that refuses is artifact_store_failed", func(t *testing.T) {
		pod := newPodServer(t, "pod-a", "1.42.3", fixture)
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target)})
		in := newRunInput(t)
		in.artifacts.putErr = errors.New("the store refused")

		res := runRounds(t, r, in)
		if res.Reason != ReasonArtifactStoreFailed {
			t.Fatalf("reason is %q, want %q", res.Reason, ReasonArtifactStoreFailed)
		}
		if res.Object != "" {
			t.Fatalf("a failed store still named %q", res.Object)
		}
		if in.artifacts.count() != 0 {
			t.Fatal("an object was stored")
		}
	})
}

// TestRoundsDeadlineExceeded proves the work stops when the Collection's
// deadline has passed.
func TestRoundsDeadlineExceeded(t *testing.T) {
	pod := newTrapServer(t, "pod-a", "1.42.3")
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target)})
	in := newRunInput(t, func(rec *Record) {
		past := slotBase.Add(-time.Minute)
		rec.Deadline = &past
	})

	if got := runRounds(t, r, in).Reason; got != ReasonDeadlineExceeded {
		t.Fatalf("reason is %q, want %q", got, ReasonDeadlineExceeded)
	}
}

// TestRoundsDeadlineUsesTheCeiling proves the deadline is computed from
// maxTargetsPerRound and not from the Pods that happen to be live.
func TestRoundsDeadlineUsesTheCeiling(t *testing.T) {
	limits := limitsWith(func(l *config.PGOLimits) { l.MaxTargetsPerRound = 32 })
	policy := schedulerDefaults(t)
	policy.Sampling.Replicas = AllReplicas()
	policy.Sampling.Rounds = 1
	policy.Sampling.MaxParallel = 4
	policy.Sampling.Duration = Duration(30 * time.Second)
	policy.Sampling.RoundInterval = Duration(30 * time.Second)

	got := Deadline(slotBase, policy, limits)
	// batches = ceil(32/4) = 8, admissionWait = duration + roundInterval = 60s.
	want := slotBase.Add(8*(30*time.Second+30*time.Second+60*time.Second) + 60*time.Second)
	if !got.Equal(want) {
		t.Fatalf("deadline is %s, want %s computed from the ceiling", got, want)
	}
}

// TestRoundsObservability proves the per-sample contract: one metric row and
// one debug record per sample, and no Pod address anywhere.
func TestRoundsObservability(t *testing.T) {
	good := newPodServer(t, "pod-a", "1.42.3", fixtureProfile(t, "cpu-a.pprof"))
	alsoGood := newPodServer(t, "pod-b", "1.42.3", fixtureProfile(t, "cpu-b.pprof"))
	bad := newPodServer(t, "pod-c", "1.42.3", []byte("garbage"))

	recorder := newCountingRecorder()
	logs := newLogCapture()
	r := newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(good.target, alsoGood.target, bad.target),
		recorder:  recorder,
		logs:      logs,
	})

	if res := runRounds(t, r, newRunInput(t)); res.Reason != "" {
		t.Fatalf("the collection failed %q", res.Reason)
	}

	want := map[string]int{sampleOutcomeOK: 2, sampleOutcomeFailed: 1}
	if got := recorder.sampleRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sample rows are %v, want %v", got, want)
	}
	records := logs.with("collection sample")
	if len(records) != 3 {
		t.Fatalf("%d sample records logged, want 3", len(records))
	}
	for _, entry := range records {
		if entry.Level != slog.LevelDebug {
			t.Errorf("a sample record is at level %v, want debug", entry.Level)
		}
		for _, key := range []string{"pod", "round", "result", "bytes"} {
			if _, ok := entry.Attrs[key]; !ok {
				t.Errorf("a sample record has no %s: %v", key, entry.Attrs)
			}
		}
	}
	if got := ipv4Pattern.FindString(logs.text()); got != "" {
		t.Errorf("a log record carries %q, which looks like a pod address", got)
	}
}

// TestRoundsGateSlotsAlwaysReturned proves every sampler gives its admission
// slot back: a multi-batch Collection leaves the gate at full capacity.
func TestRoundsGateSlotsAlwaysReturned(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")
	targets := make([]k8s.Target, 0, 6)
	for i := range 6 {
		// A mixture of outcomes, so no release path is left unexercised.
		name := fmt.Sprintf("pod-%d", i)
		switch i % 3 {
		case 0:
			targets = append(targets, newPodServer(t, name, "1.42.3", fixture).target)
		case 1:
			targets = append(targets, newPodServer(t, name, "1.42.3", []byte("garbage")).target)
		default:
			pod := newPodHandler(t, name, "1.42.3", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			targets = append(targets, pod.target)
		}
	}

	const capacity = 4
	gate := admit.New(capacity)
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(targets...), gate: gate})
	in := newRunInput(t, func(rec *Record) {
		rec.Policy.Sampling.MaxParallel = 2
		rec.Policy.Sampling.Rounds = 3
	})

	if res := runRounds(t, r, in); res.Reason != "" {
		t.Fatalf("the collection failed %q", res.Reason)
	}

	releases := make([]func(), 0, capacity)
	for range capacity {
		release, ok := gate.TryAcquire()
		if !ok {
			t.Fatalf("the gate has %d of %d slots left; a sampler never released", len(releases), capacity)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

// TestRoundsSlotTimeout proves a sample that cannot get an admission slot
// inside duration + roundInterval is recorded and skipped.
func TestRoundsSlotTimeout(t *testing.T) {
	pod := newTrapServer(t, "pod-a", "1.42.3")
	gate := admit.New(1)
	held, ok := gate.TryAcquire()
	if !ok {
		t.Fatal("a fresh gate refused its only slot")
	}
	t.Cleanup(held)

	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target), gate: gate})
	in := newRunInput(t, func(rec *Record) {
		rec.Policy.Sampling.Duration = Duration(10 * time.Millisecond)
		rec.Policy.Sampling.RoundInterval = Duration(10 * time.Millisecond)
	})

	res := runRounds(t, r, in)
	if res.Reason != ReasonNoSamples {
		t.Fatalf("reason is %q, want %q", res.Reason, ReasonNoSamples)
	}
	if got := sampleResults(res.Manifest)["pod-a"]; got != ReasonSlotTimeout {
		t.Fatalf("the sample is %q, want %q", got, ReasonSlotTimeout)
	}
}

// TestRoundsCancellationStoresNothing proves a Collection cancelled between
// rounds and one cancelled mid-sample both end with nothing in the store.
func TestRoundsCancellationStoresNothing(t *testing.T) {
	t.Run("between rounds", func(t *testing.T) {
		pod := newPodServer(t, "pod-a", "1.42.3", fixtureProfile(t, "cpu-a.pprof"))
		clock := newFakeClock(slotBase)
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target), clock: clock})
		in := newRunInput(t, func(rec *Record) {
			rec.Policy.Sampling.Rounds = 2
			rec.Policy.Sampling.RoundInterval = Duration(time.Minute)
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan workResult, 1)
		go func() { done <- r.run(ctx, in.input()) }()

		// The interval between rounds is the one timer this run arms,
		// and it is armed only once the first round has recorded its sample.
		// The Pod's hit count rises when the request arrives instead,
		// which is before the profile answering it has been read,
		// and cancelling there ends the Collection with no samples at all.
		waitFor(t, "the run waiting out the interval between rounds", func() bool {
			return clock.armedTimers() == 1
		})
		cancel()

		res := <-done
		if res.Reason != ReasonDeadlineExceeded {
			t.Fatalf("reason is %q, want %q", res.Reason, ReasonDeadlineExceeded)
		}
		if in.artifacts.count() != 0 {
			t.Fatal("an object was stored for a cancelled collection")
		}
	})

	t.Run("mid sample", func(t *testing.T) {
		reached := make(chan struct{})
		var once sync.Once
		pod := newPodHandler(t, "pod-a", "1.42.3", func(_ http.ResponseWriter, req *http.Request) {
			once.Do(func() { close(reached) })
			<-req.Context().Done()
		})
		r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(pod.target)})
		in := newRunInput(t)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan workResult, 1)
		go func() { done <- r.run(ctx, in.input()) }()

		<-reached
		cancel()

		res := <-done
		if res.Object != "" || in.artifacts.count() != 0 {
			t.Fatalf("a cancelled collection stored %q", res.Object)
		}
	})
}

// TestRoundsDecodeHeapDelta is the regression guard on the decoder's
// footprint: parsing a fixture must not cost more than
// config.PGODecodeFactor times its encoded length.
// It is skipped under -race, whose allocator accounting makes the delta
// meaningless.
func TestRoundsDecodeHeapDelta(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's allocator accounting makes a heap delta meaningless")
	}
	// config.PGODecodeFactor multiplies maxSampleBytes, which bounds the decompressed
	// body, so the guard measures against the bytes the decoder is actually
	// handed and not against the gzipped wire form.
	plain := gunzipBytes(t, fixtureProfile(t, "cpu-heap.pprof"))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	parsed, err := profile.ParseData(plain)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	// A collection first, so the delta is what the decoded profile keeps live
	// rather than what parsing allocated and threw away.
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(parsed)

	//nolint:gosec // G115: a heap figure never reaches the top bit of an int64
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if bound := int64(config.PGODecodeFactor * len(plain)); delta > bound {
		t.Fatalf("decoding %d bytes grew the heap by %d, want at most %d", len(plain), delta, bound)
	}
}

// TestSampleSinkStopsAtTheLimit pins the in-memory writer that is the only
// difference between a Collection sample and an interactive request.
func TestSampleSinkStopsAtTheLimit(t *testing.T) {
	sink := newSampleSink(8)
	if _, err := sink.Write([]byte("12345678")); err != nil {
		t.Fatalf("a write at the limit failed: %v", err)
	}
	if _, err := sink.Write([]byte("9")); err == nil {
		t.Fatal("a write past the limit succeeded")
	}
	if !sink.overflowed {
		t.Fatal("the sink did not record the overflow")
	}
	if got := string(sink.Bytes()); got != "12345678" {
		t.Fatalf("the sink holds %q, want the bytes inside the limit", got)
	}
	if !strings.Contains(errSampleTooLarge.Error(), "limit") {
		t.Errorf("the sink's error does not say what happened: %v", errSampleTooLarge)
	}
}

// TestCollectionReclaimStartsFromNothing proves recovery is at-least-once from
// the beginning: the reclaiming attempt's merge holds only its own samples,
// because the first attempt's partial merge lived in the dead owner's memory.
func TestCollectionReclaimStartsFromNothing(t *testing.T) {
	f := startPGO(t)
	fixtureA := fixtureProfile(t, "cpu-a.pprof")
	fixtureB := fixtureProfile(t, "cpu-b.pprof")
	first := newPodServer(t, "pod-a", "1.42.3", fixtureA)
	second := newPodServer(t, "pod-b", "1.42.3", fixtureB)

	// The record already carries a lapsed lease from the attempt that died.
	lapsed := slotBase
	started := slotBase
	deadline := slotBase.Add(time.Hour)
	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		rec.State = StateRunning
		rec.Attempt = 1
		rec.Owner = &Owner{Instance: "dead", Pod: "dead"}
		rec.LeaseUntil = &lapsed
		rec.StartedAt = &started
		rec.Deadline = &deadline
		rec.Policy.Sampling.Rounds = 1
		rec.Policy.Sampling.MaxParallel = 1
		rec.Policy.Sampling.RoundInterval = 0
		rec.Policy.Sampling.Duration = Duration(time.Second)
	})

	r := f.newReplica("replica", replicaOpts{clock: newFakeClock(slotBase.Add(2 * skewMargin))})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	rounds := newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(first.target, second.target),
		clock:     r.clock,
		recorder:  r.recorder,
		logs:      r.logs,
	})
	w := r.newRoundsWorker(rounds)
	scanNow(t, w)

	waitFor(t, "the reclaimed collection completed", func() bool { return f.record(id).State == StateCompleted })
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

	rec := f.record(id)
	if rec.Attempt != 2 {
		t.Fatalf("the record is at attempt %d, want 2", rec.Attempt)
	}
	want := fmt.Sprintf("%s-2.pprof", id)
	if rec.Artifact == nil || rec.Artifact.Object != want {
		t.Fatalf("the completed record names %+v, want the second attempt's object %s", rec.Artifact, want)
	}
	if rec.Manifest == nil || rec.Manifest.Attempt != 2 {
		t.Fatalf("the manifest is %+v, want the second attempt's", rec.Manifest)
	}

	// Only this attempt's two samples, once each.
	if got := len(rec.Manifest.Samples); got != 2 {
		t.Fatalf("the manifest carries %d samples, want the second attempt's 2", got)
	}
	stored := f.readObject(r, want)
	merged, err := profile.Parse(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the stored object does not parse: %v", err)
	}
	expect := len(parseFixture(t, fixtureA).Sample) + len(parseFixture(t, fixtureB).Sample)
	if len(merged.Sample) != expect {
		t.Fatalf("the merge holds %d samples, want %d: exactly this attempt's", len(merged.Sample), expect)
	}
	if _, err := f.getObject(r, fmt.Sprintf("%s-1.pprof", id)); err == nil {
		t.Fatal("the dead attempt's object exists; it never stored one")
	}
}

// TestCollectionRecordTooLarge proves the two ends of the record bound: a
// manifest at the ceiling still fits, and one forced past it ends the
// Collection record_too_large with its object deleted and its samples dropped.
func TestCollectionRecordTooLarge(t *testing.T) {
	t.Run("a manifest at the ceiling fits", func(t *testing.T) {
		rec := ceilingRecord(t)
		value, err := MarshalBounded(rec)
		if err != nil {
			t.Fatalf("a record at the ceiling does not fit: %v", err)
		}
		if len(value) > maxRecordBytes {
			t.Fatalf("the record is %d bytes, want at most %d", len(value), maxRecordBytes)
		}
	})

	t.Run("a record forced past it ends record_too_large", func(t *testing.T) {
		f := startPGO(t)
		pod := newPodServer(t, "pod-a", "1.42.3", fixtureProfile(t, "cpu-a.pprof"))
		id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
			rec.Policy.Sampling.Rounds = 1
			rec.Policy.Sampling.MaxParallel = 1
			rec.Policy.Sampling.RoundInterval = 0
			rec.Policy.Sampling.Duration = Duration(time.Second)
		})

		r := f.newReplica("replica", replicaOpts{})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		rounds := newTestRounds(t, roundsOpts{
			discovery: newFakeDiscovery(pod.target),
			clock:     r.clock,
			recorder:  r.recorder,
			logs:      r.logs,
		})
		w := r.newRoundsWorker(rounds)
		// The work returns a manifest too large for any record to carry.
		inner := w.run
		w.run = func(ctx context.Context, in workInput) workResult {
			res := inner(ctx, in)
			res.Manifest.Samples = oversizeSamples()

			return res
		}

		scanNow(t, w)
		waitFor(t, "the collection ended", func() bool { return terminal(f.record(id).State) })
		waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

		rec := f.record(id)
		if rec.State != StateFailed || rec.Reason != ReasonRecordTooLarge {
			t.Fatalf("record is %q %q, want failed %q", rec.State, rec.Reason, ReasonRecordTooLarge)
		}
		if rec.Artifact != nil {
			t.Fatalf("the terminal record names %+v, want nothing", rec.Artifact)
		}
		if rec.Manifest == nil {
			t.Fatal("the terminal record dropped its manifest entirely; it keeps the counts")
		}
		if len(rec.Manifest.Samples) != 0 {
			t.Fatalf("the terminal record kept %d samples, want none", len(rec.Manifest.Samples))
		}
		if rec.Progress.SamplesOK+rec.Progress.SamplesFailed == 0 {
			t.Fatal("the terminal record kept no sample counts")
		}
		f.waitObjectGone(r, fmt.Sprintf("%s-1.pprof", id))
	})
}

// ceilingRecord is a record with every field at the length the spec bounds it
// by: maxRounds × maxTargetsPerRound samples of maximum-length names.
func ceilingRecord(t *testing.T) Record {
	t.Helper()
	lim := testLimits()
	const (
		nameLen   = 253
		uidLen    = 36
		reasonLen = 32
	)
	samples := make([]Sample, 0, lim.MaxRounds*lim.MaxTargetsPerRound)
	for i := range lim.MaxRounds * lim.MaxTargetsPerRound {
		samples = append(samples, Sample{
			Round:     i % lim.MaxRounds,
			Pod:       strings.Repeat("p", nameLen),
			PodUID:    strings.Repeat("u", uidLen),
			Node:      strings.Repeat("n", nameLen),
			StartedAt: time.Date(2026, 8, 24, 12, 3, 13, 123456789, time.FixedZone("x", 3600)),
			Result:    strings.Repeat("r", reasonLen),
			Bytes:     9223372036854775807,
		})
	}
	started := slotBase
	deadline := slotBase.Add(time.Hour)

	return Record{
		ID:        newID(),
		Namespace: strings.Repeat("n", nameLen),
		Service:   strings.Repeat("s", nameLen),
		Origin:    OriginSchedule,
		Policy:    schedulerDefaults(t),
		State:     StateCompleted,
		Attempt:   3,
		Owner:     &Owner{Instance: strings.Repeat("i", nameLen), Pod: strings.Repeat("p", nameLen)},
		StartedAt: &started,
		Deadline:  &deadline,
		Manifest: &Manifest{
			Collection:      newID(),
			Namespace:       strings.Repeat("n", nameLen),
			Service:         strings.Repeat("s", nameLen),
			Profile:         profileKind,
			ResolvedVersion: strings.Repeat("v", 64),
			VersionLabel:    strings.Repeat("l", 63),
			Gateway:         strings.Repeat("g", nameLen),
			Samples:         samples,
		},
		Artifact:  &ArtifactRef{Object: newID() + "-3.pprof", Bytes: 1 << 30},
		CreatedBy: strings.Repeat("c", nameLen),
		CreatedAt: slotBase,
	}
}

// oversizeSamples is a manifest no record can carry.
func oversizeSamples() []Sample {
	samples := make([]Sample, 0, 4096)
	for i := range 4096 {
		samples = append(samples, Sample{
			Round:  i,
			Pod:    strings.Repeat("p", 253),
			PodUID: strings.Repeat("u", 36),
			Node:   strings.Repeat("n", 253),
			Result: sampleResultOK,
		})
	}

	return samples
}

// TestRoundsLeavesAnInteractiveSlot proves the other half of the shared-gate
// guarantee: while a Collection's fan-out is at maxParallel on the one gate,
// the slot the admission inequality reserves is still free for an interactive
// request.
// internal/httpapi proves the handler's half against the real handler.
func TestRoundsLeavesAnInteractiveSlot(t *testing.T) {
	const (
		capacity    = 3
		maxParallel = 2
	)
	fixture := fixtureProfile(t, "cpu-a.pprof")

	held := make(chan struct{})
	sampling := make(chan struct{}, 8)
	targets := make([]k8s.Target, 0, 4)
	for i := range 4 {
		pod := newPodHandler(t, fmt.Sprintf("pod-%d", i), "1.42.3", func(w http.ResponseWriter, _ *http.Request) {
			sampling <- struct{}{}
			<-held
			//nolint:errcheck // an httptest write that fails fails the assertion instead
			_, _ = w.Write(fixture)
		})
		targets = append(targets, pod.target)
	}

	gate := admit.New(capacity)
	r := newTestRounds(t, roundsOpts{discovery: newFakeDiscovery(targets...), gate: gate})
	in := newRunInput(t, func(rec *Record) { rec.Policy.Sampling.MaxParallel = maxParallel })

	done := make(chan workResult, 1)
	go func() { done <- runRounds(t, r, in) }()

	// Wait until the fan-out is at its limit.
	for range maxParallel {
		select {
		case <-sampling:
		case <-time.After(fixtureTimeout):
			t.Fatal("the fan-out never reached its limit")
		}
	}

	release, ok := gate.TryAcquire()
	if !ok {
		t.Fatalf("no slot was free for an interactive request while %d of %d were held", maxParallel, capacity)
	}
	release()

	close(held)
	if res := <-done; res.Reason != "" {
		t.Fatalf("the collection failed %q", res.Reason)
	}
}

// TestCollectionPutHeldPastTheCutoff proves the guarantee the cutoff makes and
// the one it does not: the owner issues no final update once its committed
// lease has passed, whatever state the work was in, and the object a stale
// attempt stored is its own to delete.
// The barrier sits inside the real Put, which takes no context and runs to
// completion once entered.
func TestCollectionPutHeldPastTheCutoff(t *testing.T) {
	fixture := fixtureProfile(t, "cpu-a.pprof")

	// heldPut installs a barrier inside the Put of one replica's view.
	heldPut := func(release <-chan struct{}, reached chan<- string) *kvHook {
		hook := &kvHook{}
		var once sync.Once
		hook.setBefore(func(op, key string) (error, bool) {
			if op != "put" {
				return nil, false
			}
			once.Do(func() {
				reached <- key
				<-release
			})

			return nil, false
		})

		return hook
	}

	t.Run("without a reclaiming scan nothing is committed", func(t *testing.T) {
		f := startPGO(t)
		pod := newPodServer(t, "pod-a", "1.42.3", fixture)
		// A record a previous attempt already started, so the deadline it
		// carries is short and the reclaim preserves it.
		lapsed := slotBase
		started := slotBase
		deadline := slotBase.Add(30 * time.Second)
		id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
			roundsPolicy(rec)
			rec.State = StateRunning
			rec.Attempt = 1
			rec.Owner = &Owner{Instance: "dead", Pod: "dead"}
			rec.LeaseUntil = &lapsed
			rec.StartedAt = &started
			rec.Deadline = &deadline
		})

		release := make(chan struct{})
		reached := make(chan string, 1)
		hook := heldPut(release, reached)
		r := f.newReplica("replica", replicaOpts{
			clock:      newFakeClock(slotBase.Add(2 * skewMargin)),
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

		rounds := newTestRounds(t, roundsOpts{
			discovery: newFakeDiscovery(pod.target), clock: r.clock, recorder: r.recorder, logs: r.logs,
		})
		w := r.newRoundsWorker(rounds)
		scanNow(t, w)
		claimed := waitClaimed(t, f, id, 2)

		object := <-reached
		if want := fmt.Sprintf("%s-2.pprof", id); object != want {
			t.Fatalf("the work stored %q, want %q", object, want)
		}
		// The Put takes long enough for the deadline the first claimer wrote
		// to pass, while the lease this owner holds is still comfortably valid.
		r.clock.Set(deadline.Add(-skewMargin + time.Second))
		if !r.clock.Now().Before(claimed.LeaseUntil.Add(-skewMargin)) {
			t.Fatalf("the lease had also lapsed at %s; the deadline must be the only gate", r.clock.Now())
		}
		close(release)
		waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

		rec := f.record(id)
		if rec.State != StateRunning || rec.Artifact != nil {
			t.Fatalf("record is %q naming %+v, want running and naming nothing", rec.State, rec.Artifact)
		}
		f.waitObjectGone(r, object)
		if got := r.recorder.collectionRows(); len(got) != 0 {
			t.Fatalf("collection rows are %v, want none", got)
		}
	})

	t.Run("a reclaimer completes and the stale object is never named", func(t *testing.T) {
		f := startPGO(t)
		stalePod := newPodServer(t, "pod-a", "1.42.3", fixture)
		freshPod := newPodServer(t, "pod-b", "1.42.3", fixtureProfile(t, "cpu-b.pprof"))
		id := f.seedClaimable("payment", "payment-api", roundsPolicy)

		release := make(chan struct{})
		reached := make(chan string, 1)
		hook := heldPut(release, reached)
		stale := f.newReplica("replica-stale", replicaOpts{
			wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
		})
		winner := f.newReplica("replica-winner", replicaOpts{})
		for _, r := range []*replica{stale, winner} {
			r.waitSynced()
			r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
		}

		staleWorker := stale.newRoundsWorker(newTestRounds(t, roundsOpts{
			discovery: newFakeDiscovery(stalePod.target),
			clock:     stale.clock, recorder: stale.recorder, logs: stale.logs,
		}))
		scanNow(t, staleWorker)
		claimed := waitClaimed(t, f, id, 1)
		staleObject := <-reached

		// The stale owner's lease lapses while its Put is held, and the other
		// replica reclaims and finishes the Collection.
		winner.clock.Set(claimed.LeaseUntil.Add(2 * skewMargin))
		winnerWorker := winner.newRoundsWorker(newTestRounds(t, roundsOpts{
			discovery: newFakeDiscovery(freshPod.target),
			clock:     winner.clock, recorder: winner.recorder, logs: winner.logs,
		}))
		scanNow(t, winnerWorker)
		waitFor(t, "the reclaimed collection completed", func() bool { return f.record(id).State == StateCompleted })

		// Only now does the stale attempt's Put land.
		stale.clock.Set(claimed.LeaseUntil.Add(time.Second))
		close(release)
		waitFor(t, "the stale owner loop stopped", func() bool { return staleWorker.activeSlots() == 0 })

		rec := f.record(id)
		if rec.Attempt != 2 {
			t.Fatalf("the record is at attempt %d, want the reclaimed 2", rec.Attempt)
		}
		want := fmt.Sprintf("%s-2.pprof", id)
		if rec.Artifact == nil || rec.Artifact.Object != want {
			t.Fatalf("the completed record names %+v, want %s", rec.Artifact, want)
		}
		if rec.Artifact.Object == staleObject {
			t.Fatalf("the committed record names the stale attempt's object %q", staleObject)
		}

		// The winner's bytes are intact and the loser's object is gone.
		stored := f.readObject(winner, want)
		merged, err := profile.Parse(bytes.NewReader(stored))
		if err != nil {
			t.Fatalf("the winner's object does not parse: %v", err)
		}
		if got, want := len(merged.Sample), len(parseFixture(t, fixtureProfile(t, "cpu-b.pprof")).Sample); got != want {
			t.Fatalf("the winner's object holds %d samples, want %d", got, want)
		}
		f.waitObjectGone(stale, staleObject)
	})
}

// TestCollectionMergeAndWriteHeldPastTheCutoff proves the cutoff's reach over
// the two pprof calls that take no context: profile.Merge and the merged
// profile's Write run to completion once entered, and the owner still issues no
// final update once its committed lease has passed, naming nothing.
// Both barriers sit ahead of the Put, so the attempt reaches its deadline with
// nothing stored at all.
func TestCollectionMergeAndWriteHeldPastTheCutoff(t *testing.T) {
	fixtureA := fixtureProfile(t, "cpu-a.pprof")
	fixtureB := fixtureProfile(t, "cpu-b.pprof")

	tests := []struct {
		name string
		// hold installs the barrier inside the call this case is about.
		hold func(r *Rounds, reached chan<- struct{}, release <-chan struct{})
	}{
		{
			name: "inside merge",
			hold: func(r *Rounds, reached chan<- struct{}, release <-chan struct{}) {
				inner := r.merge
				var once sync.Once
				r.merge = func(srcs []*profile.Profile) (*profile.Profile, error) {
					once.Do(func() {
						reached <- struct{}{}
						<-release
					})

					return inner(srcs)
				}
			},
		},
		{
			name: "inside write",
			hold: func(r *Rounds, reached chan<- struct{}, release <-chan struct{}) {
				inner := r.write
				var once sync.Once
				r.write = func(p *profile.Profile, w io.Writer) error {
					// The running profile's size check writes to a counter;
					// only the serialization the object is made of writes to
					// the buffer finish hands it.
					if _, ok := w.(*bytes.Buffer); ok {
						once.Do(func() {
							reached <- struct{}{}
							<-release
						})
					}

					return inner(p, w)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := startPGO(t)
			// Two Pods in the one round, sampled in turn, so the second sample
			// is the one that reaches Merge.
			podA := newPodServer(t, "pod-a", "1.42.3", fixtureA)
			podB := newPodServer(t, "pod-b", "1.42.3", fixtureB)
			id := f.seedClaimable("payment", "payment-api", roundsPolicy)

			r := f.newReplica("replica", replicaOpts{})
			r.waitSynced()
			r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

			rounds := newTestRounds(t, roundsOpts{
				discovery: newFakeDiscovery(podA.target, podB.target),
				clock:     r.clock, recorder: r.recorder, logs: r.logs,
			})
			reached := make(chan struct{}, 1)
			release := make(chan struct{})
			tc.hold(rounds, reached, release)

			w := r.newRoundsWorker(rounds)
			scanNow(t, w)
			claimed := waitClaimed(t, f, id, 1)
			<-reached

			// The call runs on past the committed lease, because nothing can
			// interrupt it: it takes no context.
			r.clock.Set(claimed.LeaseUntil.Add(time.Second))
			close(release)
			waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

			rec := f.record(id)
			if rec.State != StateRunning {
				t.Fatalf("record is %q, want running: no final update may be issued past the cutoff", rec.State)
			}
			if rec.Artifact != nil {
				t.Fatalf("the record names %+v, want nothing", rec.Artifact)
			}
			f.waitObjectGone(r, fmt.Sprintf("%s-1.pprof", id))
			if got := r.recorder.collectionRows(); len(got) != 0 {
				t.Fatalf("collection rows are %v, want none", got)
			}
		})
	}
}

// TestCollectionStalePutLandsFirst proves the other order of the two attempts'
// stores: the stale attempt's object is in the bucket before the reclaimer even
// starts, and it still commits nothing.
// Its lease is valid on its own clock, so it reaches its final update and loses
// it on the revision, which is the branch that deletes the object it wrote.
func TestCollectionStalePutLandsFirst(t *testing.T) {
	f := startPGO(t)
	stalePod := newPodServer(t, "pod-a", "1.42.3", fixtureProfile(t, "cpu-a.pprof"))
	fixtureB := fixtureProfile(t, "cpu-b.pprof")
	freshPod := newPodServer(t, "pod-b", "1.42.3", fixtureB)
	id := f.seedClaimable("payment", "payment-api", roundsPolicy)

	// The stale owner's clock never moves, so no renewal falls between its
	// claim and its final update and the second update on the record is the
	// one the barrier holds.
	var updates atomic.Int64
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	hook := &kvHook{}
	hook.setBefore(func(op, key string) (error, bool) {
		if op == "update" && key == jobKey(id) && updates.Add(1) == 2 {
			reached <- struct{}{}
			<-release
		}

		return nil, false
	})

	stale := f.newReplica("replica-stale", replicaOpts{
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	winner := f.newReplica("replica-winner", replicaOpts{})
	for _, r := range []*replica{stale, winner} {
		r.waitSynced()
		r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })
	}

	staleWorker := stale.newRoundsWorker(newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(stalePod.target),
		clock:     stale.clock, recorder: stale.recorder, logs: stale.logs,
	}))
	scanNow(t, staleWorker)
	claimed := waitClaimed(t, f, id, 1)
	<-reached

	// The stale attempt's object is already stored, before the reclaim.
	staleObject := fmt.Sprintf("%s-1.pprof", id)
	if _, err := f.getObject(stale, staleObject); err != nil {
		t.Fatalf("the stale attempt's object %s is not stored yet: %v", staleObject, err)
	}

	winner.clock.Set(claimed.LeaseUntil.Add(2 * skewMargin))
	winnerWorker := winner.newRoundsWorker(newTestRounds(t, roundsOpts{
		discovery: newFakeDiscovery(freshPod.target),
		clock:     winner.clock, recorder: winner.recorder, logs: winner.logs,
	}))
	scanNow(t, winnerWorker)
	waitFor(t, "the reclaimed collection completed", func() bool { return f.record(id).State == StateCompleted })

	// Only now does the stale attempt's final update run, and lose.
	close(release)
	waitFor(t, "the stale owner loop stopped", func() bool { return staleWorker.activeSlots() == 0 })

	rec := f.record(id)
	want := fmt.Sprintf("%s-2.pprof", id)
	if rec.Attempt != 2 {
		t.Fatalf("the record is at attempt %d, want the reclaimed 2", rec.Attempt)
	}
	if rec.Artifact == nil || rec.Artifact.Object != want {
		t.Fatalf("the completed record names %+v, want the reclaimer's object %s", rec.Artifact, want)
	}

	stored := f.readObject(winner, want)
	merged, err := profile.Parse(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the winner's object does not parse: %v", err)
	}
	if got, want := len(merged.Sample), len(parseFixture(t, fixtureB).Sample); got != want {
		t.Fatalf("the winner's object holds %d samples, want %d", got, want)
	}
	f.waitObjectGone(stale, staleObject)
	if got := stale.recorder.collectionRows(); len(got) != 0 {
		t.Fatalf("the stale owner recorded %v, want nothing: it committed no transition", got)
	}
}

// TestCollectionRenewalMismatchStopsSampling proves a record that moved under
// its owner stops the work before another Pod is dialed: the trap server is
// the Pod the next round would have sampled.
func TestCollectionRenewalMismatchStopsSampling(t *testing.T) {
	f := startPGO(t)
	fixture := fixtureProfile(t, "cpu-a.pprof")

	// The first round samples a Pod that holds the work at a barrier; the
	// second round would dial the trap.
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	var once sync.Once
	first := newPodHandler(t, "pod-first", "1.42.3", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { reached <- struct{}{} })
		<-release
		//nolint:errcheck // an httptest write that fails fails the assertion instead
		_, _ = w.Write(fixture)
	})
	trap := newTrapServer(t, "pod-trap", "1.42.3")

	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		roundsPolicy(rec)
		rec.Policy.Sampling.Rounds = 2
		rec.Policy.Sampling.MaxParallel = 1
	})

	r := f.newReplica("replica", replicaOpts{})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	discovery := newRollingDiscovery(
		[]k8s.Target{first.target},
		[]k8s.Target{trap.target},
	)
	rounds := newTestRounds(t, roundsOpts{
		discovery: discovery, clock: r.clock, recorder: r.recorder, logs: r.logs,
	})
	w := r.newRoundsWorker(rounds)
	scanNow(t, w)
	claimed := waitClaimed(t, f, id, 1)
	<-reached

	// The cancel handler's conditional update advances the revision.
	cancelled := claimed
	cancelled.State = StateCancelled
	cancelled.Reason = "cancelled_by_api"
	finished := r.clock.Now()
	cancelled.FinishedAt = &finished
	f.putJSON(f.jobs, jobKey(id), cancelled)

	// One renewal tick is enough; the trap must never be dialed afterwards.
	r.clock.Advance(testLeaseTTL / 3)
	// Advancing the clock only fires the renewal timer.
	// What stops the work is the renewal reading the moved record,
	// and the owner logs the move it found only after cancelling,
	// so a release before that line lands leaves the second round free to dial.
	waitFor(t, "the owner observing that the record moved", func() bool {
		return len(r.logs.with("pgo: collection moved under its owner")) > 0
	})
	close(release)
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

	if got := trap.hits.Load(); got != 0 {
		t.Fatalf("the trap pprof server was dialed %d times after the record moved", got)
	}
	if got := f.record(id).State; got != StateCancelled {
		t.Fatalf("record is %q, want cancelled", got)
	}
	if got := f.record(id).Artifact; got != nil {
		t.Fatalf("a cancelled collection names %+v, want nothing", got)
	}
}

// TestCollectionUnavailableRenewalStopsSampling proves the other renewal
// failure ends the work just as a lost record does.
// ErrUnavailable commits nothing, so the committed lease stays where the claim
// put it; once it has run out the work is cut off, the next round's Pod is
// never dialed, and the record is left running for another replica's scan.
func TestCollectionUnavailableRenewalStopsSampling(t *testing.T) {
	f := startPGO(t)
	fixture := fixtureProfile(t, "cpu-a.pprof")

	// The first round samples a Pod that holds the work at a barrier; the
	// second round would dial the trap.
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	var once sync.Once
	first := newPodHandler(t, "pod-first", "1.42.3", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { reached <- struct{}{} })
		<-release
		//nolint:errcheck // an httptest write that fails fails the assertion instead
		_, _ = w.Write(fixture)
	})
	trap := newTrapServer(t, "pod-trap", "1.42.3")

	id := f.seedClaimable("payment", "payment-api", func(rec *Record) {
		roundsPolicy(rec)
		rec.Policy.Sampling.Rounds = 2
		rec.Policy.Sampling.MaxParallel = 1
	})

	// Every renewal after the claim answers ErrUnavailable, as a NATS the
	// replica cannot reach would.
	claimed := make(chan struct{})
	hook := &kvHook{}
	hook.setBefore(func(op, key string) (error, bool) {
		if op != "update" || key != jobKey(id) {
			return nil, false
		}
		select {
		case <-claimed:
			return natskv.ErrUnavailable, true
		default:
			// The claim itself.
			return nil, false
		}
	})

	r := f.newReplica("replica", replicaOpts{
		wrapClient: func(c natskv.Client) natskv.Client { return newHookClient(c, hook) },
	})
	r.waitSynced()
	r.waitCache("holds the record", func(c *Caches) bool { return c.hasJob(id) })

	discovery := newRollingDiscovery(
		[]k8s.Target{first.target},
		[]k8s.Target{trap.target},
	)
	rounds := newTestRounds(t, roundsOpts{
		discovery: discovery, clock: r.clock, recorder: r.recorder, logs: r.logs,
	})
	w := r.newRoundsWorker(rounds)
	scanNow(t, w)
	claimedRec := waitClaimed(t, f, id, 1)
	close(claimed)
	<-reached

	// Two renewal ticks, each one refused and observed before the next moves
	// the clock, so the lease really is renewed against an unreachable NATS
	// rather than skipped over.
	// The claim is the first update on the record; a renewal adds one.
	renewals := func() int { return len(hook.callsFor("update", jobKey(id))) }
	for want := 2; want <= 3; want++ {
		r.clock.Advance(testLeaseTTL / 3)
		waitFor(t, "a renewal was attempted", func() bool { return renewals() >= want })
	}
	// The third tick carries the clock past the lease the claim committed,
	// which nothing since has extended.
	r.clock.Advance(testLeaseTTL / 3)
	if now := r.clock.Now(); now.Before(claimedRec.LeaseUntil.Add(-skewMargin)) {
		t.Fatalf("the clock reached %s, short of the cutoff %s", now, claimedRec.LeaseUntil.Add(-skewMargin))
	}
	close(release)
	waitFor(t, "the owner loop stopped", func() bool { return w.activeSlots() == 0 })

	if got := trap.hits.Load(); got != 0 {
		t.Fatalf("the trap pprof server was dialed %d times after the lease ran out", got)
	}
	rec := f.record(id)
	if rec.State != StateRunning {
		t.Fatalf("record is %q, want running: an owner that renewed nothing commits nothing", rec.State)
	}
	if !rec.LeaseUntil.Equal(*claimedRec.LeaseUntil) {
		t.Fatalf("the committed lease is %s, want the claim's %s", rec.LeaseUntil, claimedRec.LeaseUntil)
	}
	if rec.Artifact != nil {
		t.Fatalf("the record names %+v, want nothing", rec.Artifact)
	}
	if got := r.recorder.collectionRows(); len(got) != 0 {
		t.Fatalf("collection rows are %v, want none", got)
	}
	f.waitObjectGone(r, fmt.Sprintf("%s-1.pprof", id))
}

// roundsPolicy is a claimable record's policy for a one-round Collection the
// real work body can run inside a test.
func roundsPolicy(rec *Record) {
	rec.Policy.Sampling.Rounds = 1
	rec.Policy.Sampling.MaxParallel = 1
	rec.Policy.Sampling.RoundInterval = 0
	rec.Policy.Sampling.Duration = Duration(time.Second)
}
