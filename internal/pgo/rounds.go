package pgo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/google/pprof/profile"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/proxy"
)

// profilePath is the one upstream path a Collection fetches.
const profilePath = "/debug/pprof/profile"

// profileKind is the manifest's profile field; a Collection is CPU only.
const profileKind = "cpu"

// The reasons one sample fails.
// A failed sample is recorded and skipped; only a round with zero successes
// fails the Collection.
const (
	ReasonTargetChanged        = "target_changed"
	ReasonDiscoveryUnavailable = "discovery_unavailable"
	ReasonSampleTooLarge       = "sample_too_large"
	ReasonSampleMalformed      = "sample_malformed"
	ReasonParseFailed          = "parse_failed"
	ReasonIncompatibleProfile  = "incompatible_profile"
)

// The two values profgate_collection_samples_total carries.
const (
	sampleOutcomeOK     = "ok"
	sampleOutcomeFailed = "failed"
)

// gzipMagic is the first two bytes of a gzip stream.
var gzipMagic = []byte{0x1f, 0x8b}

// RoundsDeps is everything the work body needs.
// Discovery and Proxy are the gateway's interactive machinery, unchanged:
// the same Confirm and the same transport.
// Sampling takes no admission slot,
// so what bounds it is maxParallel per Collection and nothing shared.
type RoundsDeps struct {
	Discovery    k8s.Discovery
	Proxy        *proxy.Proxy
	Limits       config.PGOLimits
	Clock        Clock
	Recorder     metrics.Recorder
	Log          *slog.Logger
	VersionLabel string
	// Gateway is this replica's instance identifier, recorded in the manifest.
	Gateway string
	// Shuffle orders a round's targets.
	// Production seeds one per worker from crypto/rand; a test passes a fixed
	// sequence so a coverage assertion is deterministic and a failure
	// reproducible.
	Shuffle func(n int, swap func(i, j int))
}

// Rounds samples a Service's Pods round by round, merges what comes back in
// memory, and stores the merged profile.
// It never touches KV: it reports progress and returns what the owner loop
// commits.
type Rounds struct {
	deps RoundsDeps

	// decode, merge, and write are the pprof calls, behind seams so a test can
	// count what they hold, assert what they are given, and fail inside them.
	decode func(data []byte) (*profile.Profile, error)
	merge  func(srcs []*profile.Profile) (*profile.Profile, error)
	write  func(p *profile.Profile, w io.Writer) error
}

// NewRounds returns the work body one worker runs.
func NewRounds(deps RoundsDeps) *Rounds {
	if deps.Shuffle == nil {
		deps.Shuffle = newSeededShuffle()
	}

	return &Rounds{
		deps:   deps,
		decode: profile.ParseData,
		merge:  profile.Merge,
		write:  func(p *profile.Profile, w io.Writer) error { return p.Write(w) },
	}
}

// newSeededShuffle returns a shuffle over a math/rand/v2 generator seeded from
// crypto/rand, so two replicas sampling the same Service pick different subsets.
func newSeededShuffle() func(n int, swap func(i, j int)) {
	var seed [16]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(seed[:])
	// Not a security decision: the generator only spreads which Pods a round
	// samples, and its seed does come from crypto/rand.
	//nolint:gosec // G404: a shuffle, seeded from crypto/rand
	gen := mrand.New(mrand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:])))

	return gen.Shuffle
}

// sampleOutcome is one Pod's contribution, on its way to the merge.
// The profile is nil for every failure.
type sampleOutcome struct {
	sample  Sample
	profile *profile.Profile
}

// run is the work goroutine's body: the round loop, the in-memory merge, and
// the Put of the merged profile.
// NewWorker is the only caller; nothing outside the package drives a
// Collection directly.
func (r *Rounds) run(ctx context.Context, in workInput) workResult {
	rec := in.Record
	policy := rec.Policy
	man := &Manifest{
		Collection:     rec.ID,
		Namespace:      rec.Namespace,
		Service:        rec.Service,
		Profile:        profileKind,
		ConfigRevision: rec.ConfigRevision,
		VersionLabel:   r.deps.VersionLabel,
		Sampling:       policy.Sampling,
		Attempt:        rec.Attempt,
		Gateway:        r.deps.Gateway,
	}
	state := &runState{man: man, progress: Progress{Rounds: policy.Sampling.Rounds}}

	for round := range policy.Sampling.Rounds {
		state.progress.Round = round
		in.Progress(state.progress)

		if reason := r.expired(ctx, rec); reason != "" {
			return state.fail(reason)
		}
		targets, reason := r.targetsFor(ctx, round, rec, state)
		if reason != "" {
			return state.fail(reason)
		}
		if reason := r.runRound(ctx, round, targets, policy, state); reason != "" {
			return state.fail(reason)
		}
		if round < policy.Sampling.Rounds-1 {
			if !r.sleep(ctx, policy.Sampling.RoundInterval.Duration()) {
				return state.fail(ReasonDeadlineExceeded)
			}
		}
	}
	in.Progress(state.progress)

	return r.finish(ctx, in, state)
}

// runState is what one attempt accumulates.
type runState struct {
	man      *Manifest
	progress Progress
	merged   *profile.Profile
	resolved string
}

// fail ends the attempt with a reason, keeping what the manifest already holds
// so a reader can see how far it got.
func (s *runState) fail(reason string) workResult {
	return workResult{
		Reason:          reason,
		Manifest:        s.man,
		ResolvedVersion: s.resolved,
		Progress:        s.progress,
	}
}

// expired reports the reason to stop when the Collection's deadline has passed
// or the owner has cut the work context off.
func (r *Rounds) expired(ctx context.Context, rec Record) string {
	if ctx.Err() != nil {
		return ReasonDeadlineExceeded
	}
	if rec.Deadline != nil && !r.deps.Clock.Now().Before(*rec.Deadline) {
		return ReasonDeadlineExceeded
	}

	return ""
}

// sleep waits out the interval between rounds, or reports false when the work
// context ended first.
func (r *Rounds) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := r.deps.Clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C():
		return true
	case <-ctx.Done():
		return false
	}
}

// targetsFor resolves the Pods one round samples: the gateway's eligibility
// rules, the version filter, the shuffle, and the cap.
// Targets are re-resolved every round, so a Pod that leaves during a rollout
// drops out and a Pod that arrives with the same version joins.
func (r *Rounds) targetsFor(ctx context.Context, round int, rec Record, state *runState) ([]k8s.Target, string) {
	// Always the zero selection: PGO samples the configured pprof port and offers no client selection.
	targets, err := r.deps.Discovery.Targets(ctx, rec.Namespace, rec.Service, k8s.PortSelection{})
	if err != nil {
		return nil, ReasonNoTargets
	}

	if round == 0 {
		resolved, reason := ResolveVersion(targets, rec.Policy.Target.Version)
		if reason != "" {
			return nil, reason
		}
		state.resolved = resolved
		state.man.ResolvedVersion = resolved
	}

	// A Pod of a new version is filtered out by resolvedVersion; when every Pod
	// has rolled, the round finds nothing.
	// Two versions cannot share an artifact by construction.
	targets = filterTargets(targets, func(t k8s.Target) bool { return t.Version == state.resolved })
	targets = dedupeByUID(targets)
	if len(targets) == 0 {
		return nil, ReasonNoTargets
	}

	r.deps.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	want := rec.Policy.Sampling.Replicas.Resolve(r.deps.Limits.MaxTargetsPerRound)
	if len(targets) > want {
		targets = targets[:want]
		// The artifact is a sample of the fleet, not the whole of it.
		state.man.Truncated = true
	}

	return targets, ""
}

// filterTargets keeps the targets a predicate accepts.
func filterTargets(in []k8s.Target, keep func(k8s.Target) bool) []k8s.Target {
	out := in[:0:0]
	for _, t := range in {
		if keep(t) {
			out = append(out, t)
		}
	}

	return out
}

// distinctVersions lists the versions a round's targets carry, in the order
// they first appear.
func distinctVersions(targets []k8s.Target) []string {
	var out []string
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if _, ok := seen[t.Version]; ok {
			continue
		}
		seen[t.Version] = struct{}{}
		out = append(out, t.Version)
	}

	return out
}

// dedupeByUID keeps one entry per Pod, so a Pod is sampled at most once per round.
func dedupeByUID(targets []k8s.Target) []k8s.Target {
	out := targets[:0:0]
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if _, ok := seen[t.UID]; ok {
			continue
		}
		seen[t.UID] = struct{}{}
		out = append(out, t)
	}

	return out
}

// runRound fans out over one round's targets and merges what comes back, one
// sample at a time.
// The targets go to maxParallel sampling goroutines in the order the shuffle
// left them, so a Collection sampling one Pod at a time samples them in that
// order and the first success is the one the target list names first.
// The results channel is unbuffered, so at most maxParallel decoded profiles
// are in flight and the memory figure holds.
func (r *Rounds) runRound(
	ctx context.Context, round int, targets []k8s.Target, policy Policy, state *runState,
) string {
	queue := make(chan k8s.Target)
	results := make(chan sampleOutcome)

	go func() {
		defer close(queue)
		for _, t := range targets {
			select {
			case queue <- t:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range min(policy.Sampling.MaxParallel, len(targets)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range queue {
				out := r.sample(ctx, round, t, policy)
				select {
				case results <- out:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	ok := 0
	failure := ""
	for out := range results {
		// Once the round has failed, the rest of it is drained rather than
		// merged: no sampler is left blocked on its send, and nothing after the
		// failure reaches the running profile.
		if failure != "" {
			continue
		}
		failure = r.absorb(out, state, &ok)
	}
	if failure != "" {
		return failure
	}
	if ok == 0 {
		return ReasonNoSamples
	}

	return ""
}

// absorb folds one sample into the running profile and records it.
// The first success becomes the running profile without a Merge call, because
// profile.Merge reads its first argument's header before anything else.
func (r *Rounds) absorb(out sampleOutcome, state *runState, ok *int) string {
	s := out.sample
	if out.profile != nil {
		if state.merged == nil {
			state.merged = out.profile
		} else if next, err := r.merge([]*profile.Profile{state.merged, out.profile}); err != nil {
			// The running profile is untouched.
			s.Result = ReasonIncompatibleProfile
			s.Bytes = 0
		} else {
			state.merged = next
		}
	}

	if s.Result == sampleResultOK {
		*ok++
		state.progress.SamplesOK++
	} else {
		state.progress.SamplesFailed++
	}
	state.man.Samples = append(state.man.Samples, s)
	r.recordSample(s)

	if s.Result == sampleResultOK && state.merged != nil {
		if size, err := r.serializedSize(state.merged); err != nil || size > r.deps.Limits.MaxMergedBytes {
			if err != nil {
				return ReasonSerializeFailed
			}

			return ReasonMergedTooLarge
		}
	}

	return ""
}

// recordSample emits the one metric row and the one debug record per sample.
// Neither carries a Pod IP or a port.
func (r *Rounds) recordSample(s Sample) {
	result := sampleOutcomeFailed
	if s.Result == sampleResultOK {
		result = sampleOutcomeOK
	}
	r.deps.Recorder.CollectionSample(result)
	r.deps.Log.Debug("collection sample", "pod", s.Pod, "round", s.Round, "result", s.Result, "bytes", s.Bytes)
}

// serializedSize is the size of the object the running profile would be stored
// as, which is what maxMergedBytes bounds.
func (r *Rounds) serializedSize(p *profile.Profile) (int64, error) {
	var counter countingWriter
	if err := r.write(p, &counter); err != nil {
		return 0, err
	}

	return counter.n, nil
}

// countingWriter measures without keeping the bytes.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(b []byte) (int, error) {
	c.n += int64(len(b))

	return len(b), nil
}

// sample fetches one Pod's profile through the gateway's interactive
// machinery: one admission slot, the confirmation, the pinned transport, and a
// bounded in-memory sink.
func (r *Rounds) sample(ctx context.Context, round int, t k8s.Target, policy Policy) sampleOutcome {
	duration := policy.Sampling.Duration.Duration()
	s := Sample{
		Round:     round,
		Pod:       t.Pod,
		PodUID:    t.UID,
		Node:      t.Node,
		StartedAt: r.deps.Clock.Now().UTC(),
	}

	if err := r.deps.Discovery.Confirm(ctx, t); err != nil {
		// Anything the gateway cannot classify is treated as the API server
		// not vouching for the target, exactly as an interactive request does.
		s.Result = ReasonDiscoveryUnavailable
		if errors.Is(err, k8s.ErrTargetChanged) {
			s.Result = ReasonTargetChanged
		}

		return sampleOutcome{sample: s}
	}

	sink := newSampleSink(r.deps.Limits.MaxSampleBytes)
	outcome := r.deps.Proxy.Do(ctx, sink, proxy.Request{
		Target:  t,
		Path:    profilePath,
		Seconds: int(duration / time.Second),
	})
	switch {
	case sink.overflowed:
		// The sink's refusal surfaces from the proxy as a stream failure, so
		// its own flag is read first.
		s.Result = ReasonSampleTooLarge

		return sampleOutcome{sample: s}
	case outcome.Code != "ok":
		// upstream_<status> and the transport's own codes pass through as the
		// sample's reason.
		s.Result = outcome.Code

		return sampleOutcome{sample: s}
	}

	body := sink.Bytes()
	decoded, reason := r.decodeSample(body)
	if reason != "" {
		s.Result = reason

		return sampleOutcome{sample: s}
	}
	s.Result = sampleResultOK
	s.Bytes = int64(len(body))

	return sampleOutcome{sample: s, profile: decoded}
}

// decodeSample turns one body into a profile under a hard input bound.
// Only uncompressed bytes reach the decoder: nested gzip is never decoded,
// because the decoder would otherwise expand it without a bound.
func (r *Rounds) decodeSample(body []byte) (*profile.Profile, string) {
	if bytes.HasPrefix(body, gzipMagic) {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, ReasonSampleMalformed
		}
		defer func() { _ = zr.Close() }()

		limit := r.deps.Limits.MaxSampleBytes
		plain, err := io.ReadAll(io.LimitReader(zr, limit+1))
		switch {
		case int64(len(plain)) > limit:
			// A small body cannot expand past the limit.
			return nil, ReasonSampleTooLarge
		case err != nil:
			return nil, ReasonSampleMalformed
		case bytes.HasPrefix(plain, gzipMagic):
			return nil, ReasonSampleMalformed
		}
		body = plain
	}

	decoded, err := r.decode(body)
	if err != nil {
		return nil, ReasonParseFailed
	}

	return decoded, ""
}

// finish compacts, serializes, and stores the merged profile, then hands the
// object's name to the owner loop.
func (r *Rounds) finish(ctx context.Context, in workInput, state *runState) workResult {
	if state.merged == nil {
		return state.fail(ReasonNoSamples)
	}

	merged := state.merged.Compact()
	var buf bytes.Buffer
	if err := r.write(merged, &buf); err != nil {
		return state.fail(ReasonSerializeFailed)
	}
	if int64(buf.Len()) > r.deps.Limits.MaxMergedBytes {
		return state.fail(ReasonMergedTooLarge)
	}

	// The size is taken before the Put, which consumes the buffer.
	size := int64(buf.Len())
	object := fmt.Sprintf("%s-%d.pprof", in.Record.ID, in.Record.Attempt)
	if err := in.Artifacts.Put(ctx, object, &buf); err != nil {
		r.deps.Log.Warn("pgo: storing the merged profile failed",
			"collection", in.Record.ID, "object", object, "error", err)

		return state.fail(ReasonArtifactStoreFailed)
	}

	return workResult{
		Object:          object,
		Bytes:           size,
		Manifest:        state.man,
		ResolvedVersion: state.resolved,
		Progress:        state.progress,
	}
}

// sampleSink is the only difference between a Collection sample and an
// interactive request: the body's destination is memory, and it stops at
// maxSampleBytes.
type sampleSink struct {
	header     http.Header
	buf        bytes.Buffer
	limit      int64
	overflowed bool
}

// errSampleTooLarge stops the copy once the body passes the limit.
var errSampleTooLarge = errors.New("sample exceeds the sample-size limit")

func newSampleSink(limit int64) *sampleSink {
	return &sampleSink{header: make(http.Header), limit: limit}
}

func (s *sampleSink) Header() http.Header { return s.header }

func (s *sampleSink) WriteHeader(int) {}

func (s *sampleSink) Write(b []byte) (int, error) {
	if int64(s.buf.Len()+len(b)) > s.limit {
		s.overflowed = true

		return 0, errSampleTooLarge
	}

	return s.buf.Write(b)
}

// Bytes is the body as received.
func (s *sampleSink) Bytes() []byte { return s.buf.Bytes() }
