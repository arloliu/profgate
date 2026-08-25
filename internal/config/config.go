// Package config loads the gateway configuration once at startup:
// a YAML file, PROFGATE_-prefixed environment overrides, defaults,
// normalization, and validation.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arloliu/fuda"
	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Config is the loaded gateway configuration.
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Discovery DiscoveryConfig  `yaml:"discovery"`
	Limits    LimitsConfig     `yaml:"limits"`
	Auth      AuthConfig       `yaml:"auth"`
	NATS      NATSConfig       `yaml:"nats"`
	PGO       PGOConfig        `yaml:"pgo"`
	Realms    map[string]Realm `yaml:"realms" validate:"required,min=1,dive"`
}

// ServerConfig holds the two listen addresses and the log level.
type ServerConfig struct {
	Listen    string `yaml:"listen"    env:"LISTEN"     default:":8080" validate:"required,hostname_port"`
	OpsListen string `yaml:"opsListen" env:"OPS_LISTEN" default:":9090" validate:"required,hostname_port"`
	LogLevel  string `yaml:"logLevel"  env:"LOG_LEVEL"  default:"info"  validate:"oneof=debug info warn error"`
}

// DiscoveryConfig controls how Pods are matched and which port serves pprof.
type DiscoveryConfig struct {
	VersionLabel string      `yaml:"versionLabel" env:"VERSION_LABEL" default:"app.kubernetes.io/version" validate:"required"`
	Pprof        PprofConfig `yaml:"pprof"`
}

// PprofConfig names the pprof port by number or by container-port name.
// Port 0 means unset; normalization sets it to 6060 when PortName is also empty.
type PprofConfig struct {
	Port     int32  `yaml:"port"     env:"PPROF_PORT"      validate:"min=0,max=65535"`
	PortName string `yaml:"portName" env:"PPROF_PORT_NAME"`
}

// LimitsConfig caps profile durations and concurrency.
type LimitsConfig struct {
	CPUSeconds            int `yaml:"cpuSeconds"            env:"LIMIT_CPU_SECONDS"             default:"60" validate:"min=1,max=86400"`
	TraceSeconds          int `yaml:"traceSeconds"          env:"LIMIT_TRACE_SECONDS"           default:"60" validate:"min=1,max=86400"`
	MaxConcurrentProfiles int `yaml:"maxConcurrentProfiles" env:"LIMIT_MAX_CONCURRENT_PROFILES" default:"16" validate:"min=1,max=1024"`
}

// AuthConfig selects the authentication mode and the realm for anonymous requests.
type AuthConfig struct {
	Mode           string `yaml:"mode"           env:"AUTH_MODE"       default:"disabled" validate:"oneof=disabled"`
	AnonymousRealm string `yaml:"anonymousRealm" env:"ANONYMOUS_REALM" validate:"required"`
}

// Realm lists what a principal may reach; each list is exact strings or "*".
type Realm struct {
	Namespaces []string `yaml:"namespaces" validate:"required,min=1"`
	Services   []string `yaml:"services"   validate:"required,min=1"`
	Profiles   []string `yaml:"profiles"   validate:"required,min=1"`
	PGO        RealmPGO `yaml:"pgo"`
}

// RealmPGO says what the realm may do with PGO Collections.
// A realm without a pgo block has every flag false.
type RealmPGO struct {
	Read      bool `yaml:"read"`
	Collect   bool `yaml:"collect"`
	Configure bool `yaml:"configure"`
}

// NATSConfig points the gateway at the NATS cluster holding the PGO stores.
// Url is a comma-separated list of nats:// or tls:// URLs.
type NATSConfig struct {
	URL            string        `yaml:"url"            env:"NATS_URL"`
	CredsFile      string        `yaml:"credsFile"      env:"NATS_CREDS_FILE"`
	ConnectTimeout time.Duration `yaml:"connectTimeout" env:"NATS_CONNECT_TIMEOUT" default:"5s" validate:"min=1s,max=60s"`
}

// PGOConfig turns PGO collection on and holds its ceilings and default policy.
type PGOConfig struct {
	Enabled      bool          `yaml:"enabled"      env:"PGO_ENABLED"       default:"false"`
	ConfigAPI    string        `yaml:"configAPI"    env:"PGO_CONFIG_API"    default:"enabled" validate:"oneof=enabled disabled"`
	LeaseTTL     time.Duration `yaml:"leaseTTL"     env:"PGO_LEASE_TTL"     default:"60s"     validate:"min=30s,max=10m"`
	MaxAttempts  int           `yaml:"maxAttempts"  env:"PGO_MAX_ATTEMPTS"  default:"3"       validate:"min=1,max=10"`
	JobRetention time.Duration `yaml:"jobRetention" env:"PGO_JOB_RETENTION" default:"168h"    validate:"max=2160h"`
	Limits       PGOLimits     `yaml:"limits"`
	Defaults     PGODefaults   `yaml:"defaults"`
}

// PGOLimits are the ceilings every effective policy is measured against.
// They are read at startup because the container memory figure and the
// admission arithmetic depend on them.
type PGOLimits struct {
	MaxDuration          time.Duration `yaml:"maxDuration"          env:"PGO_LIMIT_MAX_DURATION"           default:"60s"      validate:"min=1s"`
	MaxRounds            int           `yaml:"maxRounds"            env:"PGO_LIMIT_MAX_ROUNDS"             default:"5"        validate:"min=1,max=20"`
	MaxParallel          int           `yaml:"maxParallel"          env:"PGO_LIMIT_MAX_PARALLEL"           default:"4"        validate:"min=1,max=64"`
	MinEvery             time.Duration `yaml:"minEvery"             env:"PGO_LIMIT_MIN_EVERY"              default:"15m"      validate:"min=1m"`
	MaxEvery             time.Duration `yaml:"maxEvery"             env:"PGO_LIMIT_MAX_EVERY"              default:"24h"      validate:"max=24h"`
	MaxRetention         time.Duration `yaml:"maxRetention"         env:"PGO_LIMIT_MAX_RETENTION"          default:"24h"      validate:"min=1m,max=720h"`
	MaxSampleBytes       int64         `yaml:"maxSampleBytes"       env:"PGO_LIMIT_MAX_SAMPLE_BYTES"       default:"33554432" validate:"min=1048576,max=268435456"`
	MaxMergedBytes       int64         `yaml:"maxMergedBytes"       env:"PGO_LIMIT_MAX_MERGED_BYTES"       default:"67108864" validate:"max=1073741824"`
	MaxTargetsPerRound   int           `yaml:"maxTargetsPerRound"   env:"PGO_LIMIT_MAX_TARGETS_PER_ROUND"  default:"32"       validate:"min=1,max=256"`
	MaxActiveCollections int           `yaml:"maxActiveCollections" env:"PGO_LIMIT_MAX_ACTIVE_COLLECTIONS" default:"2"        validate:"min=1"`
	OnDemandPerMinute    int           `yaml:"onDemandPerMinute"    env:"PGO_LIMIT_ON_DEMAND_PER_MINUTE"   default:"10"       validate:"min=1,max=600"`
	MaxLiveCollections   int           `yaml:"maxLiveCollections"   env:"PGO_LIMIT_MAX_LIVE_COLLECTIONS"   default:"64"       validate:"min=1,max=1024"`
}

// PGODefaults is the policy a Service gets before any override.
// It carries no environment overrides: it is policy, like realms.
type PGODefaults struct {
	Schedule PGOScheduleDefaults `yaml:"schedule"`
	Sampling PGOSamplingDefaults `yaml:"sampling"`
	Target   PGOTargetDefaults   `yaml:"target"`
	Artifact PGOArtifactDefaults `yaml:"artifact"`
}

// PGOScheduleDefaults is how often a Service is collected by default.
type PGOScheduleDefaults struct {
	Every  time.Duration `yaml:"every"  default:"6h"`
	Jitter time.Duration `yaml:"jitter" default:"10m"`
}

// PGOSamplingDefaults is how a Collection samples by default.
// Replicas is what the operator wrote: "all" or a decimal count.
type PGOSamplingDefaults struct {
	Duration      time.Duration `yaml:"duration"      default:"30s"`
	Rounds        int           `yaml:"rounds"        default:"2"`
	RoundInterval time.Duration `yaml:"roundInterval" validate:"min=0,max=10m"`
	Replicas      string        `yaml:"replicas"      default:"all"`
	MaxParallel   int           `yaml:"maxParallel"   default:"4"`
}

// PGOTargetDefaults is how a Collection picks the binary version it profiles.
type PGOTargetDefaults struct {
	VersionPolicy string `yaml:"versionPolicy" default:"strict" validate:"oneof=strict"`
}

// PGOArtifactDefaults is how long a finished profile is kept by default.
type PGOArtifactDefaults struct {
	Retention time.Duration `yaml:"retention" default:"2h"`
}

// defaultPprofPort is used when neither port nor portName is configured.
const defaultPprofPort = 6060

// defaultRoundInterval is used when the file leaves roundInterval out.
// It is not a `default` struct tag because 0 is a setting of its own,
// and the loader applies a tag default to any zero field.
const defaultRoundInterval = 30 * time.Second

// gracePeriodSlack is added to the longest profile duration to form the grace period.
const gracePeriodSlack = 60 * time.Second

// maxSamplesPerCollection bounds rounds times targets so a Collection record
// stays well under the size a single NATS message can carry.
const maxSamplesPerCollection = 256

// replicasAll is the sampling default that means every eligible Pod,
// up to the maxTargetsPerRound ceiling.
const replicasAll = "all"

// profileNames holds the eight profile names in the spec's order.
var profileNames = [...]string{"cpu", "trace", "heap", "allocs", "goroutine", "mutex", "block", "threadcreate"}

// IsProfile reports whether name is one of the eight profile names.
func IsProfile(name string) bool {
	return slices.Contains(profileNames[:], name)
}

// Profiles returns a copy of the eight profile names in the spec's order.
func Profiles() []string {
	return slices.Clone(profileNames[:])
}

// SlogLevel is LogLevel as the level the JSON handler is built with.
// The oneof tag is what pins the four names,
// so an unknown one never reaches here and info is the unreachable fallback.
func (s ServerConfig) SlogLevel() slog.Level {
	switch s.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// RequiredGracePeriod is the Deployment grace period the limits demand:
// the longer of the CPU and trace limits plus 60 seconds.
func (c *Config) RequiredGracePeriod() time.Duration {
	return time.Duration(max(c.Limits.CPUSeconds, c.Limits.TraceSeconds))*time.Second + gracePeriodSlack
}

// The PGO figures that no pgo.limits key expresses.
// They are constants, not configuration, and internal/pgo measures its
// policies, deadlines, and decoder against the same four.
// They live here because internal/pgo imports internal/config and the reverse
// direction would be an import cycle.
const (
	// PGODecodeFactor estimates how much heap a decoded profile occupies
	// against its encoded length: two buffers of input plus about six times
	// that in decoded structures.
	PGODecodeFactor = 8
	// PGOMaxRoundInterval is the largest roundInterval any policy may ask for.
	PGOMaxRoundInterval = 10 * time.Minute
	// PGOSampleOverhead is the per-sample allowance the deadline formula adds
	// on top of the profile duration and the wait for an admission slot.
	PGOSampleOverhead = 30 * time.Second
	// PGODeadlineSlack is the fixed tail of the deadline formula.
	PGODeadlineSlack = 60 * time.Second
)

// PGOMemoryBytes is the container memory a Collection worker can occupy at the
// configured ceilings, over the gateway's own footprint:
// per active Collection, every in-flight sample as compressed bytes,
// decompressed bytes, and a decoded profile;
// the running merged profile; and the serialized copy written to the store.
// It is a sizing rule, not a proof: the decoded sizes are an estimate.
func (c *Config) PGOMemoryBytes() int64 {
	l := c.PGO.Limits
	perCollection := int64(l.MaxParallel)*PGODecodeFactor*l.MaxSampleBytes + 2*PGODecodeFactor*l.MaxMergedBytes

	return int64(l.MaxActiveCollections) * perCollection
}

// RequiredPGOGracePeriod is the Deployment grace period a Collection demands,
// because drain waits for in-flight merges that cannot be interrupted.
// It is the deadline formula at the longest value every input can take:
// maxRounds rounds, maxDuration per sample, roundInterval at its own
// 10-minute bound, which has no pgo.limits entry, and maxTargetsPerRound
// batches of one target each.
// The batch count is the target ceiling rather than
// maxTargetsPerRound / maxParallel because pgo.limits.maxParallel only caps a
// policy from above: a policy may sample one Pod at a time, which is the
// slowest a Collection can legally run.
// A grace period below this number loses no work — a Collection the kubelet
// kills mid-merge stops renewing its lease and another replica reclaims it —
// so this is the period that lets every admissible Collection finish in place,
// not a floor below which the gateway is unsafe.
func (c *Config) RequiredPGOGracePeriod() time.Duration {
	l := c.PGO.Limits
	batches := time.Duration(l.MaxTargetsPerRound)
	rounds := time.Duration(l.MaxRounds)
	admissionWait := l.MaxDuration + PGOMaxRoundInterval

	return rounds*batches*(l.MaxDuration+PGOSampleOverhead+admissionWait) +
		(rounds-1)*PGOMaxRoundInterval + PGODeadlineSlack
}

// Load reads the YAML file at path, rejects unknown keys at any nesting level,
// applies defaults and PROFGATE_-prefixed environment overrides, normalizes, and validates.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is the operator's --config flag; reading it is the purpose
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var probe Config
	if err := dec.Decode(&probe); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var given givenKeys
	if err := yaml.Unmarshal(b, &given); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	v := validator.New()
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		return strings.Split(f.Tag.Get("yaml"), ",")[0]
	})
	loader, err := fuda.New().FromBytes(b).WithEnvPrefix("PROFGATE_").WithValidator(v).Build()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var cfg Config
	if err := loader.Load(&cfg); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			return nil, fmt.Errorf("config: %s: %s: %w", path, fieldPaths(verrs), err)
		}
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	normalize(&cfg, given)
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// givenKeys reads the file a second time to record which keys it carries,
// for the fields whose zero value is a setting rather than an absence.
// The loaded Config cannot answer that question:
// a zero field looks the same whether the operator wrote 0 or wrote nothing.
type givenKeys struct {
	PGO struct {
		Defaults struct {
			Sampling struct {
				RoundInterval *time.Duration `yaml:"roundInterval"`
			} `yaml:"sampling"`
		} `yaml:"defaults"`
	} `yaml:"pgo"`
}

// fieldPaths renders each failing field as its YAML key path,
// for example "limits.cpuSeconds".
func fieldPaths(verrs validator.ValidationErrors) string {
	paths := make([]string, 0, len(verrs))
	for _, fe := range verrs {
		_, rest, _ := strings.Cut(fe.Namespace(), ".")
		paths = append(paths, fmt.Sprintf("%s (%s)", rest, fe.Tag()))
	}
	return strings.Join(paths, ", ")
}

// normalize fills the keys whose default the loader cannot apply:
// the pprof port, where 0 means the operator named the port some other way,
// and the round interval, where 0 is a setting of its own.
func normalize(cfg *Config, given givenKeys) {
	if cfg.Discovery.Pprof.Port == 0 && cfg.Discovery.Pprof.PortName == "" {
		cfg.Discovery.Pprof.Port = defaultPprofPort
	}
	if given.PGO.Defaults.Sampling.RoundInterval == nil {
		cfg.PGO.Defaults.Sampling.RoundInterval = defaultRoundInterval
	}
}

// validate runs the checks that struct tags cannot express; each error names the key.
func validate(cfg *Config) error {
	pprof := cfg.Discovery.Pprof
	if (pprof.Port != 0) == (pprof.PortName != "") {
		return errors.New("exactly one of discovery.pprof.port and discovery.pprof.portName must be set")
	}
	if pprof.PortName != "" {
		if msgs := validation.IsValidPortName(pprof.PortName); len(msgs) > 0 {
			return fmt.Errorf("discovery.pprof.portName %q: %s", pprof.PortName, strings.Join(msgs, "; "))
		}
	}
	if msgs := validation.IsQualifiedName(cfg.Discovery.VersionLabel); len(msgs) > 0 {
		return fmt.Errorf("discovery.versionLabel %q: %s", cfg.Discovery.VersionLabel, strings.Join(msgs, "; "))
	}
	if cfg.Server.Listen == cfg.Server.OpsListen {
		return fmt.Errorf("server.opsListen must differ from server.listen (both %q)", cfg.Server.Listen)
	}
	if _, ok := cfg.Realms[cfg.Auth.AnonymousRealm]; !ok {
		return fmt.Errorf("auth.anonymousRealm %q is not a realm", cfg.Auth.AnonymousRealm)
	}
	for name, realm := range cfg.Realms {
		for _, list := range []struct {
			key     string
			entries []string
			valid   func(string) bool
		}{
			{"namespaces", realm.Namespaces, isDNSLabel},
			{"services", realm.Services, isDNSLabel},
			{"profiles", realm.Profiles, IsProfile},
		} {
			for _, entry := range list.entries {
				if entry != "*" && !list.valid(entry) {
					return fmt.Errorf("realms.%s.%s: invalid entry %q", name, list.key, entry)
				}
			}
		}
	}
	return validatePGO(cfg)
}

// validatePGO runs the rules that relate a PGO key to another key.
// The rules that judge the pgo block against itself run whether or not
// pgo.enabled, so a block that contradicts itself fails at startup rather than
// on the day collection is turned on.
// Two rules wait for pgo.enabled because they measure the PGO ceilings against
// the interactive limits, which a gateway that never collects is free to set
// low: at limits.maxConcurrentProfiles 1 no maxParallel and
// maxActiveCollections pair exists that keeps their product below it, and both
// are at least 1, and limits.cpuSeconds may sit under the shipped maxDuration.
// The NATS settings wait for the same reason:
// a disabled gateway reaches no NATS cluster and needs none configured.
// The per-key ranges are struct tags and hold either way,
// since the shipped defaults satisfy every one of them.
func validatePGO(cfg *Config) error {
	limits := cfg.PGO.Limits
	if limits.MaxRounds*limits.MaxTargetsPerRound > maxSamplesPerCollection {
		return fmt.Errorf("pgo.limits.maxRounds %d times pgo.limits.maxTargetsPerRound %d must be at most %d",
			limits.MaxRounds, limits.MaxTargetsPerRound, maxSamplesPerCollection)
	}
	if cfg.PGO.JobRetention < limits.MaxRetention+time.Hour {
		return fmt.Errorf("pgo.jobRetention %v must be at least pgo.limits.maxRetention %v plus 1h",
			cfg.PGO.JobRetention, limits.MaxRetention)
	}
	if limits.MinEvery > limits.MaxEvery {
		return fmt.Errorf("pgo.limits.maxEvery %v must be at least pgo.limits.minEvery %v", limits.MaxEvery, limits.MinEvery)
	}
	if limits.MaxSampleBytes > limits.MaxMergedBytes {
		return fmt.Errorf("pgo.limits.maxMergedBytes %d must be at least pgo.limits.maxSampleBytes %d",
			limits.MaxMergedBytes, limits.MaxSampleBytes)
	}
	if err := validatePGODefaults(&cfg.PGO.Defaults, limits); err != nil {
		return err
	}

	if !cfg.PGO.Enabled {
		return nil
	}

	if err := validateNATS(&cfg.NATS); err != nil {
		return err
	}

	if limits.MaxParallel*limits.MaxActiveCollections >= cfg.Limits.MaxConcurrentProfiles {
		return fmt.Errorf("pgo.limits.maxParallel %d times pgo.limits.maxActiveCollections %d must stay below limits.maxConcurrentProfiles %d",
			limits.MaxParallel, limits.MaxActiveCollections, cfg.Limits.MaxConcurrentProfiles)
	}
	if cpu := time.Duration(cfg.Limits.CPUSeconds) * time.Second; limits.MaxDuration > cpu {
		return fmt.Errorf("pgo.limits.maxDuration %v must be at most limits.cpuSeconds %v", limits.MaxDuration, cpu)
	}
	return nil
}

// validateNATS checks the connection settings the PGO stores are reached through.
func validateNATS(nats *NATSConfig) error {
	if nats.URL == "" {
		return errors.New("nats.url is required when pgo.enabled is true")
	}
	for _, entry := range strings.Split(nats.URL, ",") {
		entry = strings.TrimSpace(entry)
		if !strings.HasPrefix(entry, "nats://") && !strings.HasPrefix(entry, "tls://") {
			return fmt.Errorf("nats.url %q: every URL must begin with nats:// or tls://", entry)
		}
	}
	if nats.CredsFile != "" {
		f, err := os.Open(nats.CredsFile) //nolint:gosec // the operator names the file; reading it is the purpose
		if err != nil {
			return fmt.Errorf("nats.credsFile: %w", err)
		}
		_ = f.Close()
	}
	return nil
}

// validatePGODefaults checks the shipped policy against the ceilings it must obey.
func validatePGODefaults(defaults *PGODefaults, limits PGOLimits) error {
	if every := defaults.Schedule.Every; every < limits.MinEvery || every > limits.MaxEvery {
		return fmt.Errorf("pgo.defaults.schedule.every %v must be between pgo.limits.minEvery %v and pgo.limits.maxEvery %v",
			every, limits.MinEvery, limits.MaxEvery)
	}
	if jitter := defaults.Schedule.Jitter; jitter > defaults.Schedule.Every/2 {
		return fmt.Errorf("pgo.defaults.schedule.jitter %v must be at most half of pgo.defaults.schedule.every %v",
			jitter, defaults.Schedule.Every)
	}
	sampling := defaults.Sampling
	if sampling.Duration > limits.MaxDuration {
		return fmt.Errorf("pgo.defaults.sampling.duration %v must be at most pgo.limits.maxDuration %v",
			sampling.Duration, limits.MaxDuration)
	}
	if sampling.Rounds > limits.MaxRounds {
		return fmt.Errorf("pgo.defaults.sampling.rounds %d must be at most pgo.limits.maxRounds %d",
			sampling.Rounds, limits.MaxRounds)
	}
	if sampling.MaxParallel > limits.MaxParallel {
		return fmt.Errorf("pgo.defaults.sampling.maxParallel %d must be at most pgo.limits.maxParallel %d",
			sampling.MaxParallel, limits.MaxParallel)
	}
	if sampling.Replicas != replicasAll {
		count, err := strconv.Atoi(sampling.Replicas)
		if err != nil {
			return fmt.Errorf("pgo.defaults.sampling.replicas %q must be %q or a number", sampling.Replicas, replicasAll)
		}
		if count < 1 || count > limits.MaxTargetsPerRound {
			return fmt.Errorf("pgo.defaults.sampling.replicas %d must be between 1 and pgo.limits.maxTargetsPerRound %d",
				count, limits.MaxTargetsPerRound)
		}
	}
	if defaults.Artifact.Retention > limits.MaxRetention {
		return fmt.Errorf("pgo.defaults.artifact.retention %v must be at most pgo.limits.maxRetention %v",
			defaults.Artifact.Retention, limits.MaxRetention)
	}
	return nil
}

// isDNSLabel reports whether s is a DNS-1123 label, the rule for namespace and Service names.
func isDNSLabel(s string) bool {
	return len(validation.IsDNS1123Label(s)) == 0
}
