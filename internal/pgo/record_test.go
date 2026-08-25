package pgo

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
)

// assertSameJSON compares two JSON documents by their decoded values,
// so field order and whitespace do not matter but every value does.
func assertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()

	var wantV, gotV any
	if err := json.Unmarshal(want, &wantV); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if !reflect.DeepEqual(wantV, gotV) {
		t.Fatalf("JSON differs\n got %s\nwant %s", got, want)
	}
}

// specRecord is the Collection record of the spec's *Collections*, "Record".
const specRecord = `{
  "id": "7h2k9m4p6r8t0v1w3x5y",
  "namespace": "payment",
  "service": "payment-api",
  "origin": "schedule",
  "slot": "2026-08-23T12:00:00Z",
  "configRevision": 42,
  "policy": {
    "enabled": true,
    "schedule": {"every": "1h", "jitter": "5m"},
    "sampling": {"duration": "30s", "rounds": 2, "roundInterval": "30s", "replicas": "all", "maxParallel": 4},
    "target": {"versionPolicy": "strict", "version": ""},
    "artifact": {"retention": "2h"}
  },
  "state": "running",
  "attempt": 1,
  "owner": {"instance": "profgate-7f88fdf79-xabcd/q2w3e4r5", "pod": "profgate-7f88fdf79-xabcd"},
  "claimBy": "2026-08-23T13:00:00Z",
  "leaseUntil": "2026-08-23T12:06:12Z",
  "deadline": "2026-08-23T12:20:42Z",
  "reason": "",
  "resolvedVersion": "1.42.3",
  "progress": {"round": 1, "rounds": 2, "samplesOK": 5, "samplesFailed": 0},
  "manifest": null,
  "artifact": null,
  "createdBy": "anonymous",
  "createdAt": "2026-08-23T12:03:12Z",
  "startedAt": "2026-08-23T12:03:13Z",
  "finishedAt": null,
  "expiresAt": null
}`

// specManifest is the manifest of the spec's *Manifest* section.
const specManifest = `{
  "collection": "7h2k9m4p6r8t0v1w3x5y",
  "namespace": "payment",
  "service": "payment-api",
  "profile": "cpu",
  "configRevision": 42,
  "resolvedVersion": "1.42.3",
  "versionLabel": "app.kubernetes.io/version",
  "sampling": {"duration": "30s", "rounds": 2, "roundInterval": "30s", "replicas": "all", "maxParallel": 4},
  "attempt": 1,
  "truncated": false,
  "gateway": "profgate-7f88fdf79-xabcd/q2w3e4r5",
  "samples": [
    {"round": 0, "pod": "payment-api-7c8f8c9b9-a", "podUID": "3c1e", "node": "worker-07", "startedAt": "2026-08-23T12:03:13Z", "result": "ok", "bytes": 48211},
    {"round": 0, "pod": "payment-api-7c8f8c9b9-b", "podUID": "9a0f", "node": "worker-02", "startedAt": "2026-08-23T12:03:13Z", "result": "upstream_timeout", "bytes": 0}
  ]
}`

func TestRecordRoundTrip(t *testing.T) {
	var r Record
	if err := json.Unmarshal([]byte(specRecord), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if r.ID != "7h2k9m4p6r8t0v1w3x5y" || r.State != StateRunning || r.Origin != OriginSchedule {
		t.Fatalf("record = %+v", r)
	}
	if r.ConfigRevision != 42 || r.Attempt != 1 {
		t.Fatalf("record = %+v", r)
	}
	if !r.Policy.Sampling.Replicas.IsAll() || r.Policy.Sampling.MaxParallel != 4 {
		t.Fatalf("policy snapshot = %+v", r.Policy)
	}
	if r.Manifest != nil || r.Artifact != nil || r.FinishedAt != nil || r.ExpiresAt != nil {
		t.Fatal("a running record carries a manifest, an artifact, or an end time")
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSameJSON(t, []byte(specRecord), b)
}

func TestManifestRoundTrip(t *testing.T) {
	var m Manifest
	if err := json.Unmarshal([]byte(specManifest), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if m.Profile != "cpu" || len(m.Samples) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Samples[1].Result != "upstream_timeout" || m.Samples[1].Bytes != 0 {
		t.Fatalf("sample = %+v", m.Samples[1])
	}

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSameJSON(t, []byte(specManifest), b)
}

// maxRecord builds a Collection record with every field at the longest value
// the domain allows and n manifest samples of the same shape.
func maxRecord(n int) Record {
	label := strings.Repeat("a", 63)    // DNS-1123 label: namespace, Service, version
	name := strings.Repeat("b", 253)    // DNS subdomain: Pod and node names
	uid := strings.Repeat("c", 36)      // Kubernetes UID
	reason := strings.Repeat("d", 32)   // the longest sample reason
	result := strings.Repeat("e", 32)   // the longest sample result
	instance := name + "/" + "q2w3e4r5" // Pod name plus the per-process suffix
	versionLabel := strings.Repeat("f", 253) + "/" + label

	startedAt := time.Date(2026, 8, 23, 12, 3, 13, 123456789, time.FixedZone("x", 13*3600+45*60))
	claimBy := startedAt.Add(time.Hour)

	samples := make([]Sample, n)
	for i := range samples {
		samples[i] = Sample{
			Round:     math.MaxInt32,
			Pod:       name,
			PodUID:    uid,
			Node:      name,
			StartedAt: startedAt,
			Result:    result,
			Reason:    reason,
			Bytes:     math.MaxInt64,
		}
	}

	policy, _ := DefaultPolicy(testDefaults())
	policy.Enabled = true
	policy.Target.Version = label

	return Record{
		ID:              strings.Repeat("7", 20),
		Namespace:       label,
		Service:         label,
		Origin:          OriginSchedule,
		Slot:            startedAt.Format(time.RFC3339Nano),
		ConfigRevision:  math.MaxUint64,
		Policy:          policy,
		State:           StateCompleted,
		Attempt:         math.MaxInt32,
		Owner:           &Owner{Instance: instance, Pod: name},
		ClaimBy:         claimBy,
		LeaseUntil:      &claimBy,
		Deadline:        &claimBy,
		Reason:          reason,
		ResolvedVersion: label,
		Progress:        Progress{Round: math.MaxInt32, Rounds: math.MaxInt32, SamplesOK: n, SamplesFailed: n},
		Manifest: &Manifest{
			Collection:      strings.Repeat("7", 20),
			Namespace:       label,
			Service:         label,
			Profile:         "cpu",
			ConfigRevision:  math.MaxUint64,
			ResolvedVersion: label,
			VersionLabel:    versionLabel,
			Sampling:        policy.Sampling,
			Attempt:         math.MaxInt32,
			Truncated:       true,
			Gateway:         instance,
			Samples:         samples,
		},
		Artifact:   &ArtifactRef{Object: strings.Repeat("7", 20) + "-2147483647.pprof", Bytes: math.MaxInt64},
		CreatedBy:  name,
		CreatedAt:  startedAt,
		StartedAt:  &startedAt,
		FinishedAt: &claimBy,
		ExpiresAt:  &claimBy,
	}
}

// TestRecordAtCeiling is the size arithmetic of the spec's *Collections*,
// "Record": maxRounds times maxTargetsPerRound is 256 samples, and a record
// carrying that many at every field's maximum length stays under both the
// spec's 210 KiB figure and the maxRecordBytes constant.
func TestRecordAtCeiling(t *testing.T) {
	b, err := MarshalBounded(maxRecord(256))
	if err != nil {
		t.Fatalf("MarshalBounded: %v", err)
	}

	const specCeiling = 210 << 10
	if len(b) >= specCeiling {
		t.Fatalf("record is %d bytes, past the spec's %d", len(b), specCeiling)
	}
	if len(b) >= maxRecordBytes {
		t.Fatalf("record is %d bytes, past maxRecordBytes %d", len(b), maxRecordBytes)
	}
	t.Logf("256 samples at maximum length serialize to %d bytes", len(b))
}

func TestMarshalBoundedRejectsOversize(t *testing.T) {
	_, err := MarshalBounded(maxRecord(1024))
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("MarshalBounded error = %v, want ErrRecordTooLarge", err)
	}
}

// TestTerminalTooLarge covers the record_too_large form: the terminal record
// drops manifest.samples, keeps their counts, and is itself small enough that
// its own Update cannot fail for the same reason.
func TestTerminalTooLarge(t *testing.T) {
	oversize := maxRecord(1024)
	oversize.Manifest.Samples[0].Result = sampleResultOK
	oversize.Progress = Progress{}

	finishedAt := time.Date(2026, 8, 23, 12, 30, 0, 0, time.UTC)
	got := oversize.terminalTooLarge(finishedAt)

	if got.State != StateFailed || got.Reason != ReasonRecordTooLarge {
		t.Fatalf("terminal record state %q reason %q", got.State, got.Reason)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("finishedAt = %v, want %v", got.FinishedAt, finishedAt)
	}
	if got.Manifest == nil || got.Manifest.Samples != nil {
		t.Fatal("terminal record still carries manifest samples")
	}
	if got.Manifest.Gateway != oversize.Manifest.Gateway || got.Manifest.Attempt != oversize.Manifest.Attempt {
		t.Fatal("terminal record lost the manifest's scalars")
	}
	if got.Progress.SamplesOK != 1 || got.Progress.SamplesFailed != 1023 {
		t.Fatalf("progress = %+v, want 1 ok and 1023 failed", got.Progress)
	}
	if got.Artifact != nil {
		t.Fatal("a failed record names an artifact")
	}
	if oversize.Manifest.Samples == nil {
		t.Fatal("terminalTooLarge modified the record it was called on")
	}

	b, err := MarshalBounded(got)
	if err != nil {
		t.Fatalf("the record_too_large record is itself too large: %v", err)
	}
	t.Logf("terminal record serializes to %d bytes", len(b))
}

// TestDeadline is the formula of the spec's *Collections*, "Record": it reads
// replicas: all as maxTargetsPerRound and never a live target count.
func TestDeadline(t *testing.T) {
	startedAt := time.Date(2026, 8, 23, 12, 3, 13, 0, time.UTC)
	lim := testLimits()

	tests := []struct {
		name     string
		replicas Replicas
		rounds   int
		want     time.Duration
	}{
		// batches = ceil(32/4) = 8; admissionWait = 30s + 30s;
		// 2 * 8 * (30s + 30s + 60s) + 1 * 30s + 60s.
		{name: "all uses maxTargetsPerRound", replicas: AllReplicas(), rounds: 2, want: 2010 * time.Second},
		// batches = ceil(5/4) = 2; 2 * 2 * 120s + 30s + 60s.
		{name: "a count below the ceiling", replicas: ReplicaCount(5), rounds: 2, want: 570 * time.Second},
		// batches = ceil(32/4) = 8, the same as all.
		{name: "a count above the ceiling", replicas: ReplicaCount(40), rounds: 2, want: 2010 * time.Second},
		// 1 * 8 * 120s + 0 + 60s.
		{name: "one round has no interval", replicas: AllReplicas(), rounds: 1, want: 1020 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := DefaultPolicy(testDefaults())
			if err != nil {
				t.Fatalf("DefaultPolicy: %v", err)
			}
			p.Sampling.Replicas = tc.replicas
			p.Sampling.Rounds = tc.rounds

			got := Deadline(startedAt, p, lim)
			if want := startedAt.Add(tc.want); !got.Equal(want) {
				t.Fatalf("deadline = %v, want %v", got, want)
			}
		})
	}

	t.Run("five live Pods do not shorten an all deadline", func(t *testing.T) {
		p, err := DefaultPolicy(testDefaults())
		if err != nil {
			t.Fatalf("DefaultPolicy: %v", err)
		}
		five := p
		five.Sampling.Replicas = ReplicaCount(5)

		if Deadline(startedAt, p, lim).Equal(Deadline(startedAt, five, lim)) {
			t.Fatal("the all deadline equals the five-target deadline; it was computed from a live count")
		}
	})
}

// TestRequiredGracePeriodCoversEveryDeadline holds the two copies of the
// deadline arithmetic against each other.
// config.RequiredPGOGracePeriod is the number `profgate config validate`
// prints and the number an operator sets terminationGracePeriodSeconds from;
// Deadline is what a worker enforces.
// They read the same per-sample overhead, fixed slack, and 10-minute
// roundInterval bound from internal/config, and then repeat the arithmetic
// that combines them with the batching rule in two packages,
// so the printed number is only true while both stay in step.
// For each set of ceilings the test walks every policy Validate admits and
// requires the printed period to cover the longest deadline any of them
// produces, and some admissible policy to reach it exactly.
// It lives in internal/pgo because internal/pgo imports internal/config and
// the reverse direction would be an import cycle,
// so this is the only package that can see both formulas.
func TestRequiredGracePeriodCoversEveryDeadline(t *testing.T) {
	withLimits := func(mutate func(*config.PGOLimits)) config.PGOLimits {
		lim := testLimits()
		mutate(&lim)

		return lim
	}

	tests := []struct {
		name   string
		limits config.PGOLimits
	}{
		{name: "the shipped ceilings", limits: testLimits()},
		{
			name: "targets that do not divide by parallel",
			limits: withLimits(func(l *config.PGOLimits) {
				l.MaxRounds, l.MaxParallel, l.MaxTargetsPerRound = 3, 4, 7
			}),
		},
		{
			name: "no parallelism above one",
			limits: withLimits(func(l *config.PGOLimits) {
				l.MaxRounds, l.MaxParallel, l.MaxTargetsPerRound = 1, 1, 5
			}),
		},
		{
			name: "a long sample duration",
			limits: withLimits(func(l *config.PGOLimits) {
				l.MaxRounds, l.MaxParallel, l.MaxTargetsPerRound = 4, 8, 20
				l.MaxDuration = 5 * time.Minute
			}),
		},
	}

	startedAt := time.Date(2026, 8, 23, 12, 3, 13, 0, time.UTC)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lim := tc.limits
			cfg := &config.Config{PGO: config.PGOConfig{Limits: lim}}
			required := cfg.RequiredPGOGracePeriod()

			replicas := []Replicas{AllReplicas()}
			for n := 1; n <= lim.MaxTargetsPerRound; n++ {
				replicas = append(replicas, ReplicaCount(n))
			}
			durations := []time.Duration{minSamplingDuration, lim.MaxDuration}
			intervals := []time.Duration{0, config.PGOMaxRoundInterval / 2, config.PGOMaxRoundInterval}

			var worst time.Duration
			var admitted int
			for _, r := range replicas {
				for rounds := 1; rounds <= lim.MaxRounds; rounds++ {
					for parallel := 1; parallel <= lim.MaxParallel; parallel++ {
						for _, d := range durations {
							for _, interval := range intervals {
								p := Policy{
									Enabled:  true,
									Schedule: Schedule{Every: Duration(lim.MinEvery)},
									Sampling: Sampling{
										Duration:      Duration(d),
										Rounds:        rounds,
										RoundInterval: Duration(interval),
										Replicas:      r,
										MaxParallel:   parallel,
									},
									Target:   TargetPolicy{VersionPolicy: versionPolicyStrict},
									Artifact: Artifact{Retention: Duration(lim.MaxRetention)},
								}
								if v := Validate(p, lim); len(v) != 0 {
									continue
								}
								admitted++
								if got := Deadline(startedAt, p, lim).Sub(startedAt); got > worst {
									worst = got
								}
							}
						}
					}
				}
			}

			if admitted == 0 {
				t.Fatal("no candidate policy was admissible, so the walk proves nothing")
			}
			if worst > required {
				t.Errorf("the longest admissible deadline is %v, more than the printed grace period %v", worst, required)
			}
			if worst != required {
				t.Errorf("the printed grace period is %v but no admissible policy reaches past %v, so it is not the worst case", required, worst)
			}
		})
	}
}
