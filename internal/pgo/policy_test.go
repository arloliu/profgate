package pgo

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
)

// testLimits is the shipped pgo.limits block of the spec's configuration example.
func testLimits() config.PGOLimits {
	return config.PGOLimits{
		MaxDuration:          60 * time.Second,
		MaxRounds:            5,
		MaxParallel:          4,
		MinEvery:             15 * time.Minute,
		MaxEvery:             24 * time.Hour,
		MaxRetention:         24 * time.Hour,
		MaxSampleBytes:       33554432,
		MaxMergedBytes:       67108864,
		MaxTargetsPerRound:   32,
		MaxActiveCollections: 2,
		OnDemandPerMinute:    10,
		MaxLiveCollections:   64,
	}
}

// testDefaults is the shipped pgo.defaults block of the spec's configuration example.
func testDefaults() config.PGODefaults {
	return config.PGODefaults{
		Schedule: config.PGOScheduleDefaults{Every: 6 * time.Hour, Jitter: 10 * time.Minute},
		Sampling: config.PGOSamplingDefaults{
			Duration:      30 * time.Second,
			Rounds:        2,
			RoundInterval: 30 * time.Second,
			Replicas:      "all",
			MaxParallel:   4,
		},
		Target:   config.PGOTargetDefaults{VersionPolicy: "strict"},
		Artifact: config.PGOArtifactDefaults{Retention: 24 * time.Hour},
	}
}

// basePolicy is the effective policy of a Service with no override.
func basePolicy(t *testing.T) Policy {
	t.Helper()

	p, err := DefaultPolicy(testDefaults())
	if err != nil {
		t.Fatalf("DefaultPolicy: %v", err)
	}

	return p
}

// parseOverride is how the gateway reads a stored override or a request body.
func parseOverride(t *testing.T, body string) *PolicyOverride {
	t.Helper()

	var o PolicyOverride
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatalf("unmarshal override %s: %v", body, err)
	}

	return &o
}

func TestDefaultPolicy(t *testing.T) {
	t.Run("enabled has no default", func(t *testing.T) {
		if basePolicy(t).Enabled {
			t.Fatal("defaults enabled PGO; scheduling a Service is always an explicit override")
		}
	})

	t.Run("replicas all", func(t *testing.T) {
		if got := basePolicy(t).Sampling.Replicas; !got.IsAll() {
			t.Fatalf("replicas = %v, want all", got)
		}
	})

	t.Run("replicas count", func(t *testing.T) {
		d := testDefaults()
		d.Sampling.Replicas = "3"

		p, err := DefaultPolicy(d)
		if err != nil {
			t.Fatalf("DefaultPolicy: %v", err)
		}
		if p.Sampling.Replicas != ReplicaCount(3) {
			t.Fatalf("replicas = %v, want 3", p.Sampling.Replicas)
		}
	})

	t.Run("unparsable replicas is an error, not all", func(t *testing.T) {
		d := testDefaults()
		d.Sampling.Replicas = "3x"

		p, err := DefaultPolicy(d)
		if err == nil {
			t.Fatalf("DefaultPolicy accepted %q and produced %v", d.Sampling.Replicas, p.Sampling.Replicas)
		}
	})
}

func TestEffectiveLayering(t *testing.T) {
	defaults := basePolicy(t)

	tests := []struct {
		name string
		body string
		want func(Policy) Policy
	}{
		{
			name: "one block one field",
			body: `{"sampling": {"rounds": 3}}`,
			want: func(p Policy) Policy { p.Sampling.Rounds = 3; return p },
		},
		{
			name: "null field is unset",
			body: `{"sampling": {"rounds": null, "duration": "10s"}}`,
			want: func(p Policy) Policy { p.Sampling.Duration = Duration(10 * time.Second); return p },
		},
		{
			name: "null block is unset",
			body: `{"sampling": null}`,
			want: func(p Policy) Policy { return p },
		},
		{
			name: "enabled alone",
			body: `{"enabled": true}`,
			want: func(p Policy) Policy { p.Enabled = true; return p },
		},
		{
			name: "null enabled is unset",
			body: `{"enabled": null}`,
			want: func(p Policy) Policy { return p },
		},
		{
			name: "the spec's stored override",
			body: `{"enabled": true, "schedule": {"every": "1h", "jitter": "5m"}, "sampling": {"rounds": 3}}`,
			want: func(p Policy) Policy {
				p.Enabled = true
				p.Schedule.Every = Duration(time.Hour)
				p.Schedule.Jitter = Duration(5 * time.Minute)
				p.Sampling.Rounds = 3

				return p
			},
		},
		{
			name: "replicas as a count",
			body: `{"sampling": {"replicas": 3}}`,
			want: func(p Policy) Policy { p.Sampling.Replicas = ReplicaCount(3); return p },
		},
		{
			name: "replicas as all",
			body: `{"sampling": {"replicas": "all"}}`,
			want: func(p Policy) Policy { p.Sampling.Replicas = AllReplicas(); return p },
		},
		{
			name: "version pin",
			body: `{"target": {"version": "1.42.3"}}`,
			want: func(p Policy) Policy { p.Target.Version = "1.42.3"; return p },
		},
		{
			name: "retention",
			body: `{"artifact": {"retention": "4h"}}`,
			want: func(p Policy) Policy { p.Artifact.Retention = Duration(4 * time.Hour); return p },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Effective(defaults, parseOverride(t, tc.body))
			if want := tc.want(defaults); !reflect.DeepEqual(got, want) {
				t.Fatalf("effective policy\n got %+v\nwant %+v", got, want)
			}
		})
	}

	t.Run("no override", func(t *testing.T) {
		if got := Effective(defaults, nil); !reflect.DeepEqual(got, defaults) {
			t.Fatalf("effective policy\n got %+v\nwant %+v", got, defaults)
		}
	})
}

func TestValidateAccepts(t *testing.T) {
	if v := Validate(basePolicy(t), testLimits()); len(v) != 0 {
		t.Fatalf("shipped defaults reported violations: %+v", v)
	}
}

func TestValidateCeilings(t *testing.T) {
	tests := []struct {
		name string
		// limits replaces the shipped ceilings where a case needs an interval the default retention still covers,
		// so the row measures one bound and no other.
		limits  config.PGOLimits
		mutate  func(Policy) Policy
		field   string
		code    string
		ceiling string
	}{
		{
			name:    "every above maxEvery",
			limits:  limitsWith(func(l *config.PGOLimits) { l.MaxEvery = time.Hour }),
			mutate:  func(p Policy) Policy { p.Schedule.Every = Duration(2 * time.Hour); return p },
			field:   "schedule.every",
			code:    codeAboveMaximum,
			ceiling: "pgo.limits.maxEvery",
		},
		{
			name: "every below minEvery",
			mutate: func(p Policy) Policy {
				p.Schedule.Every, p.Schedule.Jitter = Duration(time.Minute), 0

				return p
			},
			field:   "schedule.every",
			code:    codeBelowMinimum,
			ceiling: "pgo.limits.minEvery",
		},
		{
			name:    "jitter above half of every",
			mutate:  func(p Policy) Policy { p.Schedule.Jitter = Duration(4 * time.Hour); return p },
			field:   "schedule.jitter",
			code:    codeOutOfRange,
			ceiling: "schedule.every/2",
		},
		{
			name:    "duration above maxDuration",
			mutate:  func(p Policy) Policy { p.Sampling.Duration = Duration(61 * time.Second); return p },
			field:   "sampling.duration",
			code:    codeAboveMaximum,
			ceiling: "pgo.limits.maxDuration",
		},
		{
			name:    "duration below one second",
			mutate:  func(p Policy) Policy { p.Sampling.Duration = 0; return p },
			field:   "sampling.duration",
			code:    codeBelowMinimum,
			ceiling: "1s",
		},
		{
			name:    "rounds above maxRounds",
			mutate:  func(p Policy) Policy { p.Sampling.Rounds = 6; return p },
			field:   "sampling.rounds",
			code:    codeAboveMaximum,
			ceiling: "pgo.limits.maxRounds",
		},
		{
			name:    "rounds below one",
			mutate:  func(p Policy) Policy { p.Sampling.Rounds = 0; return p },
			field:   "sampling.rounds",
			code:    codeBelowMinimum,
			ceiling: "1",
		},
		{
			name:    "roundInterval above ten minutes",
			mutate:  func(p Policy) Policy { p.Sampling.RoundInterval = Duration(11 * time.Minute); return p },
			field:   "sampling.roundInterval",
			code:    codeOutOfRange,
			ceiling: "10m",
		},
		{
			name:    "replicas above maxTargetsPerRound",
			mutate:  func(p Policy) Policy { p.Sampling.Replicas = ReplicaCount(33); return p },
			field:   "sampling.replicas",
			code:    codeAboveMaximum,
			ceiling: "pgo.limits.maxTargetsPerRound",
		},
		{
			name:    "replicas below one",
			mutate:  func(p Policy) Policy { p.Sampling.Replicas = ReplicaCount(0); return p },
			field:   "sampling.replicas",
			code:    codeBelowMinimum,
			ceiling: "1",
		},
		{
			name:    "replicas unset",
			mutate:  func(p Policy) Policy { p.Sampling.Replicas = Replicas{}; return p },
			field:   "sampling.replicas",
			code:    codeBelowMinimum,
			ceiling: "1",
		},
		{
			name:    "maxParallel above the ceiling",
			mutate:  func(p Policy) Policy { p.Sampling.MaxParallel = 5; return p },
			field:   "sampling.maxParallel",
			code:    codeAboveMaximum,
			ceiling: "pgo.limits.maxParallel",
		},
		{
			name:    "maxParallel below one",
			mutate:  func(p Policy) Policy { p.Sampling.MaxParallel = 0; return p },
			field:   "sampling.maxParallel",
			code:    codeBelowMinimum,
			ceiling: "1",
		},
		{
			name:    "versionPolicy is not strict",
			mutate:  func(p Policy) Policy { p.Target.VersionPolicy = "loose"; return p },
			field:   "target.versionPolicy",
			code:    codeNotPermitted,
			ceiling: "strict",
		},
		{
			name:    "retention above maxRetention",
			mutate:  func(p Policy) Policy { p.Artifact.Retention = Duration(25 * time.Hour); return p },
			field:   "artifact.retention",
			code:    codeAboveMaximum,
			ceiling: "pgo.limits.maxRetention",
		},
		{
			name:    "retention below one minute",
			mutate:  func(p Policy) Policy { p.Artifact.Retention = Duration(30 * time.Second); return p },
			field:   "artifact.retention",
			code:    codeBelowMinimum,
			ceiling: "1m",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lim := testLimits()
			if tc.limits != (config.PGOLimits{}) {
				lim = tc.limits
			}

			got := Validate(tc.mutate(basePolicy(t)), lim)
			if len(got) != 1 {
				t.Fatalf("violations = %+v, want exactly one", got)
			}
			if got[0].Field != tc.field || got[0].Ceiling != tc.ceiling {
				t.Fatalf("violation = %+v, want field %q ceiling %q", got[0], tc.field, tc.ceiling)
			}
			if got[0].Code != tc.code {
				t.Errorf("violation code = %q, want %q", got[0].Code, tc.code)
			}
			if got[0].Detail == "" {
				t.Fatal("violation carries no detail")
			}
		})
	}
}

// TestValidateRetentionCoversInterval pins the one rule that judges an effective policy against itself.
// An artifact kept for less than its own interval leaves the Service with nothing to download for the tail of each one,
// which is what a build asking for the newest profile finds.
// The rule reads the effective policy,
// so an override that sets one of the two fields is judged against the default of the other.
func TestValidateRetentionCoversInterval(t *testing.T) {
	t.Run("retention under the interval", func(t *testing.T) {
		p := basePolicy(t)
		p.Schedule.Every = Duration(6 * time.Hour)
		p.Artifact.Retention = Duration(time.Hour)

		got := Validate(p, testLimits())

		if len(got) != 1 {
			t.Fatalf("violations = %+v, want exactly one", got)
		}
		if got[0].Field != "artifact.retention" || got[0].Code != codeRetentionUnderInterval {
			t.Fatalf("violation = %+v, want artifact.retention %q", got[0], codeRetentionUnderInterval)
		}
		if got[0].Ceiling != "schedule.every" {
			t.Errorf("ceiling = %q, want the sibling field the value is measured against", got[0].Ceiling)
		}
		if !strings.Contains(got[0].Detail, "1h") || !strings.Contains(got[0].Detail, "6h") {
			t.Errorf("detail = %q, want both values named", got[0].Detail)
		}
	})

	// An interval and a retention of the same length keep one artifact downloadable at every moment,
	// which is what the rule asks for.
	t.Run("retention equal to the interval", func(t *testing.T) {
		p := basePolicy(t)
		p.Schedule.Every = Duration(6 * time.Hour)
		p.Artifact.Retention = Duration(6 * time.Hour)

		if got := Validate(p, testLimits()); len(got) != 0 {
			t.Fatalf("violations = %+v, want none", got)
		}
	})

	t.Run("retention above the interval", func(t *testing.T) {
		p := basePolicy(t)
		p.Schedule.Every = Duration(6 * time.Hour)
		p.Artifact.Retention = Duration(12 * time.Hour)

		if got := Validate(p, testLimits()); len(got) != 0 {
			t.Fatalf("violations = %+v, want none", got)
		}
	})

	// Layering is one level deep per block,
	// so an override naming only the interval is judged against the default retention,
	// and the other way round.
	t.Run("an override raising only the interval", func(t *testing.T) {
		defaults := testDefaults()
		defaults.Schedule.Every = time.Hour
		defaults.Artifact.Retention = 2 * time.Hour
		base, err := DefaultPolicy(defaults)
		if err != nil {
			t.Fatalf("DefaultPolicy: %v", err)
		}

		got := Validate(Effective(base, parseOverride(t, `{"schedule": {"every": "6h"}}`)), testLimits())

		if len(got) != 1 || got[0].Code != codeRetentionUnderInterval {
			t.Fatalf("violations = %+v, want one %q", got, codeRetentionUnderInterval)
		}
	})

	t.Run("an override lowering only the retention", func(t *testing.T) {
		got := Validate(Effective(basePolicy(t), parseOverride(t, `{"artifact": {"retention": "1h"}}`)), testLimits())

		if len(got) != 1 || got[0].Code != codeRetentionUnderInterval {
			t.Fatalf("violations = %+v, want one %q", got, codeRetentionUnderInterval)
		}
	})

	// The rule is reported beside a ceiling rather than in place of it,
	// each on its own field and in the order the policy declares them.
	t.Run("beside a ceiling violation", func(t *testing.T) {
		p := basePolicy(t)
		p.Sampling.Rounds = 99
		p.Artifact.Retention = Duration(time.Hour)

		got := Validate(p, testLimits())

		want := []Violation{
			{Field: "sampling.rounds", Code: codeAboveMaximum},
			{Field: "artifact.retention", Code: codeRetentionUnderInterval},
		}
		if len(got) != len(want) {
			t.Fatalf("violations = %+v, want two", got)
		}
		for i, w := range want {
			if got[i].Field != w.Field || got[i].Code != w.Code {
				t.Errorf("violation %d = %+v, want field %q code %q", i, got[i], w.Field, w.Code)
			}
		}
	})

	// A retention under the floor is one fault on one field rather than two:
	// the floor is the value the writer is asked to raise first.
	t.Run("a retention under the floor keeps its own message", func(t *testing.T) {
		p := basePolicy(t)
		p.Artifact.Retention = Duration(30 * time.Second)

		got := Validate(p, testLimits())

		if len(got) != 1 || got[0].Code != codeBelowMinimum {
			t.Fatalf("violations = %+v, want one %q", got, codeBelowMinimum)
		}
	})
}

// TestValidateLoweredCeiling is the read side: an override written under a
// higher ceiling is still stored when the ceiling drops, and reading it names
// the field and the ceiling it now exceeds.
func TestValidateLoweredCeiling(t *testing.T) {
	stored := parseOverride(t, `{"enabled": true, "schedule": {"every": "20h"}, "sampling": {"rounds": 5}}`)

	written := Effective(basePolicy(t), stored)
	if v := Validate(written, testLimits()); len(v) != 0 {
		t.Fatalf("override was rejected at write time: %+v", v)
	}

	lowered := testLimits()
	lowered.MaxEvery = time.Hour
	lowered.MaxRounds = 2

	got := Validate(Effective(basePolicy(t), stored), lowered)
	if len(got) != 2 {
		t.Fatalf("violations = %+v, want two", got)
	}
	want := []Violation{
		{Field: "schedule.every", Ceiling: "pgo.limits.maxEvery"},
		{Field: "sampling.rounds", Ceiling: "pgo.limits.maxRounds"},
	}
	for i, w := range want {
		if got[i].Field != w.Field || got[i].Ceiling != w.Ceiling {
			t.Fatalf("violation %d = %+v, want field %q ceiling %q", i, got[i], w.Field, w.Ceiling)
		}
	}
}

func TestReplicasJSON(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		tests := []struct {
			body string
			want Replicas
			out  string
		}{
			{body: `"all"`, want: AllReplicas(), out: `"all"`},
			{body: `1`, want: ReplicaCount(1), out: `1`},
			{body: `32`, want: ReplicaCount(32), out: `32`},
		}
		for _, tc := range tests {
			t.Run(tc.body, func(t *testing.T) {
				var r Replicas
				if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
					t.Fatalf("unmarshal %s: %v", tc.body, err)
				}
				if r != tc.want {
					t.Fatalf("replicas = %v, want %v", r, tc.want)
				}
				b, err := json.Marshal(r)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if string(b) != tc.out {
					t.Fatalf("marshal = %s, want %s", b, tc.out)
				}
			})
		}
	})

	t.Run("rejected", func(t *testing.T) {
		for _, body := range []string{`"ALL"`, `"every"`, `"3"`, `""`, `true`, `1.5`, `[]`} {
			t.Run(body, func(t *testing.T) {
				var r Replicas
				if err := json.Unmarshal([]byte(body), &r); err == nil {
					t.Fatalf("accepted %s as %v", body, r)
				}
			})
		}
	})

	t.Run("resolve against the ceiling", func(t *testing.T) {
		if got := AllReplicas().Resolve(32); got != 32 {
			t.Fatalf("all resolved to %d, want 32", got)
		}
		if got := ReplicaCount(3).Resolve(32); got != 3 {
			t.Fatalf("3 resolved to %d, want 3", got)
		}
		if got := ReplicaCount(40).Resolve(32); got != 32 {
			t.Fatalf("40 resolved to %d, want the ceiling 32", got)
		}
	})
}

func TestDurationJSON(t *testing.T) {
	tests := []struct {
		body string
		want time.Duration
		out  string
	}{
		{body: `"1h"`, want: time.Hour, out: `"1h"`},
		{body: `"5m"`, want: 5 * time.Minute, out: `"5m"`},
		{body: `"30s"`, want: 30 * time.Second, out: `"30s"`},
		{body: `"2h"`, want: 2 * time.Hour, out: `"2h"`},
		{body: `"168h"`, want: 168 * time.Hour, out: `"168h"`},
		{body: `"0s"`, want: 0, out: `"0s"`},
		{body: `"1h30m"`, want: 90 * time.Minute, out: `"1h30m"`},
		{body: `"1h0m30s"`, want: time.Hour + 30*time.Second, out: `"1h30s"`},
		{body: `"1500ms"`, want: 1500 * time.Millisecond, out: `"1.5s"`},
	}

	for _, tc := range tests {
		t.Run(tc.body, func(t *testing.T) {
			var d Duration
			if err := json.Unmarshal([]byte(tc.body), &d); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.body, err)
			}
			if d.Duration() != tc.want {
				t.Fatalf("duration = %v, want %v", d.Duration(), tc.want)
			}
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.out {
				t.Fatalf("marshal = %s, want %s", b, tc.out)
			}
		})
	}

	t.Run("rejected", func(t *testing.T) {
		for _, body := range []string{`"1 hour"`, `""`, `3600`, `"1x"`} {
			t.Run(body, func(t *testing.T) {
				var d Duration
				if err := json.Unmarshal([]byte(body), &d); err == nil {
					t.Fatalf("accepted %s as %v", body, d.Duration())
				}
			})
		}
	})
}

// TestStoredOverrideRoundTrip pins the KV value shape of section 6.4.
func TestStoredOverrideRoundTrip(t *testing.T) {
	const stored = `{
  "policy": {
    "enabled": true,
    "schedule": {"every": "1h", "jitter": "5m"},
    "sampling": {"rounds": 3}
  },
  "updatedBy": "anonymous",
  "updatedAt": "2026-08-23T10:00:00Z"
}`

	var got StoredOverride
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.UpdatedBy != "anonymous" {
		t.Fatalf("updatedBy = %q", got.UpdatedBy)
	}
	if !got.UpdatedAt.Equal(time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("updatedAt = %v", got.UpdatedAt)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSameJSON(t, []byte(stored), b)
}

// TestValidateAlwaysCodesAViolation measures a policy that misses every bound
// at once, so a ceiling added without a code fails here rather than shipping a
// violation a client cannot read.
func TestValidateAlwaysCodesAViolation(t *testing.T) {
	p := basePolicy(t)
	p.Schedule.Every = Duration(25 * time.Hour)
	p.Schedule.Jitter = Duration(20 * time.Hour)
	p.Sampling.Duration = Duration(61 * time.Second)
	p.Sampling.Rounds = 6
	p.Sampling.RoundInterval = Duration(11 * time.Minute)
	p.Sampling.Replicas = ReplicaCount(33)
	p.Sampling.MaxParallel = 5
	p.Target.VersionPolicy = "loose"
	p.Artifact.Retention = Duration(25 * time.Hour)

	got := Validate(p, testLimits())

	if len(got) != 9 {
		t.Fatalf("violations = %+v, want one per policy field", got)
	}
	known := []string{codeAboveMaximum, codeBelowMinimum, codeOutOfRange, codeNotPermitted, codeRetentionUnderInterval}
	for _, v := range got {
		if v.Code == "" {
			t.Errorf("violation %+v carries no code", v)

			continue
		}
		if !slices.Contains(known, v.Code) {
			t.Errorf("violation %+v carries a code outside %v", v, known)
		}
	}
}
