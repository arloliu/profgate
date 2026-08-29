// Package pgo collects representative CPU profiles for Profile-Guided
// Optimization: it layers the policy of one Service over the operator's
// defaults, publishes and claims Collections through the NATS seam, merges the
// samples in memory, and stores the merged profile in the Object Store.
package pgo

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arloliu/profgate/internal/config"
)

// replicasAll is the sampling value that means every eligible Pod,
// up to the pgo.limits.maxTargetsPerRound ceiling.
const replicasAll = "all"

// Bounds that no pgo.limits key expresses,
// so a policy is measured against them wherever it is validated.
const (
	minSamplingDuration = time.Second
	minRetention        = time.Minute
	versionPolicyStrict = "strict"
)

// Duration is a Go duration string in JSON: "30s", "1h".
// It marshals in the shortest form that parses back to the same value,
// so a policy written as "1h" is read back as "1h".
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String is the shortest Go duration string for the value:
// whole hours and minutes carry no zero components after them.
func (d Duration) String() string {
	v := time.Duration(d)
	if v == 0 {
		return "0s"
	}

	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
		v = -v
	}
	if h := v / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		v -= h * time.Hour
	}
	if m := v / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		v -= m * time.Minute
	}
	if v > 0 {
		b.WriteString(v.String())
	}

	return b.String()
}

// MarshalText writes the duration string.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("policy: duration %q: %w", b, err)
	}
	*d = Duration(v)

	return nil
}

// Replicas is how many Pods of a Service one round samples:
// the string "all" or a count.
// Its zero value is neither, so a policy that never set it fails Validate
// instead of silently meaning every Pod.
type Replicas struct {
	all   bool
	count int
}

// AllReplicas is every eligible Pod, up to pgo.limits.maxTargetsPerRound.
func AllReplicas() Replicas { return Replicas{all: true} }

// ReplicaCount is an explicit number of Pods per round.
func ReplicaCount(n int) Replicas { return Replicas{count: n} }

// IsAll reports whether the policy asks for every eligible Pod.
func (r Replicas) IsAll() bool { return r.all }

// Count is the explicit number of Pods, or 0 when the policy says "all".
func (r Replicas) Count() int {
	if r.all {
		return 0
	}

	return r.count
}

// Resolve is the number of Pods one round samples under a maxTargetsPerRound ceiling.
func (r Replicas) Resolve(maxTargets int) int {
	if r.all {
		return maxTargets
	}

	return min(r.count, maxTargets)
}

// String renders the value as it appears in JSON.
func (r Replicas) String() string {
	if r.all {
		return replicasAll
	}

	return strconv.Itoa(r.count)
}

// MarshalJSON writes "all" or the count as a JSON number.
func (r Replicas) MarshalJSON() ([]byte, error) {
	if r.all {
		return json.Marshal(replicasAll)
	}

	return json.Marshal(r.count)
}

// UnmarshalJSON accepts the string "all" or a JSON integer, and nothing else:
// a quoted number is a rejected string, not a count.
func (r *Replicas) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("policy: replicas: %w", err)
		}
		if s != replicasAll {
			return fmt.Errorf("policy: replicas %q must be %q or a number", s, replicasAll)
		}
		*r = AllReplicas()

		return nil
	}

	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("policy: replicas: %w", err)
	}
	*r = ReplicaCount(n)

	return nil
}

// Policy is the effective PGO policy of one Service.
type Policy struct {
	Enabled  bool         `json:"enabled"`
	Schedule Schedule     `json:"schedule"`
	Sampling Sampling     `json:"sampling"`
	Target   TargetPolicy `json:"target"`
	Artifact Artifact     `json:"artifact"`
}

// Schedule is how often a Service is collected.
type Schedule struct {
	Every  Duration `json:"every"`
	Jitter Duration `json:"jitter"`
}

// Sampling is how one Collection samples its Pods.
type Sampling struct {
	Duration      Duration `json:"duration"`
	Rounds        int      `json:"rounds"`
	RoundInterval Duration `json:"roundInterval"`
	Replicas      Replicas `json:"replicas"`
	MaxParallel   int      `json:"maxParallel"`
}

// TargetPolicy is how a Collection picks the binary version it profiles.
// An empty Version means whatever the targets agree on.
type TargetPolicy struct {
	VersionPolicy string `json:"versionPolicy"`
	Version       string `json:"version"`
}

// Artifact is how long a finished profile is kept.
type Artifact struct {
	Retention Duration `json:"retention"`
}

// PolicyOverride is a Service's stored override, one level deep:
// a block the override omits or sets to null is unset, and so is a field
// inside a block it does set.
type PolicyOverride struct {
	Enabled  *bool             `json:"enabled,omitempty"`
	Schedule *ScheduleOverride `json:"schedule,omitempty"`
	Sampling *SamplingOverride `json:"sampling,omitempty"`
	Target   *TargetOverride   `json:"target,omitempty"`
	Artifact *ArtifactOverride `json:"artifact,omitempty"`
}

// ScheduleOverride is the schedule block of an override.
type ScheduleOverride struct {
	Every  *Duration `json:"every,omitempty"`
	Jitter *Duration `json:"jitter,omitempty"`
}

// SamplingOverride is the sampling block of an override.
type SamplingOverride struct {
	Duration      *Duration `json:"duration,omitempty"`
	Rounds        *int      `json:"rounds,omitempty"`
	RoundInterval *Duration `json:"roundInterval,omitempty"`
	Replicas      *Replicas `json:"replicas,omitempty"`
	MaxParallel   *int      `json:"maxParallel,omitempty"`
}

// TargetOverride is the target block of an override.
type TargetOverride struct {
	VersionPolicy *string `json:"versionPolicy,omitempty"`
	Version       *string `json:"version,omitempty"`
}

// ArtifactOverride is the artifact block of an override.
type ArtifactOverride struct {
	Retention *Duration `json:"retention,omitempty"`
}

// StoredOverride is the value at service.<ns>.<svc> in PROFGATE_CONFIG.
// UpdatedBy is the principal of the request that wrote it;
// UpdatedAt is set by the gateway.
type StoredOverride struct {
	Policy    *PolicyOverride `json:"policy"`
	UpdatedBy string          `json:"updatedBy"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// DefaultPolicy is the operator's pgo.defaults as a Policy.
// Enabled is false: it has no operator default, so scheduling a Service is
// always an explicit override.
// An unparsable replicas value is an error rather than "all",
// because configuration validation runs only when pgo.enabled is true.
func DefaultPolicy(d config.PGODefaults) (Policy, error) {
	replicas := AllReplicas()
	if d.Sampling.Replicas != replicasAll {
		n, err := strconv.Atoi(d.Sampling.Replicas)
		if err != nil {
			return Policy{}, fmt.Errorf("pgo.defaults.sampling.replicas %q must be %q or a number",
				d.Sampling.Replicas, replicasAll)
		}
		replicas = ReplicaCount(n)
	}

	return Policy{
		Enabled: false,
		Schedule: Schedule{
			Every:  Duration(d.Schedule.Every),
			Jitter: Duration(d.Schedule.Jitter),
		},
		Sampling: Sampling{
			Duration:      Duration(d.Sampling.Duration),
			Rounds:        d.Sampling.Rounds,
			RoundInterval: Duration(d.Sampling.RoundInterval),
			Replicas:      replicas,
			MaxParallel:   d.Sampling.MaxParallel,
		},
		Target:   TargetPolicy{VersionPolicy: d.Target.VersionPolicy},
		Artifact: Artifact{Retention: Duration(d.Artifact.Retention)},
	}, nil
}

// Effective layers an override onto the defaults, block by block, one level
// deep: an override {"sampling": {"rounds": 3}} changes rounds and nothing else.
func Effective(defaults Policy, override *PolicyOverride) Policy {
	p := defaults
	if override == nil {
		return p
	}

	if override.Enabled != nil {
		p.Enabled = *override.Enabled
	}
	if s := override.Schedule; s != nil {
		setIf(&p.Schedule.Every, s.Every)
		setIf(&p.Schedule.Jitter, s.Jitter)
	}
	if s := override.Sampling; s != nil {
		setIf(&p.Sampling.Duration, s.Duration)
		setIf(&p.Sampling.Rounds, s.Rounds)
		setIf(&p.Sampling.RoundInterval, s.RoundInterval)
		setIf(&p.Sampling.Replicas, s.Replicas)
		setIf(&p.Sampling.MaxParallel, s.MaxParallel)
	}
	if t := override.Target; t != nil {
		setIf(&p.Target.VersionPolicy, t.VersionPolicy)
		setIf(&p.Target.Version, t.Version)
	}
	if a := override.Artifact; a != nil {
		setIf(&p.Artifact.Retention, a.Retention)
	}

	return p
}

// setIf replaces dst when the override set the field; a nil src is unset.
func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// The vocabulary of a violation: which way the value misses its bound.
// A client reads Code rather than parsing Detail, which is free to change.
const (
	// codeAboveMaximum is a value above the ceiling the detail names.
	codeAboveMaximum = "above_maximum"
	// codeBelowMinimum is a value below the floor the detail names.
	codeBelowMinimum = "below_minimum"
	// codeOutOfRange is a value outside a range whose two ends are one rule.
	codeOutOfRange = "out_of_range"
	// codeNotPermitted is a value outside the fixed set the field admits.
	codeNotPermitted = "not_permitted"
	// codeRetentionUnderInterval is an artifact.retention shorter than the schedule.every of the same policy.
	codeRetentionUnderInterval = "retention_under_interval"
)

// Violation is one policy field that exceeds a bound.
// Ceiling names the bound so a reader can tell a lowered pgo.limits key from a
// value the policy could never have held,
// and Code says which way the value misses it.
type Violation struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Ceiling string `json:"ceiling"`
	Detail  string `json:"detail"`
}

// Validate measures an effective policy against the ceilings, in the order the
// policy declares its fields.
// The result is the same wherever it is called: a write turns it into
// 400 limit_exceeded, a scheduler read makes the Service ineligible, and a
// worker claim fails the Collection with limit_exceeded.
func Validate(p Policy, lim config.PGOLimits) []Violation {
	var out []Violation
	add := func(field, code, ceiling, format string, args ...any) {
		out = append(out, Violation{
			Field:   field,
			Code:    code,
			Ceiling: ceiling,
			Detail:  fmt.Sprintf(format, args...),
		})
	}

	switch every := p.Schedule.Every.Duration(); {
	case every > lim.MaxEvery:
		add("schedule.every", codeAboveMaximum, "pgo.limits.maxEvery", "%v is more than %v", every, lim.MaxEvery)
	case every < lim.MinEvery:
		add("schedule.every", codeBelowMinimum, "pgo.limits.minEvery", "%v is less than %v", every, lim.MinEvery)
	}

	if jitter := p.Schedule.Jitter.Duration(); jitter < 0 || jitter > p.Schedule.Every.Duration()/2 {
		add("schedule.jitter", codeOutOfRange, "schedule.every/2", "%v is more than half of %v", jitter, p.Schedule.Every)
	}

	switch d := p.Sampling.Duration.Duration(); {
	case d > lim.MaxDuration:
		add("sampling.duration", codeAboveMaximum, "pgo.limits.maxDuration", "%v is more than %v", d, lim.MaxDuration)
	case d < minSamplingDuration:
		add("sampling.duration", codeBelowMinimum, minSamplingDuration.String(), "%v is less than %v", d, minSamplingDuration)
	}

	switch rounds := p.Sampling.Rounds; {
	case rounds > lim.MaxRounds:
		add("sampling.rounds", codeAboveMaximum, "pgo.limits.maxRounds", "%d is more than %d", rounds, lim.MaxRounds)
	case rounds < 1:
		add("sampling.rounds", codeBelowMinimum, "1", "%d is less than 1", rounds)
	}

	if ri := p.Sampling.RoundInterval.Duration(); ri < 0 || ri > config.PGOMaxRoundInterval {
		add("sampling.roundInterval", codeOutOfRange, Duration(config.PGOMaxRoundInterval).String(),
			"%v is outside 0 to %v", ri, config.PGOMaxRoundInterval)
	}

	if !p.Sampling.Replicas.IsAll() {
		switch n := p.Sampling.Replicas.Count(); {
		case n > lim.MaxTargetsPerRound:
			add("sampling.replicas", codeAboveMaximum, "pgo.limits.maxTargetsPerRound", "%d is more than %d", n, lim.MaxTargetsPerRound)
		case n < 1:
			add("sampling.replicas", codeBelowMinimum, "1", "%d is less than 1", n)
		}
	}

	switch mp := p.Sampling.MaxParallel; {
	case mp > lim.MaxParallel:
		add("sampling.maxParallel", codeAboveMaximum, "pgo.limits.maxParallel", "%d is more than %d", mp, lim.MaxParallel)
	case mp < 1:
		add("sampling.maxParallel", codeBelowMinimum, "1", "%d is less than 1", mp)
	}

	if p.Target.VersionPolicy != versionPolicyStrict {
		add("target.versionPolicy", codeNotPermitted, versionPolicyStrict, "%q is not %q", p.Target.VersionPolicy, versionPolicyStrict)
	}

	switch r := p.Artifact.Retention.Duration(); {
	case r > lim.MaxRetention:
		add("artifact.retention", codeAboveMaximum, "pgo.limits.maxRetention", "%v is more than %v", r, lim.MaxRetention)
	case r < minRetention:
		add("artifact.retention", codeBelowMinimum, Duration(minRetention).String(), "%v is less than %v", r, minRetention)
	case r < p.Schedule.Every.Duration():
		add("artifact.retention", codeRetentionUnderInterval, "schedule.every",
			"%v is less than schedule.every %v", r, p.Schedule.Every)
	}

	return out
}
