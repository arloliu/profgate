package config_test

import (
	"log/slog"
	"reflect"
	"slices"
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

// wantDiscovery compares a loaded discovery block field by field.
// The block holds two slices, so it cannot be compared with ==,
// and comparing the lists by length alone would let a stray entry through.
func wantDiscovery(t *testing.T, got, want config.DiscoveryConfig, ports []int32, names []string) {
	t.Helper()
	if got.VersionLabel != want.VersionLabel || got.Pprof.Port != want.Pprof.Port || got.Pprof.PortName != want.Pprof.PortName {
		t.Fatalf("discovery = %+v, want %+v", got, want)
	}
	if !slices.Equal(got.Pprof.AllowedPorts, ports) {
		t.Fatalf("discovery.pprof.allowedPorts = %v, want %v", got.Pprof.AllowedPorts, ports)
	}
	if !slices.Equal(got.Pprof.AllowedPortNames, names) {
		t.Fatalf("discovery.pprof.allowedPortNames = %v, want %v", got.Pprof.AllowedPortNames, names)
	}
}

func TestLoad(t *testing.T) {
	t.Run("good", func(t *testing.T) {
		cfg := loadOK(t, fixture("good.yaml"))
		want := config.Config{
			Server: config.ServerConfig{
				Listen: ":8080", OpsListen: ":9090", LogLevel: "info", DrainDelay: 5 * time.Second,
				TLS: config.TLSConfig{MinVersion: "1.2"},
			},
			Discovery: config.DiscoveryConfig{VersionLabel: "app.kubernetes.io/version", Pprof: config.PprofConfig{Port: 6060}},
			Limits:    config.LimitsConfig{CPUSeconds: 60, TraceSeconds: 60, MaxConcurrentProfiles: 16},
			Auth:      config.AuthConfig{Mode: "disabled", AnonymousRealm: "developer"},
		}
		if cfg.Server != want.Server || cfg.Limits != want.Limits || cfg.Auth != want.Auth {
			t.Fatalf("Load() = %+v, want %+v", *cfg, want)
		}
		wantDiscovery(t, cfg.Discovery, want.Discovery, nil, nil)
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
		loadErr(t, fixture("bad-port.yaml"), "discovery.pprof.port: must be at most 65535")
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

	t.Run("log level from file", func(t *testing.T) {
		cfg := loadOK(t, fixture("log-level.yaml"))
		if cfg.Server.LogLevel != "warn" {
			t.Fatalf("server.logLevel = %q, want warn", cfg.Server.LogLevel)
		}
	})
	t.Run("log level values", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			level slog.Level
		}{
			{"debug", slog.LevelDebug},
			{"info", slog.LevelInfo},
			{"warn", slog.LevelWarn},
			{"error", slog.LevelError},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_LOG_LEVEL", tc.name)
				cfg := loadOK(t, fixture("good.yaml"))
				if cfg.Server.LogLevel != tc.name {
					t.Fatalf("server.logLevel = %q, want %q", cfg.Server.LogLevel, tc.name)
				}
				if got := cfg.Server.SlogLevel(); got != tc.level {
					t.Fatalf("SlogLevel() = %v, want %v", got, tc.level)
				}
			})
		}
	})
	t.Run("log level unknown", func(t *testing.T) {
		t.Setenv("PROFGATE_LOG_LEVEL", "verbose")
		loadErr(t, fixture("good.yaml"), "server.logLevel: must be one of: debug, info, warn, error")
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
		t.Setenv("PROFGATE_LOG_LEVEL", "debug")
		t.Setenv("PROFGATE_DRAIN_DELAY", "0s")
		cfg := loadOK(t, fixture("two-realms.yaml"))
		want := config.Config{
			Server: config.ServerConfig{
				Listen: "127.0.0.1:18080", OpsListen: "127.0.0.1:19090", LogLevel: "debug", DrainDelay: 0,
				TLS: config.TLSConfig{MinVersion: "1.2"},
			},
			Discovery: config.DiscoveryConfig{VersionLabel: "example.com/release", Pprof: config.PprofConfig{Port: 7070}},
			Limits:    config.LimitsConfig{CPUSeconds: 120, TraceSeconds: 30, MaxConcurrentProfiles: 4},
			Auth:      config.AuthConfig{Mode: "disabled", AnonymousRealm: "ops"},
		}
		if cfg.Server != want.Server || cfg.Limits != want.Limits || cfg.Auth != want.Auth {
			t.Fatalf("Load() = %+v, want %+v", *cfg, want)
		}
		wantDiscovery(t, cfg.Discovery, want.Discovery, nil, nil)
	})
	t.Run("env port name", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_PORT_NAME", "pprof")
		cfg := loadOK(t, fixture("neither-port.yaml"))
		if cfg.Discovery.Pprof.Port != 0 || cfg.Discovery.Pprof.PortName != "pprof" {
			t.Fatalf("Pprof = %+v, want Port 0 and PortName pprof", cfg.Discovery.Pprof)
		}
	})

	// The two allowlists bound what a request may name instead of the configured default.
	// They are independent, both default empty, and an empty one accepts any value of its parameter,
	// so a bare configuration has to load with both nil rather than with a list nobody wrote.
	t.Run("lists from file", func(t *testing.T) {
		cfg := loadOK(t, fixture("allowed-ports.yaml"))
		want := config.DiscoveryConfig{VersionLabel: "app.kubernetes.io/version", Pprof: config.PprofConfig{Port: 6060}}
		wantDiscovery(t, cfg.Discovery, want, []int32{6060, 6061}, []string{"pprof", "pprof-alt"})
	})
	t.Run("ports from env", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORTS", "6060,6061")
		cfg := loadOK(t, fixture("good.yaml"))
		if got := cfg.Discovery.Pprof.AllowedPorts; !slices.Equal(got, []int32{6060, 6061}) {
			t.Fatalf("allowedPorts = %v, want [6060 6061]", got)
		}
	})
	t.Run("names from env", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORT_NAMES", "pprof,pprof-alt")
		cfg := loadOK(t, fixture("good.yaml"))
		if got := cfg.Discovery.Pprof.AllowedPortNames; !slices.Equal(got, []string{"pprof", "pprof-alt"}) {
			t.Fatalf("allowedPortNames = %v, want [pprof pprof-alt]", got)
		}
	})
	// A ConfigMap or shell that spaces a list out for readability still names
	// the same two ports; the reader that splits the value trims the space.
	t.Run("env with spaces", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORTS", "6060, 6061")
		cfg := loadOK(t, fixture("good.yaml"))
		if got := cfg.Discovery.Pprof.AllowedPorts; !slices.Equal(got, []int32{6060, 6061}) {
			t.Fatalf("allowedPorts = %v, want [6060 6061]", got)
		}
	})
	t.Run("port out of range in file", func(t *testing.T) {
		loadErr(t, fixture("allowed-ports-range.yaml"), "discovery.pprof.allowedPorts")
	})
	t.Run("port out of range in env", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORTS", "65536")
		loadErr(t, fixture("good.yaml"), "discovery.pprof.allowedPorts")
	})
	// The loader refuses the value before validation runs, so the message is
	// the loader's: it names the field and the entry it could not read.
	t.Run("port not a number", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORTS", "6060,abc")
		loadErr(t, fixture("good.yaml"), "field 'AllowedPorts' (tag 'env'): invalid size format: abc")
	})
	t.Run("bad name in file", func(t *testing.T) {
		loadErr(t, fixture("allowed-ports-name.yaml"), `discovery.pprof.allowedPortNames "Pprof"`)
	})
	t.Run("bad name in env", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORT_NAMES", "-x")
		loadErr(t, fixture("good.yaml"), `discovery.pprof.allowedPortNames "-x"`)
	})
	// A repeated entry says the operator meant two different values and wrote
	// one twice, which is worth a startup failure rather than a silent merge.
	t.Run("duplicate port", func(t *testing.T) {
		loadErr(t, fixture("allowed-ports-dup.yaml"), "discovery.pprof.allowedPorts: duplicate entry 6060")
	})
	t.Run("duplicate name", func(t *testing.T) {
		loadErr(t, fixture("allowed-ports-dup-name.yaml"), `discovery.pprof.allowedPortNames: duplicate entry "pprof"`)
	})
	t.Run("unknown key under pprof", func(t *testing.T) {
		loadErr(t, fixture("allowed-ports-unknown.yaml"), "field allowedPort not found in type config.PprofConfig")
	})
	// The lists take no part in the rule that fills the default port:
	// a gateway that bounds what clients may name still profiles 6060 by default.
	t.Run("neither port still 6060", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_PORTS", "7070")
		cfg := loadOK(t, fixture("neither-port.yaml"))
		if cfg.Discovery.Pprof.Port != 6060 {
			t.Fatalf("port = %d, want 6060", cfg.Discovery.Pprof.Port)
		}
		if got := cfg.Discovery.Pprof.AllowedPorts; !slices.Equal(got, []int32{7070}) {
			t.Fatalf("allowedPorts = %v, want [7070]", got)
		}
	})

	t.Run("grace period", func(t *testing.T) {
		t.Setenv("PROFGATE_LIMIT_CPU_SECONDS", "60")
		t.Setenv("PROFGATE_LIMIT_TRACE_SECONDS", "90")
		cfg := loadOK(t, fixture("good.yaml"))
		// The 5-second drain delay, the 90-second trace limit, and 60 seconds of slack.
		if got := cfg.RequiredGracePeriod(); got != 155*time.Second {
			t.Fatalf("RequiredGracePeriod() = %v, want 155s", got)
		}
	})

	t.Run("grace period follows the drain delay", func(t *testing.T) {
		t.Setenv("PROFGATE_DRAIN_DELAY", "20s")
		cfg := loadOK(t, fixture("good.yaml"))
		// A grace period that ignored the delay would cut the drain short by
		// exactly the window the endpoints need.
		if got := cfg.RequiredGracePeriod(); got != 140*time.Second {
			t.Fatalf("RequiredGracePeriod() = %v, want 140s", got)
		}
	})

	t.Run("drain delay out of range", func(t *testing.T) {
		t.Setenv("PROFGATE_DRAIN_DELAY", "61s")
		loadErr(t, fixture("good.yaml"), "server.drainDelay")
	})
}

// TestPprofAllows covers the rule that decides whether a request may name a
// port or a container-port name.
// Three properties carry it:
// an empty list permits any value of its parameter,
// the configured default is permitted whatever the list holds,
// and each list bounds only its own parameter,
// so a numeric default leaves every name to allowedPortNames alone.
func TestPprofAllows(t *testing.T) {
	t.Run("AllowsPort", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			pprof config.PprofConfig
			port  int32
			want  bool
		}{
			{"empty list takes the default", config.PprofConfig{Port: 6060}, 6060, true},
			{"empty list takes any port", config.PprofConfig{Port: 6060}, 9999, true},
			{"the default passes a list without it", config.PprofConfig{Port: 6060, AllowedPorts: []int32{7070}}, 6060, true},
			{"a listed port passes", config.PprofConfig{Port: 6060, AllowedPorts: []int32{7070}}, 7070, true},
			{"an unlisted port is refused", config.PprofConfig{Port: 6060, AllowedPorts: []int32{7070}}, 9999, false},
			{"a name default lists ports on its own", config.PprofConfig{PortName: "pprof", AllowedPorts: []int32{7070}}, 7070, true},
			{"a name default permits no port of its own", config.PprofConfig{PortName: "pprof", AllowedPorts: []int32{7070}}, 6060, false},
			{"the unset default is not a port a request may name", config.PprofConfig{PortName: "pprof", AllowedPorts: []int32{7070}}, 0, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.pprof.AllowsPort(tc.port); got != tc.want {
					t.Fatalf("AllowsPort(%d) with %+v = %t, want %t", tc.port, tc.pprof, got, tc.want)
				}
			})
		}
	})

	t.Run("AllowsPortName", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			pprof    config.PprofConfig
			portName string
			want     bool
		}{
			{"empty list takes the default", config.PprofConfig{PortName: "pprof"}, "pprof", true},
			{"empty list takes any name", config.PprofConfig{PortName: "pprof"}, "x", true},
			{"the default passes a list without it", config.PprofConfig{PortName: "pprof", AllowedPortNames: []string{"metrics"}}, "pprof", true},
			{"a listed name passes", config.PprofConfig{PortName: "pprof", AllowedPortNames: []string{"metrics"}}, "metrics", true},
			{"an unlisted name is refused", config.PprofConfig{PortName: "pprof", AllowedPortNames: []string{"metrics"}}, "x", false},
			{"a numeric default lists names on its own", config.PprofConfig{Port: 6060, AllowedPortNames: []string{"metrics"}}, "metrics", true},
			{"a numeric default permits no name of its own", config.PprofConfig{Port: 6060, AllowedPortNames: []string{"metrics"}}, "pprof", false},
			{"the unset default is not a name a request may send", config.PprofConfig{Port: 6060, AllowedPortNames: []string{"metrics"}}, "", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.pprof.AllowsPortName(tc.portName); got != tc.want {
					t.Fatalf("AllowsPortName(%q) with %+v = %t, want %t", tc.portName, tc.pprof, got, tc.want)
				}
			})
		}
	})
}

// TestLoadTLS covers the presence-implied TLS block: two paths that are set
// together or not at all. The pairing rule is the point -- a gateway that
// serves HTTPS with only half a key pair is not a state the process should be
// able to reach, so half a block is a startup failure rather than a listener
// that fails every handshake.
func TestLoadTLS(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		cfg := loadOK(t, fixture("good.yaml"))
		if cfg.Server.TLS.Enabled() {
			t.Fatalf("server.tls = %+v, want the plaintext default", cfg.Server.TLS)
		}
		if cfg.Server.TLS.MinVersion != "1.2" {
			t.Fatalf("server.tls.minVersion = %q, want the 1.2 default", cfg.Server.TLS.MinVersion)
		}
	})

	t.Run("from file", func(t *testing.T) {
		cfg := loadOK(t, fixture("tls.yaml"))
		want := config.TLSConfig{CertFile: "testdata/tls.crt", KeyFile: "testdata/tls.key", MinVersion: "1.3"}
		if cfg.Server.TLS != want {
			t.Fatalf("server.tls = %+v, want %+v", cfg.Server.TLS, want)
		}
		if !cfg.Server.TLS.Enabled() {
			t.Fatal("server.tls names both files, so Enabled() must report true")
		}
	})

	t.Run("from env", func(t *testing.T) {
		t.Setenv("PROFGATE_TLS_CERT_FILE", fixture("tls.crt"))
		t.Setenv("PROFGATE_TLS_KEY_FILE", fixture("tls.key"))
		t.Setenv("PROFGATE_TLS_MIN_VERSION", "1.3")
		cfg := loadOK(t, fixture("good.yaml"))
		want := config.TLSConfig{CertFile: fixture("tls.crt"), KeyFile: fixture("tls.key"), MinVersion: "1.3"}
		if cfg.Server.TLS != want {
			t.Fatalf("server.tls = %+v, want %+v", cfg.Server.TLS, want)
		}
	})

	t.Run("key without cert", func(t *testing.T) {
		t.Setenv("PROFGATE_TLS_KEY_FILE", fixture("tls.key"))
		loadErr(t, fixture("good.yaml"), "server.tls.certFile")
	})

	t.Run("cert without key", func(t *testing.T) {
		t.Setenv("PROFGATE_TLS_CERT_FILE", fixture("tls.crt"))
		loadErr(t, fixture("good.yaml"), "server.tls.keyFile")
	})

	t.Run("cert file missing", func(t *testing.T) {
		t.Setenv("PROFGATE_TLS_CERT_FILE", fixture("no-such.crt"))
		t.Setenv("PROFGATE_TLS_KEY_FILE", fixture("tls.key"))
		loadErr(t, fixture("good.yaml"), "server.tls.certFile")
	})

	t.Run("key file missing", func(t *testing.T) {
		t.Setenv("PROFGATE_TLS_CERT_FILE", fixture("tls.crt"))
		t.Setenv("PROFGATE_TLS_KEY_FILE", fixture("no-such.key"))
		loadErr(t, fixture("good.yaml"), "server.tls.keyFile")
	})

	t.Run("min version unknown", func(t *testing.T) {
		t.Setenv("PROFGATE_TLS_MIN_VERSION", "1.1")
		loadErr(t, fixture("good.yaml"), "server.tls.minVersion")
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

	// The floors the four keys carry apply to what the operator wrote,
	// not to a key left out: an absent key still takes its shipped default.
	t.Run("absent sampling and artifact keys take their defaults", func(t *testing.T) {
		cfg := loadOK(t, fixture("pgo-replicas-int.yaml"))
		sampling := cfg.PGO.Defaults.Sampling
		if sampling.Duration != 30*time.Second || sampling.Rounds != 2 || sampling.MaxParallel != 4 {
			t.Fatalf("sampling = %+v, want duration 30s, rounds 2, maxParallel 4", sampling)
		}
		if got := cfg.PGO.Defaults.Artifact.Retention; got != 2*time.Hour {
			t.Fatalf("retention = %v, want 2h", got)
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

	// An explicit zero reaches the loader untouched.
	// Each of these four keys carries the floor the Collection API holds an override to:
	// a zero duration samples nothing,
	// and zero rounds or zero maxParallel describes a Collection that does no work.
	t.Run("a default below its floor is refused", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			fixture string
			want    string
		}{
			{"zero duration", "pgo-zero-duration.yaml", "pgo.defaults.sampling.duration: must be at least 1s"},
			{"zero rounds", "pgo-zero-rounds.yaml", "pgo.defaults.sampling.rounds: must be at least 1"},
			{"zero maxParallel", "pgo-zero-max-parallel.yaml", "pgo.defaults.sampling.maxParallel: must be at least 1"},
			{"zero retention", "pgo-zero-retention.yaml", "pgo.defaults.artifact.retention: must be at least 1m"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.fixture), tc.want)
			})
		}
	})

	// A pgo block that contradicts itself is an error the day it is written,
	// not the day pgo.enabled flips to true,
	// and it names the same key either way.
	t.Run("defaults above limits while disabled", func(t *testing.T) {
		loadErr(t, fixture("pgo-disabled-bad-rounds.yaml"), "pgo.defaults.sampling.rounds")
	})
	t.Run("limits contradict each other while disabled", func(t *testing.T) {
		loadErr(t, fixture("pgo-disabled-bad-retention.yaml"), "pgo.jobRetention")
	})

	// The rules that measure the PGO ceilings against the interactive limits
	// wait for pgo.enabled:
	// a gateway that never collects may hold limits.maxConcurrentProfiles 1,
	// which no maxParallel and maxActiveCollections pair stays below,
	// and a limits.cpuSeconds under the shipped pgo.limits.maxDuration.
	t.Run("interactive limits are not measured against pgo while disabled", func(t *testing.T) {
		t.Setenv("PROFGATE_LIMIT_MAX_CONCURRENT_PROFILES", "1")
		t.Setenv("PROFGATE_LIMIT_CPU_SECONDS", "30")
		loadOK(t, fixture("pgo-disabled.yaml"))
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
	// 5 x 32 x (60s + 30s + 660s) + 4 x 600s + 60s, after the 5-second drain delay.
	if got, want := cfg.RequiredPGOGracePeriod(), 122465*time.Second; got != want {
		t.Fatalf("RequiredPGOGracePeriod() = %v, want %v", got, want)
	}
}

// loadErrAll loads path and fails the test unless the error mentions every want.
// The authentication rules that relate two keys name both, so a single
// substring would not show that the message points at the pair.
func loadErrAll(t *testing.T, path string, wants ...string) {
	t.Helper()
	_, err := config.Load(path)
	if err == nil {
		t.Fatalf("Load(%q) = nil error, want one containing %q", path, wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load(%q) error = %q, want it to contain %q", path, err.Error(), want)
		}
	}
}

func TestLoadAuth(t *testing.T) {
	t.Run("disabled unchanged", func(t *testing.T) {
		cfg := loadOK(t, fixture("good.yaml"))
		if cfg.Auth.Mode != "disabled" || cfg.Auth.AnonymousRealm != "developer" {
			t.Fatalf("auth = %+v, want disabled with anonymousRealm developer", cfg.Auth)
		}
		if cfg.Auth.Basic != nil || cfg.Auth.OIDC != nil {
			t.Fatalf("auth = %+v, want no basic and no oidc block", cfg.Auth)
		}
	})

	// A realm name is sealed into the session cookie, so it is held to a
	// DNS-1123 label and its 63-byte bound the same way a namespace is.
	t.Run("realm name label", func(t *testing.T) {
		for _, tc := range []struct{ name, file, realm string }{
			{"empty", "auth-realm-empty.yaml", "realms."},
			{"64 bytes", "auth-realm-long.yaml", "realms." + strings.Repeat("b", 64)},
			{"uppercase", "auth-realm-upper.yaml", "realms.Developer"},
			{"underscore", "auth-realm-under.yaml", "realms.dev_team"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErrAll(t, fixture(tc.file), tc.realm, "DNS-1123")
			})
		}
		t.Run("63 bytes", func(t *testing.T) {
			loadOK(t, fixture("auth-realm-63.yaml"))
		})
	})

	// fuda never walks a nil pointer, so a default inside a block nobody wrote
	// cannot make that block look configured.
	t.Run("absent block stays nil", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_MAX_CONCURRENT", "8")
		cfg := loadOK(t, fixture("good.yaml"))
		if cfg.Auth.Basic != nil {
			t.Fatalf("auth.basic = %+v, want nil", *cfg.Auth.Basic)
		}
	})

	// anonymousRealm is the wide-open realm every request gets in disabled mode.
	// Refusing it in the other two modes is what stops a mode change from
	// silently activating a realm nobody re-read.
	t.Run("anonymousRealm required in disabled", func(t *testing.T) {
		loadErr(t, fixture("auth-disabled-no-anon.yaml"), "auth.anonymousRealm")
	})
	t.Run("anonymousRealm forbidden in basic", func(t *testing.T) {
		t.Setenv("PROFGATE_ANONYMOUS_REALM", "developer")
		loadErr(t, fixture("auth-basic.yaml"), "auth.anonymousRealm")
	})
	t.Run("anonymousRealm forbidden in oidc", func(t *testing.T) {
		t.Setenv("PROFGATE_ANONYMOUS_REALM", "developer")
		loadErr(t, fixture("auth-oidc.yaml"), "auth.anonymousRealm")
	})

	// A block that does not apply cannot be mistaken for one that does.
	t.Run("basic block under oidc", func(t *testing.T) {
		loadErr(t, fixture("auth-oidc-basic.yaml"), "auth.basic")
	})
	t.Run("oidc block under basic", func(t *testing.T) {
		loadErr(t, fixture("auth-basic-oidc.yaml"), "auth.oidc")
	})
	t.Run("basic block under disabled", func(t *testing.T) {
		loadErr(t, fixture("auth-disabled-basic.yaml"), "auth.basic")
	})
	t.Run("basic needs a block", func(t *testing.T) {
		loadErr(t, fixture("auth-basic-none.yaml"), "auth.basic")
	})
	t.Run("oidc needs a block", func(t *testing.T) {
		loadErr(t, fixture("auth-oidc-none.yaml"), "auth.oidc")
	})

	t.Run("basic ok", func(t *testing.T) {
		cfg := loadOK(t, fixture("auth-basic.yaml"))
		basic := cfg.Auth.Basic
		if basic == nil {
			t.Fatal("auth.basic = nil, want a block")
		}
		if basic.MaxConcurrent != 16 || basic.AllowPlaintext || basic.UsersFile != "" {
			t.Fatalf("auth.basic = %+v, want maxConcurrent 16 and no other setting", *basic)
		}
		if len(basic.Users) != 1 || basic.Users[0].Name != "alice" || basic.Users[0].Realm != "developer" {
			t.Fatalf("auth.basic.users = %+v, want one alice in developer", basic.Users)
		}
	})

	t.Run("user name rules", func(t *testing.T) {
		for _, tc := range []struct{ name, file string }{
			{"empty", "auth-basic-name-empty.yaml"},
			{"257 bytes", "auth-basic-name-long.yaml"},
			{"colon", "auth-basic-name-colon.yaml"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.file), "auth.basic.users[0].name")
			})
		}
	})

	// Only hashes are accepted, so a plaintext password in configuration
	// fails at startup instead of becoming a password nobody can use.
	t.Run("hash grammar", func(t *testing.T) {
		for _, tc := range []struct{ name, file string }{
			{"plaintext", "auth-basic-hash-plain.yaml"},
			{"52 trailing characters", "auth-basic-hash-short.yaml"},
			{"md5 prefix", "auth-basic-hash-prefix.yaml"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.file), "auth.basic.users[0].passwordHash")
			})
		}
	})

	t.Run("cost range", func(t *testing.T) {
		for _, tc := range []struct{ name, file, cost string }{
			{"cost 09", "auth-basic-cost-low.yaml", "9"},
			{"cost 15", "auth-basic-cost-high.yaml", "15"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErrAll(t, fixture(tc.file), `"alice"`, tc.cost)
			})
		}
	})

	t.Run("mixed costs", func(t *testing.T) {
		loadErrAll(t, fixture("auth-basic-cost-mixed.yaml"), `"alice"`, `"bob"`, "12", "10")
	})
	t.Run("mixed costs across file", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_USERS_FILE", fixture("auth-users-cost10.yaml"))
		loadErrAll(t, fixture("auth-basic.yaml"), "auth.basic.usersFile", `"alice"`, `"bob"`, "12", "10")
	})

	t.Run("user realm", func(t *testing.T) {
		loadErr(t, fixture("auth-basic-realm.yaml"), "auth.basic.users[0].realm")
	})

	t.Run("duplicate names", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_USERS_FILE", fixture("auth-users-dup.yaml"))
		loadErrAll(t, fixture("auth-basic.yaml"), "auth.basic.usersFile", `"alice"`)
	})

	t.Run("no users", func(t *testing.T) {
		loadErrAll(t, fixture("auth-basic-empty.yaml"), "auth.basic", "at least one user")
	})

	t.Run("users file unreadable", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_USERS_FILE", fixture("nonexistent.yaml"))
		loadErr(t, fixture("auth-basic.yaml"), "auth.basic.usersFile")
	})
	t.Run("users file unknown key", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_USERS_FILE", fixture("auth-users-unknown.yaml"))
		loadErrAll(t, fixture("auth-basic.yaml"), "auth.basic.usersFile", "field password not found")
	})
	// The file half of the user set is what a Secret volume carries,
	// so a readable file at the shared cost simply loads.
	t.Run("users file merges", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_USERS_FILE", fixture("auth-users.yaml"))
		cfg := loadOK(t, fixture("auth-basic.yaml"))
		if cfg.Auth.Basic.UsersFile != fixture("auth-users.yaml") {
			t.Fatalf("auth.basic.usersFile = %q, want the file the environment named", cfg.Auth.Basic.UsersFile)
		}
	})

	// Basic sends the password on every request, so plaintext is refused
	// unless the operator says the network is already protected.
	t.Run("plaintext refused", func(t *testing.T) {
		loadErrAll(t, fixture("auth-basic-plain.yaml"), "auth.basic.allowPlaintext", "server.tls")
	})
	t.Run("plaintext allowed", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_ALLOW_PLAINTEXT", "true")
		loadOK(t, fixture("auth-basic-plain.yaml"))
	})

	t.Run("maxConcurrent range", func(t *testing.T) {
		for _, value := range []string{"0", "1025"} {
			t.Run(value, func(t *testing.T) {
				t.Setenv("PROFGATE_AUTH_BASIC_MAX_CONCURRENT", value)
				loadErr(t, fixture("auth-basic.yaml"), "auth.basic.maxConcurrent")
			})
		}
	})

	t.Run("oidc ok", func(t *testing.T) {
		cfg := loadOK(t, fixture("auth-oidc.yaml"))
		oidc := cfg.Auth.OIDC
		if oidc == nil {
			t.Fatal("auth.oidc = nil, want a block")
		}
		want := config.OIDCConfig{
			Issuer: "https://issuer.example", Audience: "profgate",
			TokenType: "id", UsernameClaim: "sub", GroupsClaim: "groups",
			DiscoveryTimeout: 30 * time.Second, ClockSkew: 30 * time.Second,
			JWKSRefresh: time.Hour, JWKSRefreshMin: time.Minute, JWKSMaxStale: 24 * time.Hour,
		}
		got := *oidc
		got.Mapping = config.OIDCMapping{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("auth.oidc = %+v, want %+v", got, want)
		}
		if oidc.Mapping.DefaultRealm != "developer" || oidc.Browser != nil {
			t.Fatalf("auth.oidc = %+v, want defaultRealm developer and no browser block", *oidc)
		}
	})

	t.Run("issuer required", func(t *testing.T) {
		loadErr(t, fixture("auth-oidc-no-issuer.yaml"), "auth.oidc.issuer")
	})
	// A plaintext issuer has no override. auth.basic.allowPlaintext covers the
	// gateway's own listener behind an Ingress and could not reach this rule
	// anyway: no basic block can exist alongside oidc mode.
	t.Run("issuer plaintext", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_OIDC_ISSUER", "http://issuer.example")
		loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.issuer")
	})
	t.Run("issuer shape", func(t *testing.T) {
		for _, tc := range []struct{ name, value string }{
			{"userinfo", "https://user@issuer.example"},
			{"fragment", "https://issuer.example/#x"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_AUTH_OIDC_ISSUER", tc.value)
				loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.issuer")
			})
		}
	})

	t.Run("audience", func(t *testing.T) {
		t.Run("absent", func(t *testing.T) {
			loadErr(t, fixture("auth-oidc-no-audience.yaml"), "auth.oidc.audience")
		})
		t.Run("257 bytes", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_AUDIENCE", strings.Repeat("a", 257))
			loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.audience")
		})
	})

	t.Run("claim lengths", func(t *testing.T) {
		t.Run("usernameClaim 65 bytes", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_USERNAME_CLAIM", strings.Repeat("a", 65))
			loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.usernameClaim")
		})
		t.Run("groupsClaim empty", func(t *testing.T) {
			loadErr(t, fixture("auth-oidc-claim-empty.yaml"), "auth.oidc.groupsClaim")
		})
	})

	// The CA is opened and parsed here so a path typo or an empty file names
	// the key at startup rather than failing every discovery fetch later.
	t.Run("caFile", func(t *testing.T) {
		for _, tc := range []struct{ name, value string }{
			{"nonexistent", fixture("nonexistent.pem")},
			{"no certificate", fixture("auth-ca-empty.pem")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_AUTH_OIDC_CA_FILE", tc.value)
				loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.caFile")
			})
		}
		t.Run("one certificate", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_CA_FILE", fixture("auth-ca.pem"))
			loadOK(t, fixture("auth-oidc.yaml"))
		})
	})

	t.Run("httpProxy", func(t *testing.T) {
		t.Run("ftp", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_HTTP_PROXY", "ftp://x")
			loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.httpProxy")
		})
		t.Run("http", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_HTTP_PROXY", "http://proxy:3128")
			loadOK(t, fixture("auth-oidc.yaml"))
		})
	})

	t.Run("duration ranges", func(t *testing.T) {
		for _, tc := range []struct{ name, env, value, key string }{
			{"discoveryTimeout 0s", "PROFGATE_AUTH_OIDC_DISCOVERY_TIMEOUT", "0s", "auth.oidc.discoveryTimeout"},
			{"clockSkew 6m", "PROFGATE_AUTH_OIDC_CLOCK_SKEW", "6m", "auth.oidc.clockSkew"},
			{"jwksRefresh 30s", "PROFGATE_AUTH_OIDC_JWKS_REFRESH", "30s", "auth.oidc.jwksRefresh"},
			{"jwksRefreshMin 2h", "PROFGATE_AUTH_OIDC_JWKS_REFRESH_MIN", "2h", "auth.oidc.jwksRefreshMin"},
			{"jwksMaxStale 8d", "PROFGATE_AUTH_OIDC_JWKS_MAX_STALE", "192h", "auth.oidc.jwksMaxStale"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(tc.env, tc.value)
				loadErr(t, fixture("auth-oidc.yaml"), tc.key)
			})
		}
	})

	// A set the gateway may keep trusting for less time than it waits to
	// refresh would go stale on every cycle.
	t.Run("maxStale below refresh", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_OIDC_JWKS_REFRESH", "2h")
		t.Setenv("PROFGATE_AUTH_OIDC_JWKS_MAX_STALE", "1h")
		loadErrAll(t, fixture("auth-oidc.yaml"), "auth.oidc.jwksMaxStale", "auth.oidc.jwksRefresh")
	})

	// An oidc gateway with no mapping at all can admit nobody.
	t.Run("mapping empty", func(t *testing.T) {
		loadErr(t, fixture("auth-oidc-no-mapping.yaml"), "auth.oidc.mapping")
	})
	t.Run("mapping names", func(t *testing.T) {
		for _, tc := range []struct{ name, file, key string }{
			{"empty", "auth-oidc-map-empty.yaml", "auth.oidc.mapping.users[0].name"},
			{"257 bytes", "auth-oidc-map-long.yaml", "auth.oidc.mapping.users[0].name"},
			{"duplicate group", "auth-oidc-map-dup.yaml", "auth.oidc.mapping.groups"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.file), tc.key)
			})
		}
	})
	t.Run("mapping realms", func(t *testing.T) {
		t.Run("user", func(t *testing.T) {
			loadErr(t, fixture("auth-oidc-map-realm.yaml"), "auth.oidc.mapping.users[0].realm")
		})
		t.Run("default", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_DEFAULT_REALM", "nobody")
			loadErr(t, fixture("auth-oidc.yaml"), "auth.oidc.mapping.defaultRealm")
		})
	})
	// An empty defaultRealm is the key written out rather than a realm named "",
	// so it loads and the default step simply never matches.
	t.Run("defaultRealm empty is unset", func(t *testing.T) {
		cfg := loadOK(t, fixture("auth-oidc-groups.yaml"))
		if cfg.Auth.OIDC.Mapping.DefaultRealm != "" {
			t.Fatalf("mapping.defaultRealm = %q, want empty", cfg.Auth.OIDC.Mapping.DefaultRealm)
		}
	})

	t.Run("browser ok", func(t *testing.T) {
		cfg := loadOK(t, fixture("auth-browser.yaml"))
		browser := cfg.Auth.OIDC.Browser
		if browser == nil {
			t.Fatal("auth.oidc.browser = nil, want a block")
		}
		if !slices.Equal(browser.Scopes, []string{"openid", "profile", "email"}) {
			t.Fatalf("scopes = %v, want [openid profile email]", browser.Scopes)
		}
		if browser.SessionTTL != 8*time.Hour || browser.TransactionTTL != 5*time.Minute {
			t.Fatalf("browser = %+v, want sessionTTL 8h and transactionTTL 5m", *browser)
		}
	})

	// The ID token the flow receives carries the client ID in aud and is
	// verified against audience; two values would validate and admit nobody.
	t.Run("clientID equals audience", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_OIDC_CLIENT_ID", "other")
		loadErrAll(t, fixture("auth-browser.yaml"), "auth.oidc.browser.clientID", "auth.oidc.audience")
	})

	t.Run("clientSecretFile", func(t *testing.T) {
		for _, tc := range []struct{ name, value string }{
			{"nonexistent", fixture("nonexistent")},
			{"empty", fixture("auth-secret-empty")},
			{"1025 bytes", fixture("auth-secret-big")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE", tc.value)
				loadErr(t, fixture("auth-browser.yaml"), "auth.oidc.browser.clientSecretFile")
			})
		}
		t.Run("1024 bytes", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE", fixture("auth-secret"))
			loadOK(t, fixture("auth-browser.yaml"))
		})
	})

	t.Run("redirectURL", func(t *testing.T) {
		for _, tc := range []struct{ name, value string }{
			{"plaintext", "http://profgate.example/auth/callback"},
			{"userinfo", "https://u@profgate.example/auth/callback"},
			{"query", "https://profgate.example/auth/callback?x=1"},
			{"fragment", "https://profgate.example/auth/callback#f"},
			{"other path", "https://profgate.example/callback"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_AUTH_OIDC_REDIRECT_URL", tc.value)
				loadErr(t, fixture("auth-browser.yaml"), "auth.oidc.browser.redirectURL")
			})
		}
	})

	t.Run("scopes", func(t *testing.T) {
		for _, tc := range []struct{ name, file string }{
			{"without openid", "auth-browser-scope-noopenid.yaml"},
			{"duplicate", "auth-browser-scope-dup.yaml"},
			{"65 bytes", "auth-browser-scope-long.yaml"},
			{"with a space", "auth-browser-scope-space.yaml"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.file), "auth.oidc.browser.scopes")
			})
		}
	})

	t.Run("cookieKeyFile", func(t *testing.T) {
		t.Run("absent", func(t *testing.T) {
			loadErr(t, fixture("auth-browser-no-key.yaml"), "auth.oidc.browser.cookieKeyFile")
		})
		t.Run("unreadable", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE", fixture("nonexistent.key"))
			loadErr(t, fixture("auth-browser.yaml"), "auth.oidc.browser.cookieKeyFile")
		})
	})

	t.Run("ttl ranges", func(t *testing.T) {
		for _, tc := range []struct{ name, env, value, key string }{
			{"sessionTTL 4m", "PROFGATE_AUTH_OIDC_SESSION_TTL", "4m", "auth.oidc.browser.sessionTTL"},
			{"sessionTTL 25h", "PROFGATE_AUTH_OIDC_SESSION_TTL", "25h", "auth.oidc.browser.sessionTTL"},
			{"transactionTTL 30s", "PROFGATE_AUTH_OIDC_TRANSACTION_TTL", "30s", "auth.oidc.browser.transactionTTL"},
			{"transactionTTL 16m", "PROFGATE_AUTH_OIDC_TRANSACTION_TTL", "16m", "auth.oidc.browser.transactionTTL"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(tc.env, tc.value)
				loadErr(t, fixture("auth-browser.yaml"), tc.key)
			})
		}
	})

	// The session cookie carries Secure and a __Host- prefix, which a plaintext
	// listener cannot set, and no escape hatch covers that:
	// auth.basic.allowPlaintext lives in a block oidc mode cannot carry.
	t.Run("browser needs tls", func(t *testing.T) {
		loadErr(t, fixture("auth-browser-plain.yaml"), "server.tls")
	})
	t.Run("browser needs id tokens", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_OIDC_TOKEN_TYPE", "access")
		loadErr(t, fixture("auth-browser.yaml"), "auth.oidc.tokenType")
	})

	t.Run("unknown keys", func(t *testing.T) {
		t.Run("auth.basic.user", func(t *testing.T) {
			loadErr(t, fixture("auth-basic-unknown.yaml"), "field user not found in type config.BasicConfig")
		})
		t.Run("auth.oidc.browser.clientId", func(t *testing.T) {
			loadErr(t, fixture("auth-browser-unknown.yaml"), "field clientId not found in type config.OIDCBrowser")
		})
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Run("basic", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_MODE", "basic")
			t.Setenv("PROFGATE_AUTH_BASIC_USERS_FILE", fixture("auth-users.yaml"))
			t.Setenv("PROFGATE_AUTH_BASIC_ALLOW_PLAINTEXT", "true")
			t.Setenv("PROFGATE_AUTH_BASIC_MAX_CONCURRENT", "32")
			cfg := loadOK(t, fixture("auth-basic-plain.yaml"))
			want := config.BasicConfig{
				UsersFile: fixture("auth-users.yaml"), AllowPlaintext: true, MaxConcurrent: 32,
			}
			got := *cfg.Auth.Basic
			got.Users = nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("auth.basic = %+v, want %+v", got, want)
			}
		})

		t.Run("oidc", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_MODE", "oidc")
			t.Setenv("PROFGATE_AUTH_OIDC_ISSUER", "https://other.example/realms/x")
			t.Setenv("PROFGATE_AUTH_OIDC_AUDIENCE", "gateway")
			t.Setenv("PROFGATE_AUTH_OIDC_TOKEN_TYPE", "access")
			t.Setenv("PROFGATE_AUTH_OIDC_USERNAME_CLAIM", "preferred_username")
			t.Setenv("PROFGATE_AUTH_OIDC_GROUPS_CLAIM", "roles")
			t.Setenv("PROFGATE_AUTH_OIDC_CA_FILE", fixture("auth-ca.pem"))
			t.Setenv("PROFGATE_AUTH_OIDC_HTTP_PROXY", "socks5://proxy:1080")
			t.Setenv("PROFGATE_AUTH_OIDC_DISCOVERY_TIMEOUT", "45s")
			t.Setenv("PROFGATE_AUTH_OIDC_CLOCK_SKEW", "10s")
			t.Setenv("PROFGATE_AUTH_OIDC_JWKS_REFRESH", "2h")
			t.Setenv("PROFGATE_AUTH_OIDC_JWKS_REFRESH_MIN", "30s")
			t.Setenv("PROFGATE_AUTH_OIDC_JWKS_MAX_STALE", "48h")
			t.Setenv("PROFGATE_AUTH_OIDC_DEFAULT_REALM", "developer")
			cfg := loadOK(t, fixture("auth-oidc.yaml"))
			want := config.OIDCConfig{
				Issuer: "https://other.example/realms/x", Audience: "gateway", TokenType: "access",
				UsernameClaim: "preferred_username", GroupsClaim: "roles",
				CAFile: fixture("auth-ca.pem"), HTTPProxy: "socks5://proxy:1080",
				DiscoveryTimeout: 45 * time.Second, ClockSkew: 10 * time.Second,
				JWKSRefresh: 2 * time.Hour, JWKSRefreshMin: 30 * time.Second, JWKSMaxStale: 48 * time.Hour,
			}
			got := *cfg.Auth.OIDC
			got.Mapping = config.OIDCMapping{}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("auth.oidc = %+v, want %+v", got, want)
			}
			if cfg.Auth.OIDC.Mapping.DefaultRealm != "developer" {
				t.Fatalf("mapping.defaultRealm = %q, want developer", cfg.Auth.OIDC.Mapping.DefaultRealm)
			}
		})

		t.Run("browser", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_CLIENT_ID", "gateway")
			t.Setenv("PROFGATE_AUTH_OIDC_AUDIENCE", "gateway")
			t.Setenv("PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE", fixture("auth-secret"))
			t.Setenv("PROFGATE_AUTH_OIDC_REDIRECT_URL", "https://gw.example/auth/callback")
			t.Setenv("PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE", fixture("cookie.key"))
			t.Setenv("PROFGATE_AUTH_OIDC_SESSION_TTL", "12h")
			t.Setenv("PROFGATE_AUTH_OIDC_TRANSACTION_TTL", "10m")
			cfg := loadOK(t, fixture("auth-browser.yaml"))
			want := config.OIDCBrowser{
				ClientID: "gateway", ClientSecretFile: fixture("auth-secret"),
				RedirectURL: "https://gw.example/auth/callback", CookieKeyFile: fixture("cookie.key"),
				SessionTTL: 12 * time.Hour, TransactionTTL: 10 * time.Minute,
			}
			got := *cfg.Auth.OIDC.Browser
			got.Scopes = nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("auth.oidc.browser = %+v, want %+v", got, want)
			}
		})
	})
}

// TestLoadUI covers the console key: where its value comes from, what a value
// that is not a boolean does, and the rule that holds an enabled console to a
// browser login under oidc.
func TestLoadUI(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		// env is PROFGATE_UI_ENABLED; an empty string leaves it unset.
		env string
		// wantErr is a substring of the load error; empty means the load must succeed.
		wantErr string
		want    bool
	}{
		{name: "absent", file: "good.yaml"},
		{name: "from file", file: "ui-enabled.yaml", want: true},
		{name: "from env", file: "good.yaml", env: "true", want: true},
		{name: "env false over file", file: "ui-enabled.yaml", env: "false"},
		// The decoder refuses the value before validation runs, so the message
		// is the decoder's: it names the value it could not read as a boolean.
		{name: "not boolean", file: "ui-not-bool.yaml", wantErr: "cannot unmarshal !!str `yes-please` into bool"},
		{name: "unknown key", file: "ui-unknown.yaml", wantErr: "field path not found in type config.UIConfig"},
		{name: "under basic", file: "ui-basic.yaml", want: true},
		{
			name: "under oidc without browser", file: "ui-oidc.yaml",
			wantErr: "ui.enabled requires auth.oidc.browser when auth.mode is oidc",
		},
		{name: "under oidc with browser", file: "ui-oidc-browser.yaml", want: true},
		// The rule reads only an enabled console: an issuer with no browser
		// block stays a valid configuration for a gateway that serves no page.
		{name: "off under oidc without browser", file: "ui-oidc-off.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PROFGATE_UI_ENABLED", tc.env)
			}
			if tc.wantErr != "" {
				loadErr(t, fixture(tc.file), tc.wantErr)
				return
			}
			cfg := loadOK(t, fixture(tc.file))
			if cfg.UI.Enabled != tc.want {
				t.Fatalf("ui.enabled = %v, want %v", cfg.UI.Enabled, tc.want)
			}
		})
	}
}
