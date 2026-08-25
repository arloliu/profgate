package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
)

// fixture returns the path of a testdata file.
func fixture(name string) string {
	return "testdata/" + name
}

// loadErr loads path and fails the test unless the error mentions want.
func loadErr(t *testing.T, path, want string) {
	t.Helper()
	_, err := config.Load(path)
	if err == nil {
		t.Fatalf("Load(%q) = nil error, want one containing %q", path, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Load(%q) error = %q, want it to contain %q", path, err.Error(), want)
	}
}

// loadOK loads path and fails the test on any error.
func loadOK(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error = %v", path, err)
	}
	return cfg
}

func TestLoad(t *testing.T) {
	t.Run("good", func(t *testing.T) {
		cfg := loadOK(t, fixture("good.yaml"))
		want := config.Config{
			Server:    config.ServerConfig{Listen: ":8080", OpsListen: ":9090"},
			Discovery: config.DiscoveryConfig{VersionLabel: "app.kubernetes.io/version", Pprof: config.PprofConfig{Port: 6060}},
			Limits:    config.LimitsConfig{CPUSeconds: 60, TraceSeconds: 60, MaxConcurrentProfiles: 16},
			Auth:      config.AuthConfig{Mode: "disabled", AnonymousRealm: "developer"},
		}
		if cfg.Server != want.Server || cfg.Discovery != want.Discovery || cfg.Limits != want.Limits || cfg.Auth != want.Auth {
			t.Fatalf("Load() = %+v, want %+v", *cfg, want)
		}
		realm, ok := cfg.Realms["developer"]
		if !ok || len(cfg.Realms) != 1 {
			t.Fatalf("Realms = %+v, want only developer", cfg.Realms)
		}
		for _, list := range [][]string{realm.Namespaces, realm.Services, realm.Profiles} {
			if len(list) != 1 || list[0] != "*" {
				t.Fatalf("developer realm = %+v, want every list [\"*\"]", realm)
			}
		}
	})

	t.Run("unknown top-level", func(t *testing.T) {
		loadErr(t, fixture("unknown-top.yaml"), "field extra not found in type config.Config")
	})
	t.Run("unknown nested", func(t *testing.T) {
		loadErr(t, fixture("unknown-nested.yaml"), "field foo not found in type config.LimitsConfig")
	})
	t.Run("unknown in realm", func(t *testing.T) {
		loadErr(t, fixture("unknown-realm.yaml"), "field profilse not found in type config.Realm")
	})

	t.Run("limit zero", func(t *testing.T) {
		t.Setenv("PROFGATE_LIMIT_CPU_SECONDS", "0")
		loadErr(t, fixture("good.yaml"), "limits.cpuSeconds")
	})
	t.Run("limit too large", func(t *testing.T) {
		t.Setenv("PROFGATE_LIMIT_TRACE_SECONDS", "86401")
		loadErr(t, fixture("good.yaml"), "limits.traceSeconds")
	})
	t.Run("concurrency out of range", func(t *testing.T) {
		t.Setenv("PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES", "1025")
		loadErr(t, fixture("good.yaml"), "limits.maxConcurrentProfiles")
	})

	t.Run("neither port", func(t *testing.T) {
		cfg := loadOK(t, fixture("neither-port.yaml"))
		if cfg.Discovery.Pprof.Port != 6060 || cfg.Discovery.Pprof.PortName != "" {
			t.Fatalf("Pprof = %+v, want Port 6060 and empty PortName", cfg.Discovery.Pprof)
		}
	})
	t.Run("name only", func(t *testing.T) {
		cfg := loadOK(t, fixture("name-only.yaml"))
		if cfg.Discovery.Pprof.Port != 0 || cfg.Discovery.Pprof.PortName != "pprof" {
			t.Fatalf("Pprof = %+v, want Port 0 and PortName pprof", cfg.Discovery.Pprof)
		}
	})
	t.Run("both", func(t *testing.T) {
		loadErr(t, fixture("both-ports.yaml"), "exactly one of discovery.pprof.port and discovery.pprof.portName")
	})
	t.Run("bad port name", func(t *testing.T) {
		loadErr(t, fixture("bad-name.yaml"), "discovery.pprof.portName")
	})
	t.Run("port out of range", func(t *testing.T) {
		loadErr(t, fixture("bad-port.yaml"), "discovery.pprof.port")
	})
	t.Run("bad version label", func(t *testing.T) {
		t.Setenv("PROFGATE_VERSION_LABEL", "bad label")
		loadErr(t, fixture("good.yaml"), "discovery.versionLabel")
	})
	t.Run("bad listen", func(t *testing.T) {
		t.Setenv("PROFGATE_LISTEN", "nope")
		loadErr(t, fixture("good.yaml"), "server.listen")
	})
	t.Run("same listen", func(t *testing.T) {
		loadErr(t, fixture("same-listen.yaml"), "server.opsListen must differ")
	})
	t.Run("no realms", func(t *testing.T) {
		loadErr(t, fixture("no-realms.yaml"), "realms")
	})
	t.Run("anonymous realm unknown", func(t *testing.T) {
		loadErr(t, fixture("bad-anon.yaml"), `auth.anonymousRealm "nope" is not a realm`)
	})
	t.Run("realm entry invalid", func(t *testing.T) {
		loadErr(t, fixture("bad-entry.yaml"), "realms.developer.namespaces")
	})
	t.Run("profile unknown", func(t *testing.T) {
		loadErr(t, fixture("bad-profile.yaml"), "realms.developer.profiles")
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("PROFGATE_LISTEN", "127.0.0.1:18080")
		t.Setenv("PROFGATE_OPS_LISTEN", "127.0.0.1:19090")
		t.Setenv("PROFGATE_VERSION_LABEL", "example.com/release")
		t.Setenv("PROFGATE_PPROF_PORT", "7070")
		t.Setenv("PROFGATE_LIMIT_CPU_SECONDS", "120")
		t.Setenv("PROFGATE_LIMIT_TRACE_SECONDS", "30")
		t.Setenv("PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES", "4")
		t.Setenv("PROFGATE_AUTH_MODE", "disabled")
		t.Setenv("PROFGATE_ANONYMOUS_REALM", "ops")
		cfg := loadOK(t, fixture("two-realms.yaml"))
		want := config.Config{
			Server:    config.ServerConfig{Listen: "127.0.0.1:18080", OpsListen: "127.0.0.1:19090"},
			Discovery: config.DiscoveryConfig{VersionLabel: "example.com/release", Pprof: config.PprofConfig{Port: 7070}},
			Limits:    config.LimitsConfig{CPUSeconds: 120, TraceSeconds: 30, MaxConcurrentProfiles: 4},
			Auth:      config.AuthConfig{Mode: "disabled", AnonymousRealm: "ops"},
		}
		if cfg.Server != want.Server || cfg.Discovery != want.Discovery || cfg.Limits != want.Limits || cfg.Auth != want.Auth {
			t.Fatalf("Load() = %+v, want %+v", *cfg, want)
		}
	})
	t.Run("env port name", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_PORT_NAME", "pprof")
		cfg := loadOK(t, fixture("neither-port.yaml"))
		if cfg.Discovery.Pprof.Port != 0 || cfg.Discovery.Pprof.PortName != "pprof" {
			t.Fatalf("Pprof = %+v, want Port 0 and PortName pprof", cfg.Discovery.Pprof)
		}
	})

	t.Run("grace period", func(t *testing.T) {
		t.Setenv("PROFGATE_LIMIT_CPU_SECONDS", "60")
		t.Setenv("PROFGATE_LIMIT_TRACE_SECONDS", "90")
		cfg := loadOK(t, fixture("good.yaml"))
		if got := cfg.RequiredGracePeriod(); got != 150*time.Second {
			t.Fatalf("RequiredGracePeriod() = %v, want 150s", got)
		}
	})
}

func TestProfiles(t *testing.T) {
	want := []string{"cpu", "trace", "heap", "allocs", "goroutine", "mutex", "block", "threadcreate"}
	got := config.Profiles()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Profiles() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if config.Profiles()[0] != "cpu" {
		t.Fatal("Profiles() returned shared storage; a caller could mutate the profile list")
	}
	for _, name := range want {
		if !config.IsProfile(name) {
			t.Errorf("IsProfile(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "*", "CPU", "profile"} {
		if config.IsProfile(name) {
			t.Errorf("IsProfile(%q) = true, want false", name)
		}
	}
}

func TestLoadPGO(t *testing.T) {
	t.Run("full example", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-full.yaml"))
		wantNATS := config.NATSConfig{
			URL:            "nats://nats.profgate.svc:4222",
			CredsFile:      fixture("nats.creds"),
			ConnectTimeout: 5 * time.Second,
		}
		if cfg.NATS != wantNATS {
			t.Fatalf("NATS = %+v, want %+v", cfg.NATS, wantNATS)
		}
		want := config.PGOConfig{
			Enabled:      true,
			ConfigAPI:    "enabled",
			LeaseTTL:     60 * time.Second,
			MaxAttempts:  3,
			JobRetention: 168 * time.Hour,
			Limits: config.PGOLimits{
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
			},
			Defaults: config.PGODefaults{
				Schedule: config.PGOScheduleDefaults{Every: 6 * time.Hour, Jitter: 10 * time.Minute},
				Sampling: config.PGOSamplingDefaults{
					Duration:      30 * time.Second,
					Rounds:        2,
					RoundInterval: 30 * time.Second,
					Replicas:      "all",
					MaxParallel:   4,
				},
				Target:   config.PGOTargetDefaults{VersionPolicy: "strict"},
				Artifact: config.PGOArtifactDefaults{Retention: 2 * time.Hour},
			},
		}
		if cfg.PGO != want {
			t.Fatalf("PGO = %+v, want %+v", cfg.PGO, want)
		}
		realm := cfg.Realms["developer"]
		if realm.PGO != (config.RealmPGO{Read: true, Collect: true, Configure: true}) {
			t.Fatalf("realm pgo = %+v, want every flag true", realm.PGO)
		}
	})

	// A round interval of zero runs rounds back-to-back,
	// a setting an operator can write.
	// It is also the field's zero value, so a loader default would erase it.
	t.Run("an explicit zero round interval survives", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-zero-round-interval.yaml"))
		if got := cfg.PGO.Defaults.Sampling.RoundInterval; got != 0 {
			t.Fatalf("roundInterval = %v, want 0", got)
		}
	})

	// The sampling block is present and carries a sibling key,
	// which is the shape an absent roundInterval arrives in.
	t.Run("an absent round interval is 30s", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-replicas-int.yaml"))
		if got := cfg.PGO.Defaults.Sampling.RoundInterval; got != 30*time.Second {
			t.Fatalf("roundInterval = %v, want 30s", got)
		}
	})

	t.Run("realm without pgo", func(t *testing.T) {
		cfg := loadOK(t, fixture("good.yaml"))
		realm := cfg.Realms["developer"]
		if realm.PGO != (config.RealmPGO{}) {
			t.Fatalf("realm pgo = %+v, want every flag false", realm.PGO)
		}
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("PROFGATE_NATS_URL", "tls://one.example:4222,nats://two.example:4222")
		t.Setenv("PROFGATE_NATS_CREDS_FILE", "testdata/nats.creds")
		t.Setenv("PROFGATE_NATS_CONNECT_TIMEOUT", "9s")
		t.Setenv("PROFGATE_PGO_ENABLED", "true")
		t.Setenv("PROFGATE_PGO_CONFIG_API", "disabled")
		t.Setenv("PROFGATE_PGO_LEASE_TTL", "90s")
		t.Setenv("PROFGATE_PGO_MAX_ATTEMPTS", "7")
		t.Setenv("PROFGATE_PGO_JOB_RETENTION", "200h")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_DURATION", "45s")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_ROUNDS", "4")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_PARALLEL", "5")
		t.Setenv("PROFGATE_PGO_LIMIT_MIN_EVERY", "20m")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_EVERY", "12h")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_RETENTION", "10h")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES", "2097152")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES", "4194304")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_TARGETS_PER_ROUND", "16")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS", "3")
		t.Setenv("PROFGATE_PGO_LIMIT_ON_DEMAND_PER_MINUTE", "20")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_LIVE_COLLECTIONS", "128")
		cfg := loadOK(t, fixture("good.yaml"))
		wantNATS := config.NATSConfig{
			URL:            "tls://one.example:4222,nats://two.example:4222",
			CredsFile:      fixture("nats.creds"),
			ConnectTimeout: 9 * time.Second,
		}
		if cfg.NATS != wantNATS {
			t.Fatalf("NATS = %+v, want %+v", cfg.NATS, wantNATS)
		}
		want := config.PGOConfig{
			Enabled:      true,
			ConfigAPI:    "disabled",
			LeaseTTL:     90 * time.Second,
			MaxAttempts:  7,
			JobRetention: 200 * time.Hour,
			Limits: config.PGOLimits{
				MaxDuration:          45 * time.Second,
				MaxRounds:            4,
				MaxParallel:          5,
				MinEvery:             20 * time.Minute,
				MaxEvery:             12 * time.Hour,
				MaxRetention:         10 * time.Hour,
				MaxSampleBytes:       2097152,
				MaxMergedBytes:       4194304,
				MaxTargetsPerRound:   16,
				MaxActiveCollections: 3,
				OnDemandPerMinute:    20,
				MaxLiveCollections:   128,
			},
			Defaults: cfg.PGO.Defaults,
		}
		if cfg.PGO != want {
			t.Fatalf("PGO = %+v, want %+v", cfg.PGO, want)
		}
	})

	t.Run("url required when enabled", func(t *testing.T) {
		loadErr(t, fixture("pgo-no-url.yaml"), "nats.url")
	})
	t.Run("url optional when disabled", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-disabled.yaml"))
		if cfg.PGO.Enabled {
			t.Fatal("pgo.enabled = true, want false")
		}
	})
	t.Run("url scheme", func(t *testing.T) {
		t.Setenv("PROFGATE_NATS_URL", "http://nats.example:4222")
		loadErr(t, fixture("pgo-full.yaml"), "nats.url")
	})
	t.Run("creds file missing", func(t *testing.T) {
		t.Setenv("PROFGATE_NATS_CREDS_FILE", "testdata/no-such.creds")
		loadErr(t, fixture("pgo-full.yaml"), "nats.credsFile")
	})

	t.Run("admission inequality violated", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_PARALLEL", "8")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.maxParallel")
	})
	t.Run("admission inequality satisfied", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_PARALLEL", "6")
		loadOK(t, fixture("pgo-full.yaml"))
	})
	t.Run("record size product", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_ROUNDS", "20")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.maxRounds")
	})
	t.Run("on-demand rate too low", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_ON_DEMAND_PER_MINUTE", "0")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.onDemandPerMinute")
	})
	t.Run("on-demand rate too high", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_ON_DEMAND_PER_MINUTE", "601")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.onDemandPerMinute")
	})
	t.Run("job retention too short", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_JOB_RETENTION", "24h")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.jobRetention")
	})
	t.Run("max every below min every", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_EVERY", "10m")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.maxEvery")
	})
	t.Run("max duration above cpu seconds", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_DURATION", "120s")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.maxDuration")
	})
	t.Run("sample bytes above merged bytes", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES", "2097152")
		loadErr(t, fixture("pgo-full.yaml"), "pgo.limits.maxMergedBytes")
	})
	t.Run("defaults above limits", func(t *testing.T) {
		loadErr(t, fixture("pgo-bad-rounds.yaml"), "pgo.defaults.sampling.rounds")
	})
	t.Run("jitter above half of every", func(t *testing.T) {
		loadErr(t, fixture("pgo-bad-jitter.yaml"), "pgo.defaults.schedule.jitter")
	})

	t.Run("replicas all", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-full.yaml"))
		if cfg.PGO.Defaults.Sampling.Replicas != "all" {
			t.Fatalf("replicas = %q, want all", cfg.PGO.Defaults.Sampling.Replicas)
		}
	})
	t.Run("replicas integer", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-replicas-int.yaml"))
		if cfg.PGO.Defaults.Sampling.Replicas != "3" {
			t.Fatalf("replicas = %q, want 3", cfg.PGO.Defaults.Sampling.Replicas)
		}
	})
	t.Run("replicas not a number", func(t *testing.T) {
		loadErr(t, fixture("pgo-bad-replicas.yaml"), "pgo.defaults.sampling.replicas")
	})
	t.Run("replicas above ceiling", func(t *testing.T) {
		loadErr(t, fixture("pgo-replicas-over.yaml"), "pgo.defaults.sampling.replicas")
	})
}

func TestPGOSizing(t *testing.T) {
	cfg := loadOK(t, fixture("pgo-full.yaml"))
	// The spec's own arithmetic for the shipped ceilings:
	// 2 x (4 x 8 x 32 MiB + 2 x 8 x 64 MiB).
	if got, want := cfg.PGOMemoryBytes(), int64(4*1024*1024*1024); got != want {
		t.Fatalf("PGOMemoryBytes() = %d, want %d", got, want)
	}
	// The deadline formula at the slowest policy the shipped ceilings admit,
	// which samples one Pod at a time:
	// 5 x 32 x (60s + 30s + 660s) + 4 x 600s + 60s.
	if got, want := cfg.RequiredPGOGracePeriod(), 122460*time.Second; got != want {
		t.Fatalf("RequiredPGOGracePeriod() = %v, want %v", got, want)
	}
}
