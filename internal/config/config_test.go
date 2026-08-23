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
