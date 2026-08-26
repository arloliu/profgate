package pgo

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/profgate/internal/config"
)

// maxRecordBytes is the largest serialized Collection record the gateway sends
// to a KV bucket, well under the 1 MiB default NATS message limit and more
// than twice the 210 KiB a record at every ceiling occupies.
// A constant, not configuration.
// internal/natskv/preflight.go holds the same value as the bucket contract's
// floor for MaxValueSize; the two move together.
const maxRecordBytes = 512 << 10

// ErrRecordTooLarge reports that a serialized record exceeds maxRecordBytes.
// The owner loop turns it into a terminal record rather than a wedged one.
var ErrRecordTooLarge = errors.New("record too large")

// State is where a Collection is in its life.
type State string

// The states of the spec's *Collections*, "Record".
// initializing lasts from the record's Create until its creator wins the
// active key, and is never claimable.
const (
	StateInitializing State = "initializing"
	StatePending      State = "pending"
	StateRunning      State = "running"
	StateCompleted    State = "completed"
	StateFailed       State = "failed"
	StateCancelled    State = "cancelled"
	StateExpired      State = "expired"
)

// Origin is what created a Collection.
type Origin string

// The two origins: a schedule slot, or a POST /collections request.
const (
	OriginSchedule Origin = "schedule"
	OriginAPI      Origin = "api"
)

// The reasons a Collection ends failed or cancelled.
const (
	ReasonVersionMissing      = "version_missing"
	ReasonVersionConflict     = "version_conflict"
	ReasonNoTargets           = "no_targets"
	ReasonNoSamples           = "no_samples"
	ReasonDeadlineExceeded    = "deadline_exceeded"
	ReasonAttemptsExhausted   = "attempts_exhausted"
	ReasonArtifactStoreFailed = "artifact_store_failed"
	ReasonMergedTooLarge      = "merged_too_large"
	ReasonSerializeFailed     = "serialize_failed"
	ReasonRecordTooLarge      = "record_too_large"
	ReasonNotClaimed          = "not_claimed"
	ReasonNotPublished        = "not_published"
	ReasonLimitExceeded       = "limit_exceeded"
	ReasonCancelledByAPI      = "cancelled_by_api"
)

// sampleResultOK is the manifest result of a sample that was fetched and parsed.
const sampleResultOK = "ok"

// Record is the value at job.<id> in PROFGATE_JOBS, and the durable source of
// truth for one Collection; gateway memory is a watched cache of it.
type Record struct {
	ID             string    `json:"id"`
	Namespace      string    `json:"namespace"`
	Service        string    `json:"service"`
	Origin         Origin    `json:"origin"`
	Slot           string    `json:"slot,omitempty"` // RFC 3339 for display; absent for api
	ConfigRevision uint64    `json:"configRevision"` // 0 when no override existed
	Policy         Policy    `json:"policy"`         // the snapshot it runs with; never re-read
	State          State     `json:"state"`
	Attempt        int       `json:"attempt"` // claims so far; 0 while pending
	Owner          *Owner    `json:"owner"`
	ClaimBy        time.Time `json:"claimBy"`

	LeaseUntil *time.Time `json:"leaseUntil"`
	Deadline   *time.Time `json:"deadline"` // set at first claim, from the snapshot and the ceilings

	Reason          string       `json:"reason"`
	ResolvedVersion string       `json:"resolvedVersion"`
	Progress        Progress     `json:"progress"` // the owner's last renewal snapshot; informational
	Manifest        *Manifest    `json:"manifest"`
	Artifact        *ArtifactRef `json:"artifact"` // set only by the completed update

	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	ExpiresAt  *time.Time `json:"expiresAt"` // finishedAt + retention, for completed
}

// Owner is the claiming replica.
// Instance is the Pod name plus a per-process random suffix.
type Owner struct {
	Instance string `json:"instance"`
	Pod      string `json:"pod"`
}

// Progress is how far the owner had got at its last renewal.
type Progress struct {
	Round         int `json:"round"`
	Rounds        int `json:"rounds"`
	SamplesOK     int `json:"samplesOK"`
	SamplesFailed int `json:"samplesFailed"`
}

// ArtifactRef names the stored profile and its size.
// The object name carries the attempt, so two attempts never write one name.
type ArtifactRef struct {
	Object string `json:"object"`
	Bytes  int64  `json:"bytes"`
}

// Manifest answers whether a profile is safe for a build, why it is smaller
// than expected, and whether it covers the whole fleet.
// It carries no Pod IP and no port.
type Manifest struct {
	Collection      string   `json:"collection"`
	Namespace       string   `json:"namespace"`
	Service         string   `json:"service"`
	Profile         string   `json:"profile"`
	ConfigRevision  uint64   `json:"configRevision"`
	ResolvedVersion string   `json:"resolvedVersion"`
	VersionLabel    string   `json:"versionLabel"`
	Sampling        Sampling `json:"sampling"`
	Attempt         int      `json:"attempt"`
	Truncated       bool     `json:"truncated"` // a round had more eligible Pods than it sampled
	Gateway         string   `json:"gateway"`
	Samples         []Sample `json:"samples,omitempty"`
}

// Sample is one Pod's contribution to one round.
type Sample struct {
	Round     int       `json:"round"`
	Pod       string    `json:"pod"`
	PodUID    string    `json:"podUID"`
	Node      string    `json:"node"`
	StartedAt time.Time `json:"startedAt"`
	Result    string    `json:"result"`
	Reason    string    `json:"reason,omitempty"`
	Bytes     int64     `json:"bytes"`
}

// MarshalBounded serializes a value for a KV write and refuses anything past
// maxRecordBytes, so every Update is size-checked before it reaches NATS.
func MarshalBounded(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("pgo: serialize record: %w", err)
	}
	if len(b) > maxRecordBytes {
		return nil, fmt.Errorf("pgo: %d bytes exceeds %d: %w", len(b), maxRecordBytes, ErrRecordTooLarge)
	}

	return b, nil
}

// terminalTooLarge is the record_too_large terminal form: the manifest keeps
// its scalars and the counts its samples carried, the samples themselves go,
// and no artifact is named because the object has been deleted.
// The result is small by construction, so its own Update cannot fail the same way.
func (r Record) terminalTooLarge(finishedAt time.Time) Record {
	out := r
	out.State = StateFailed
	out.Reason = ReasonRecordTooLarge
	out.FinishedAt = &finishedAt
	out.ExpiresAt = nil
	out.Artifact = nil

	if r.Manifest != nil {
		m := *r.Manifest
		out.Progress.SamplesOK = 0
		out.Progress.SamplesFailed = 0
		for _, s := range m.Samples {
			if s.Result == sampleResultOK {
				out.Progress.SamplesOK++
			} else {
				out.Progress.SamplesFailed++
			}
		}
		m.Samples = nil
		out.Manifest = &m
	}

	return out
}

// Deadline is when a Collection gives up, computed at its first claim from the
// policy snapshot and the maxTargetsPerRound ceiling — never from the live
// target count, so every replica that reads the record agrees on it.
func Deadline(startedAt time.Time, p Policy, lim config.PGOLimits) time.Time {
	rounds := time.Duration(p.Sampling.Rounds)
	targets := p.Sampling.Replicas.Resolve(lim.MaxTargetsPerRound)
	batches := time.Duration((targets + p.Sampling.MaxParallel - 1) / p.Sampling.MaxParallel)

	duration := p.Sampling.Duration.Duration()
	roundInterval := p.Sampling.RoundInterval.Duration()
	admissionWait := duration + roundInterval

	return startedAt.Add(rounds*batches*(duration+config.PGOSampleOverhead+admissionWait) +
		(rounds-1)*roundInterval + config.PGODeadlineSlack)
}
