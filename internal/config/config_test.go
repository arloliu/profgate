package config_test

import (
	"log/slog"
	"reflect"
	"slices"
	"strconv"
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
// The block holds a slice, so it cannot be compared with ==,
// and comparing the list by length alone would let a stray entry through.
func wantDiscovery(t *testing.T, got, want config.DiscoveryConfig, selections []config.Selection) {
	t.Helper()
	if got.VersionLabel != want.VersionLabel || got.Pprof.Port != want.Pprof.Port || got.Pprof.PortName != want.Pprof.PortName {
		t.Fatalf("discovery = %+v, want %+v", got, want)
	}
	if !slices.Equal(got.Pprof.AllowedSelections, selections) {
		t.Fatalf("discovery.pprof.allowedSelections = %v, want %v", got.Pprof.AllowedSelections, selections)
	}
}

// The four entry shapes, named once so the tables below read as the spec does.
var (
	port6061  = config.Selection{Kind: config.SelectionPort, Value: "6061"}
	portAny   = config.Selection{Kind: config.SelectionPort, Value: config.AnySelection}
	namePprof = config.Selection{Kind: config.SelectionPortName, Value: "pprof-alt"}
	nameAny   = config.Selection{Kind: config.SelectionPortName, Value: config.AnySelection}
)

// removedKeyMessage is what an operator carrying an older deployment forward
// must read: the key that replaced the old lists and its environment variable.
const removedKeyMessage = "allowedSelections"

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
		wantDiscovery(t, cfg.Discovery, want.Discovery, nil)
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
		loadErr(t, fixture("unknown-top.yaml"), "line 1: unknown key extra")
	})
	t.Run("unknown nested", func(t *testing.T) {
		loadErr(t, fixture("unknown-nested.yaml"), "line 1: unknown key limits.foo")
	})
	t.Run("unknown in realm", func(t *testing.T) {
		loadErr(t, fixture("unknown-realm.yaml"), "line 3: unknown key realms.developer.profilse")
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
		wantDiscovery(t, cfg.Discovery, want.Discovery, nil)
	})
	t.Run("env port name", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_PORT_NAME", "pprof")
		cfg := loadOK(t, fixture("neither-port.yaml"))
		if cfg.Discovery.Pprof.Port != 0 || cfg.Discovery.Pprof.PortName != "pprof" {
			t.Fatalf("Pprof = %+v, want Port 0 and PortName pprof", cfg.Discovery.Pprof)
		}
	})

	// One list bounds what a request may name instead of the configured default.
	// It is empty unless the file writes it, and an empty list admits only the
	// configured default, so a bare configuration loads with no entry at all.
	t.Run("no list", func(t *testing.T) {
		cfg := loadOK(t, fixture("good.yaml"))
		if len(cfg.Discovery.Pprof.AllowedSelections) != 0 {
			t.Fatalf("allowedSelections = %v, want empty", cfg.Discovery.Pprof.AllowedSelections)
		}
	})
	t.Run("list from file", func(t *testing.T) {
		for _, tc := range []struct {
			name, file string
			want       []config.Selection
		}{
			{"numeric entry", "selections-port.yaml", []config.Selection{port6061}},
			{"port wildcard", "selections-port-any.yaml", []config.Selection{portAny}},
			{"named entry", "selections-name.yaml", []config.Selection{namePprof}},
			{"name wildcard", "selections-name-any.yaml", []config.Selection{nameAny}},
			{"a wildcard beside the other kind", "selections-mixed-any.yaml", []config.Selection{portAny, namePprof}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg := loadOK(t, fixture(tc.file))
				want := config.DiscoveryConfig{VersionLabel: "app.kubernetes.io/version", Pprof: config.PprofConfig{Port: 6060}}
				wantDiscovery(t, cfg.Discovery, want, tc.want)
			})
		}
	})
	// An entry is exactly one of port and portName; the entry refuses every
	// other shape itself, because a custom decoder is outside the strict
	// unknown-key pass. A repeated key is two keys, not the last one.
	t.Run("entry shape", func(t *testing.T) {
		for _, tc := range []struct{ name, file, want string }{
			{"both keys", "selections-both-keys.yaml", "exactly one of port and portName"},
			{"no key", "selections-no-keys.yaml", "exactly one of port and portName"},
			{"a third key", "selections-extra-key.yaml", "exactly one of port and portName"},
			{"a repeated key", "selections-repeated-key.yaml", "exactly one of port and portName"},
			{"port zero", "selections-port-zero.yaml", "1-65535"},
			{"port over", "selections-port-over.yaml", "1-65535"},
			{"port letters", "selections-port-letters.yaml", "1-65535"},
			{"name uppercase", "selections-name-upper.yaml", `discovery.pprof.allowedSelections: portName "Pprof"`},
			{"name leading hyphen", "selections-name-hyphen.yaml", `discovery.pprof.allowedSelections: portName "-x"`},
			{"name sixteen characters", "selections-name-long.yaml", `discovery.pprof.allowedSelections: portName "abcdefghijklmnop"`},
			{"name all digits", "selections-name-digits.yaml", `discovery.pprof.allowedSelections: portName "6061"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.file), tc.want)
			})
		}
	})
	// A repeated entry says the operator meant two different values and wrote
	// one twice, and a wildcard beside a concrete entry of its kind leaves the
	// concrete entry deciding nothing; both are startup failures.
	t.Run("list rules", func(t *testing.T) {
		for _, tc := range []struct{ name, file, want string }{
			{"duplicate port", "selections-dup-port.yaml", "discovery.pprof.allowedSelections: duplicate entry port:6061"},
			{"duplicate name", "selections-dup-name.yaml", "discovery.pprof.allowedSelections: duplicate entry portName:pprof-alt"},
			{"port wildcard beside a port", "selections-port-any-beside.yaml", "discovery.pprof.allowedSelections: port:* beside port:6061"},
			{"name wildcard beside a name", "selections-name-any-beside.yaml", "discovery.pprof.allowedSelections: portName:* beside portName:pprof-alt"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.file), tc.want)
			})
		}
	})
	// The two lists this one replaced are refused by name, whatever their
	// value, before the unknown-key pass would call them unknown, so the
	// message says what to write instead.
	t.Run("removed keys", func(t *testing.T) {
		for _, tc := range []struct{ name, file, key string }{
			{"allowedPorts", "removed-ports.yaml", "allowedPorts"},
			{"allowedPortNames", "removed-names.yaml", "allowedPortNames"},
			{"allowedPorts null", "removed-ports-null.yaml", "allowedPorts"},
			{"allowedPortNames null", "removed-names-null.yaml", "allowedPortNames"},
			{"allowedPorts empty", "removed-ports-empty.yaml", "allowedPorts"},
			{"allowedPortNames empty", "removed-names-empty.yaml", "allowedPortNames"},
			{"both empty", "removed-both-empty.yaml", "allowedPorts"},
			{"allowedPorts behind a merge key", "removed-ports-merge.yaml", "allowedPorts"},
			{"allowedPortNames behind a merge sequence", "removed-names-merge-seq.yaml", "allowedPortNames"},
			{"a merged removed key beside the new one", "removed-merge-beside-selections.yaml", "allowedPorts"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := config.Load(fixture(tc.file))
				if err == nil {
					t.Fatalf("Load(%q) = nil error, want the removed-key message", tc.file)
				}
				for _, want := range []string{"discovery.pprof." + tc.key, removedKeyMessage, "PROFGATE_PPROF_ALLOWED_SELECTIONS"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Load(%q) error = %q, want it to contain %q", tc.file, err.Error(), want)
					}
				}
				if strings.Contains(err.Error(), "not found in type") {
					t.Errorf("Load(%q) error = %q: the unknown-key pass spoke before the removed-key check", tc.file, err.Error())
				}
			})
		}
	})
	// The environment variable replaces the file's list rather than adding to
	// it, and set-but-empty is how a deployment narrows an inherited wildcard.
	t.Run("list from env", func(t *testing.T) {
		for _, tc := range []struct {
			name, value string
			want        []config.Selection
		}{
			{"empty replaces the file's list", "", []config.Selection{}},
			{"one number", "port:6061", []config.Selection{port6061}},
			{"both kinds in order", "port:6061,portName:pprof-alt", []config.Selection{port6061, namePprof}},
			{"port wildcard", "port:*", []config.Selection{portAny}},
			{"name wildcard", "portName:*", []config.Selection{nameAny}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_PPROF_ALLOWED_SELECTIONS", tc.value)
				cfg := loadOK(t, fixture("selections-port-any.yaml"))
				if got := cfg.Discovery.Pprof.AllowedSelections; !slices.Equal(got, tc.want) {
					t.Fatalf("allowedSelections = %v, want %v", got, tc.want)
				}
			})
		}
	})
	t.Run("absent env leaves the file's list", func(t *testing.T) {
		cfg := loadOK(t, fixture("selections-port-any.yaml"))
		if got := cfg.Discovery.Pprof.AllowedSelections; !slices.Equal(got, []config.Selection{portAny}) {
			t.Fatalf("allowedSelections = %v, want [%v]", got, portAny)
		}
	})
	// A leading, trailing, or doubled comma is a typo that must not quietly
	// widen or narrow the list, so an empty token is an error.
	t.Run("env errors", func(t *testing.T) {
		for _, tc := range []struct{ name, value, want string }{
			{"leading comma", ",port:6061", "empty token"},
			{"trailing comma", "port:6061,", "empty token"},
			{"doubled comma", "port:6061,,portName:x", "empty token"},
			{"bare number", "6061", "port:<number>, portName:<name>, port:*, or portName:*"},
			{"unknown kind", "ports:6061", "port:<number>, portName:<name>, port:*, or portName:*"},
			{"port over", "port:70000", "1-65535"},
			{"duplicate", "port:6061,port:6061", "duplicate entry port:6061"},
			{"wildcard beside a port", "port:*,port:6061", "port:* beside port:6061"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("PROFGATE_PPROF_ALLOWED_SELECTIONS", tc.value)
				loadErr(t, fixture("good.yaml"), tc.want)
			})
		}
	})
	// The two removed variables are refused on presence, as os.LookupEnv
	// reports it, so a variable set to the empty string is refused too.
	t.Run("removed env", func(t *testing.T) {
		for _, tc := range []struct{ name, variable, value string }{
			{"allowed ports", "PROFGATE_PPROF_ALLOWED_PORTS", "6061"},
			{"allowed port names", "PROFGATE_PPROF_ALLOWED_PORT_NAMES", "pprof-alt"},
			{"allowed ports empty", "PROFGATE_PPROF_ALLOWED_PORTS", ""},
			{"allowed port names empty", "PROFGATE_PPROF_ALLOWED_PORT_NAMES", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(tc.variable, tc.value)
				_, err := config.Load(fixture("good.yaml"))
				if err == nil {
					t.Fatalf("Load with %s set = nil error, want the removed-variable message", tc.variable)
				}
				for _, want := range []string{tc.variable, removedKeyMessage, "PROFGATE_PPROF_ALLOWED_SELECTIONS"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err.Error(), want)
					}
				}
			})
		}
	})
	// The list takes no part in the rule that fills the default port:
	// a gateway that bounds what clients may name still profiles 6060 by default.
	t.Run("neither port still 6060", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_ALLOWED_SELECTIONS", "port:7070")
		cfg := loadOK(t, fixture("neither-port.yaml"))
		if cfg.Discovery.Pprof.Port != 6060 {
			t.Fatalf("port = %d, want 6060", cfg.Discovery.Pprof.Port)
		}
		want := []config.Selection{{Kind: config.SelectionPort, Value: "7070"}}
		if got := cfg.Discovery.Pprof.AllowedSelections; !slices.Equal(got, want) {
			t.Fatalf("allowedSelections = %v, want %v", got, want)
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

// TestDecodeErrorNamesTheKey pins the message of a file the strict decode refuses:
// the file, the line, and the key path, so the operator reads what to fix
// where the library named a Go type and no file.
// A type mismatch keeps the library's own text after the key,
// and where several keys share the line, as a flow mapping writes them,
// the message keeps the file and the line and names no key rather than guessing one.
func TestDecodeErrorNamesTheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		// want is the whole message after "config: <path>: ".
		want string
	}{
		{name: "an unknown key in a nested block", file: "unknown-server.yaml",
			want: "line 3: unknown key server.opsListn"},
		{name: "an unknown key at the top level", file: "unknown-realmz.yaml",
			want: "line 3: unknown key realmz"},
		{name: "an unknown key inside a sequence element", file: "unknown-users-entry.yaml",
			want: "line 8: unknown key auth.oidc.mapping.users[0].nam"},
		{name: "two typos are both named", file: "unknown-two.yaml",
			want: "line 2: unknown key server.opsListn\ntestdata/unknown-two.yaml: line 7: unknown key limits.cpuSecs"},
		{name: "a type mismatch names the key", file: "bad-cpu-seconds.yaml",
			want: "line 4: limits.cpuSeconds: cannot unmarshal !!str `abc` into int"},
		{name: "a type mismatch in a flow mapping names the line", file: "bad-cpu-seconds-flow.yaml",
			want: "line 3: cannot unmarshal !!str `abc` into int"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fixture(tc.file)
			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("Load(%q) = nil error, want %q", path, tc.want)
			}
			if want := "config: " + path + ": " + tc.want; err.Error() != want {
				t.Fatalf("Load(%q) error = %q, want %q", path, err.Error(), want)
			}
			// The library's wording is what the rewrite reads;
			// its type name leaking through means the wording moved.
			if strings.Contains(err.Error(), "not found in type") {
				t.Errorf("Load(%q) error = %q, want no Go type name", path, err.Error())
			}
		})
	}
	t.Run("a valid file still loads", func(t *testing.T) {
		loadOK(t, fixture("pgo-full.yaml"))
	})
}

// TestPprofPortZeroIsRefused covers the written zero:
// discovery.pprof.port is 1-65535, and omitting the key is how the default is taken,
// so a file or variable that writes 0 is refused instead of normalized to 6060
// or read as "unset" beside a portName.
// A null value counts as the key not being written,
// and the variable decides over the file in both directions.
func TestPprofPortZeroIsRefused(t *testing.T) {
	// portZeroMessage is the one refusal, with the source that wrote the zero substituted.
	portZeroMessage := func(source string) string {
		return source + ": 0 is not a port (1-65535); omit it for the default 6060, or set discovery.pprof.portName instead"
	}
	fileMessage := func(name string) string {
		return "config: " + fixture(name) + ": " + portZeroMessage("discovery.pprof.port")
	}
	envMessage := "config: " + portZeroMessage("PROFGATE_PPROF_PORT")
	loadExact := func(t *testing.T, name, want string) {
		t.Helper()
		_, err := config.Load(fixture(name))
		if err == nil || err.Error() != want {
			t.Fatalf("Load(%q) error = %v, want %q", fixture(name), err, want)
		}
	}
	loadPort := func(t *testing.T, name string, want int32) {
		t.Helper()
		cfg := loadOK(t, fixture(name))
		if cfg.Discovery.Pprof.Port != want {
			t.Fatalf("Load(%q) Port = %d, want %d", fixture(name), cfg.Discovery.Pprof.Port, want)
		}
	}

	t.Run("a zero port in the file", func(t *testing.T) {
		loadExact(t, "port-zero.yaml", fileMessage("port-zero.yaml"))
	})
	t.Run("a zero port beside a port name", func(t *testing.T) {
		loadExact(t, "port-zero-with-name.yaml", fileMessage("port-zero-with-name.yaml"))
	})
	t.Run("a zero port from the environment", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_PORT", "0")
		loadExact(t, "neither-port.yaml", envMessage)
	})
	t.Run("a zero variable over a valid file port", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_PORT", "0")
		loadExact(t, "good.yaml", envMessage)
	})
	t.Run("a valid variable over a zero file port", func(t *testing.T) {
		t.Setenv("PROFGATE_PPROF_PORT", "6061")
		loadPort(t, "port-zero.yaml", 6061)
	})
	t.Run("a null port is absent", func(t *testing.T) {
		loadPort(t, "port-null.yaml", 6060)
	})
	t.Run("an absent port still defaults", func(t *testing.T) {
		loadPort(t, "neither-port.yaml", 6060)
	})
	t.Run("a zero allowedSelections entry keeps its own message", func(t *testing.T) {
		loadErr(t, fixture("selections-port-zero.yaml"), "1-65535")
	})
}

// TestPprofAllows covers the rule that decides whether a request may name a
// port or a container-port name.
// Three properties carry it:
// the configured default is permitted whatever the list holds,
// an empty list permits nothing beyond it, the other kind included,
// and a wildcard covers its own kind and no more.
func TestPprofAllows(t *testing.T) {
	list := func(entries ...config.Selection) []config.Selection { return entries }
	port6060 := config.Selection{Kind: config.SelectionPort, Value: "6060"}
	byPort := func(n int32) config.Selection {
		return config.Selection{Kind: config.SelectionPort, Value: strconv.Itoa(int(n))}
	}
	byName := func(name string) config.Selection {
		return config.Selection{Kind: config.SelectionPortName, Value: name}
	}
	for _, tc := range []struct {
		name  string
		pprof config.PprofConfig
		ask   config.Selection // the request's parameter and value
		want  bool
	}{
		{"empty list, the default by number", config.PprofConfig{Port: 6060}, byPort(6060), true},
		{"empty list, a number beyond it", config.PprofConfig{Port: 6060}, byPort(6061), false},
		{"empty list refuses the other kind", config.PprofConfig{Port: 6060}, byName("pprof"), false},
		{"empty list, the default by name", config.PprofConfig{PortName: "pprof"}, byName("pprof"), true},
		{"a named default is no numeric default", config.PprofConfig{PortName: "pprof"}, byPort(6060), false},
		{"a listed number", config.PprofConfig{Port: 6060, AllowedSelections: list(port6061)}, byPort(6061), true},
		{"an unlisted number", config.PprofConfig{Port: 6060, AllowedSelections: list(port6061)}, byPort(6062), false},
		{"an unlisted kind", config.PprofConfig{Port: 6060, AllowedSelections: list(port6061)}, byName("pprof-alt"), false},
		{"listing the default changes nothing", config.PprofConfig{Port: 6060, AllowedSelections: list(port6060)}, byPort(6060), true},
		{"the port wildcard", config.PprofConfig{Port: 6060, AllowedSelections: list(portAny)}, byPort(65535), true},
		{"the port wildcard covers no name", config.PprofConfig{Port: 6060, AllowedSelections: list(portAny)}, byName("pprof-alt"), false},
		{"the name wildcard", config.PprofConfig{Port: 6060, AllowedSelections: list(nameAny)}, byName("anything"), true},
		{"the name wildcard covers no number", config.PprofConfig{Port: 6060, AllowedSelections: list(nameAny)}, byPort(6061), false},
		{"a named default beside a number, the name", config.PprofConfig{PortName: "pprof", AllowedSelections: list(port6061)}, byName("pprof"), true},
		{"a named default beside a number, the number", config.PprofConfig{PortName: "pprof", AllowedSelections: list(port6061)}, byPort(6061), true},
		{"a named default beside a number, another number", config.PprofConfig{PortName: "pprof", AllowedSelections: list(port6061)}, byPort(6060), false},
		{"a named default beside a number, another name", config.PprofConfig{PortName: "pprof", AllowedSelections: list(port6061)}, byName("metrics"), false},
		{"both wildcards, any number", config.PprofConfig{Port: 6060, AllowedSelections: list(portAny, nameAny)}, byPort(9999), true},
		{"both wildcards, any name", config.PprofConfig{Port: 6060, AllowedSelections: list(portAny, nameAny)}, byName("whatever"), true},
		{"the unset default is not a number a request may name", config.PprofConfig{PortName: "pprof", AllowedSelections: list(port6061)}, byPort(0), false},
		{"the unset default is not a name a request may send", config.PprofConfig{Port: 6060, AllowedSelections: list(namePprof)}, byName(""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			if tc.ask.Kind == config.SelectionPort {
				n, err := strconv.ParseInt(tc.ask.Value, 10, 32)
				if err != nil {
					t.Fatalf("row asks for port %q: %v", tc.ask.Value, err)
				}
				got = tc.pprof.AllowsPort(int32(n))
			} else {
				got = tc.pprof.AllowsPortName(tc.ask.Value)
			}
			if got != tc.want {
				t.Fatalf("%s with %+v = %t, want %t", tc.ask, tc.pprof, got, tc.want)
			}
		})
	}
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

// TestRealmEntryMessages covers the message a refused realm entry carries.
// profiles is the one list with a closed set,
// so its refusal names the eight names and the wildcard it accepts;
// namespaces and services hold DNS-1123 labels, a set no message could list,
// and their refusal names the entry alone.
func TestRealmEntryMessages(t *testing.T) {
	loadExact := func(t *testing.T, name, want string) {
		t.Helper()
		_, err := config.Load(fixture(name))
		if err == nil || err.Error() != want {
			t.Fatalf("Load(%q) error = %v, want %q", fixture(name), err, want)
		}
	}

	t.Run("a profile entry is told the accepted names", func(t *testing.T) {
		loadExact(t, "bad-profile.yaml", "config: "+fixture("bad-profile.yaml")+
			`: realms.developer.profiles: invalid entry "heaap"; accepted: cpu, trace, heap, allocs, goroutine, mutex, block, threadcreate, or "*"`)
	})
	t.Run("a namespace entry keeps its message", func(t *testing.T) {
		loadExact(t, "bad-entry.yaml", "config: "+fixture("bad-entry.yaml")+
			`: realms.developer.namespaces: invalid entry "Bad_Name"`)
	})
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
				MaxSampleBytes:       16777216,
				MaxMergedBytes:       33554432,
				MaxTargetsPerRound:   32,
				MaxActiveCollections: 1,
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
				Artifact: config.PGOArtifactDefaults{Retention: 24 * time.Hour},
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
		if got := cfg.PGO.Defaults.Artifact.Retention; got != 24*time.Hour {
			t.Fatalf("retention = %v, want 24h", got)
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
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_RETENTION", "48h")
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
				MaxRetention:         48 * time.Hour,
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
	// Two shipped defaults sit on their own ceiling,
	// so the ceiling cannot move down until the default moves first,
	// and maxEvery ships at its own maximum and cannot move up at all;
	// docs/configuration.md states the pairs and these cases hold the figures.
	t.Run("max parallel below the default", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_PARALLEL", "3")
		loadErr(t, fixture("pgo-full.yaml"),
			"pgo.defaults.sampling.maxParallel 4 must be at most pgo.limits.maxParallel 3")
	})
	t.Run("max every above its maximum", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_EVERY", "25h")
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

	// The one default rule that judges the policy against itself rather than against a ceiling:
	// an artifact kept for less than its own interval leaves no downloadable profile for the tail of each one.
	// It names both keys, and it holds whether or not pgo.enabled.
	t.Run("a default retention under the default interval", func(t *testing.T) {
		const want = "pgo.defaults.artifact.retention 1h0m0s must be at least pgo.defaults.schedule.every 6h0m0s"
		for _, tc := range []struct{ name, fixture string }{
			{"while enabled", "pgo-retention-under-every.yaml"},
			{"while disabled", "pgo-disabled-retention-under-every.yaml"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				loadErr(t, fixture(tc.fixture), want)
			})
		}
	})

	// The ceiling on the same key keeps its own message,
	// which is what tells an operator to raise pgo.limits.maxRetention rather than pgo.defaults.schedule.every.
	t.Run("a default retention above its ceiling", func(t *testing.T) {
		loadErr(t, fixture("pgo-retention-above-max.yaml"),
			"pgo.defaults.artifact.retention 25h0m0s must be at most pgo.limits.maxRetention 24h0m0s")
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

	// Sampling takes no admission slot,
	// so the fan-out a Collection may reach is not measured against the gate interactive requests pass through:
	// 8 times 2 is the whole of limits.maxConcurrentProfiles and loads anyway.
	t.Run("the pgo fan-out is not measured against the interactive gate", func(t *testing.T) {
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_PARALLEL", "8")
		t.Setenv("PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS", "2")
		cfg := loadOK(t, fixture("pgo-full.yaml"))
		if got := cfg.Limits.MaxConcurrentProfiles; got != 16 {
			t.Fatalf("limits.maxConcurrentProfiles = %d, want the default 16 this case is measured against", got)
		}
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
	// versionPolicy was the only key under pgo.defaults.target,
	// so the block is refused by name — whatever it holds, and even when a
	// merge key carries it in — before the unknown-key pass would call it unknown.
	t.Run("removed target block", func(t *testing.T) {
		for _, tc := range []struct{ name, file string }{
			{"target block", "removed-pgo-target.yaml"},
			{"target block set to null", "removed-pgo-target-null.yaml"},
			{"target block behind a merge key", "removed-pgo-target-merge.yaml"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := config.Load(fixture(tc.file))
				if err == nil {
					t.Fatalf("Load(%q) = nil error, want the removed-key message", tc.file)
				}
				if !strings.Contains(err.Error(), "pgo.defaults.target has been removed") {
					t.Errorf("Load(%q) error = %q, want it to name pgo.defaults.target", tc.file, err.Error())
				}
				if strings.Contains(err.Error(), "not found in type") {
					t.Errorf("Load(%q) error = %q: the unknown-key pass spoke before the removed-key check", tc.file, err.Error())
				}
			})
		}
	})
}

func TestPGOSizing(t *testing.T) {
	cfg := loadOK(t, fixture("pgo-full.yaml"))
	// The working set at the shipped ceilings: 1 x (4 x 8 x 16 MiB + 2 x 8 x 32 MiB).
	if got, want := cfg.PGOMemoryBytes(), int64(1<<30); got != want {
		t.Fatalf("PGOMemoryBytes() = %d, want %d", got, want)
	}
	// The container holds that working set and the gateway's own 512 MiB beside it.
	if got, want := cfg.GatewayMemoryBytes(), int64(1536<<20); got != want {
		t.Fatalf("GatewayMemoryBytes() = %d, want %d", got, want)
	}
}

// TestGatewayMemoryWithCollectionOff proves pgo.enabled alone moves the container figure:
// with collection off the container is the gateway's own footprint,
// while the working set the ceilings size is unchanged, because the chart mirrors that arithmetic.
func TestGatewayMemoryWithCollectionOff(t *testing.T) {
	t.Setenv("PROFGATE_PGO_ENABLED", "false")
	cfg := loadOK(t, fixture("pgo-full.yaml"))
	if got, want := cfg.PGOMemoryBytes(), int64(1<<30); got != want {
		t.Fatalf("PGOMemoryBytes() = %d, want %d: the ceiling arithmetic does not read pgo.enabled", got, want)
	}
	if got, want := cfg.GatewayMemoryBytes(), int64(512<<20); got != want {
		t.Fatalf("GatewayMemoryBytes() = %d, want %d: a gateway with collection off needs no merge budget", got, want)
	}
}

// TestGatewayMemoryFollowsEveryCeiling proves the container figure moves with each ceiling
// that sizes the working set, and that the base term does not.
func TestGatewayMemoryFollowsEveryCeiling(t *testing.T) {
	base := loadOK(t, fixture("pgo-full.yaml"))
	baseTerm := base.GatewayMemoryBytes() - base.PGOMemoryBytes()

	for _, tc := range []struct{ name, env, value string }{
		{"maxParallel", "PROFGATE_PGO_LIMIT_MAX_PARALLEL", "8"},
		{"maxSampleBytes", "PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES", "33554432"},
		{"maxMergedBytes", "PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES", "67108864"},
		{"maxActiveCollections", "PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			cfg := loadOK(t, fixture("pgo-full.yaml"))
			if cfg.PGOMemoryBytes() <= base.PGOMemoryBytes() {
				t.Fatalf("raising %s left the working set at %d", tc.name, cfg.PGOMemoryBytes())
			}
			if got, want := cfg.GatewayMemoryBytes(), baseTerm+cfg.PGOMemoryBytes(); got != want {
				t.Fatalf("GatewayMemoryBytes() = %d, want %d: the container follows the working set", got, want)
			}
			if got := cfg.GatewayMemoryBytes() - cfg.PGOMemoryBytes(); got != baseTerm {
				t.Fatalf("the base term is %d, want the fixed %d", got, baseTerm)
			}
		})
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
			loadErr(t, fixture("auth-basic-unknown.yaml"), "line 12: unknown key auth.basic.user")
		})
		t.Run("auth.oidc.browser.clientId", func(t *testing.T) {
			loadErr(t, fixture("auth-browser-unknown.yaml"), "line 13: unknown key auth.oidc.browser.clientId")
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

// TestLoadOIDCCLI covers the auth.oidc.cli block:
// the defaults its presence fills, the rule that binds its client identifier to the audience under ID tokens, the scope rules refused under the block's own key, and the modes
// that cannot carry it at all.
func TestLoadOIDCCLI(t *testing.T) {
	// The default scope list requests offline_access because Dex issues a
	// refresh token only when asked; the browser flow's list does not.
	defaultScopes := []string{"openid", "offline_access"}

	for _, tc := range []struct {
		name string
		file string
		// wantErr lists substrings the load error must all contain; empty means the load must succeed.
		wantErr []string
		// wantCLI is compared with the loaded block when the load succeeds; nil expects an absent block.
		wantCLI *config.OIDCCLI
	}{
		{name: "absent", file: "auth-oidc.yaml"},
		{name: "empty block takes every default", file: "cli-empty.yaml",
			wantCLI: &config.OIDCCLI{ClientID: "profgate", Scopes: defaultScopes}},
		{name: "clientID equals audience under id", file: "cli-clientid-equal.yaml",
			wantCLI: &config.OIDCCLI{ClientID: "profgate", Scopes: defaultScopes}},
		{name: "clientID differs from audience under id", file: "cli-clientid-differs.yaml",
			wantErr: []string{"auth.oidc.cli.clientID", "auth.oidc.audience"}},
		// RFC 9068 access tokens carry the resource as aud, so a second registration does not produce an azp the gateway refuses.
		{name: "clientID differs from audience under access", file: "cli-clientid-differs-access.yaml",
			wantCLI: &config.OIDCCLI{ClientID: "other", Scopes: defaultScopes}},
		{name: "clientID 257 bytes", file: "cli-clientid-long.yaml",
			wantErr: []string{"auth.oidc.cli.clientID", "1 to 256 bytes"}},
		{name: "scopes without openid", file: "cli-scope-noopenid.yaml", wantErr: []string{"auth.oidc.cli.scopes"}},
		{name: "scopes duplicate", file: "cli-scope-dup.yaml", wantErr: []string{"auth.oidc.cli.scopes"}},
		{name: "scope 65 bytes", file: "cli-scope-long.yaml", wantErr: []string{"auth.oidc.cli.scopes"}},
		{name: "scope with a space", file: "cli-scope-space.yaml", wantErr: []string{"auth.oidc.cli.scopes"}},
		{name: "explicit scopes stay as written", file: "cli-scopes.yaml",
			wantCLI: &config.OIDCCLI{ClientID: "profgate", Scopes: []string{"openid", "email"}}},
		// The registration is shared with the browser flow under ID tokens, and
		// a registration holding a secret is confidential where the device grant needs a public client.
		{name: "beside a browser client secret", file: "cli-browser-secret.yaml",
			wantErr: []string{"auth.oidc.cli", "auth.oidc.browser.clientSecretFile"}},
		{name: "beside a public browser client", file: "cli-browser.yaml",
			wantCLI: &config.OIDCCLI{ClientID: "profgate", Scopes: defaultScopes}},
		{name: "under basic", file: "cli-basic.yaml", wantErr: []string{"auth.oidc must not be set"}},
		{name: "under disabled", file: "cli-disabled.yaml", wantErr: []string{"auth.oidc must not be set"}},
		{name: "unknown key", file: "cli-unknown.yaml",
			wantErr: []string{"line 9: unknown key auth.oidc.cli.clientId"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.wantErr) > 0 {
				loadErrAll(t, fixture(tc.file), tc.wantErr...)
				return
			}
			cfg := loadOK(t, fixture(tc.file))
			got := cfg.Auth.OIDC.CLI
			if tc.wantCLI == nil {
				if got != nil {
					t.Fatalf("auth.oidc.cli = %+v, want no block", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("auth.oidc.cli = nil, want a block")
			}
			if !reflect.DeepEqual(*got, *tc.wantCLI) {
				t.Fatalf("auth.oidc.cli = %+v, want %+v", *got, *tc.wantCLI)
			}
		})
	}

	// A scope error under cli names its own key; the browser key would send
	// the operator to a block the file may not even carry.
	t.Run("scope error never names the browser key", func(t *testing.T) {
		_, err := config.Load(fixture("cli-scope-noopenid.yaml"))
		if err == nil || strings.Contains(err.Error(), "browser") {
			t.Fatalf("Load error = %v, want one naming auth.oidc.cli.scopes only", err)
		}
	})

	t.Run("env", func(t *testing.T) {
		// The loader walks a pointer block only when the file creates it;
		// a variable set while the block is absent is refused,
		// which TestOIDCOverridesNeedTheirBlock covers.
		t.Run("clientID variable differs from audience under id", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_CLI_CLIENT_ID", "other")
			loadErrAll(t, fixture("cli-empty.yaml"), "auth.oidc.cli.clientID", "auth.oidc.audience")
		})
		t.Run("pkce variable", func(t *testing.T) {
			t.Setenv("PROFGATE_AUTH_OIDC_CLI_PKCE", "true")
			cfg := loadOK(t, fixture("cli-empty.yaml"))
			if !cfg.Auth.OIDC.CLI.PKCE {
				t.Fatalf("auth.oidc.cli = %+v, want pkce true", *cfg.Auth.OIDC.CLI)
			}
		})
	})
}

// TestOIDCOverridesNeedTheirBlock covers the eight variables that override a key of auth.oidc.browser or auth.oidc.cli.
// The loader never walks a nil pointer,
// so a variable set while its block is absent from the file lands nowhere;
// it is refused instead, naming itself and the block, in every auth.mode.
// The empty-block cases hold the refusal to the block's presence rather than the variable alone:
// a file that opens the block with `{}` and configures it from the environment loads.
func TestOIDCOverridesNeedTheirBlock(t *testing.T) {
	message := func(variable, block string) string {
		return "config: " + variable + " is set but " + block + " is absent; " +
			"the variable overrides a key of that block and only the file opens it, " +
			"so write the block (an empty mapping is enough) or unset the variable"
	}
	// Presence is what is refused, so one variable carries the empty value.
	variables := []struct{ variable, block, value string }{
		{"PROFGATE_AUTH_OIDC_CLIENT_ID", "auth.oidc.browser", "profgate"},
		{"PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE", "auth.oidc.browser", ""},
		{"PROFGATE_AUTH_OIDC_REDIRECT_URL", "auth.oidc.browser", "https://profgate.example/auth/callback"},
		{"PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE", "auth.oidc.browser", fixture("cookie.key")},
		{"PROFGATE_AUTH_OIDC_SESSION_TTL", "auth.oidc.browser", "1h"},
		{"PROFGATE_AUTH_OIDC_TRANSACTION_TTL", "auth.oidc.browser", "2m"},
		{"PROFGATE_AUTH_OIDC_CLI_CLIENT_ID", "auth.oidc.cli", "profgate"},
		{"PROFGATE_AUTH_OIDC_CLI_PKCE", "auth.oidc.cli", "true"},
	}
	for _, file := range []struct{ name, file string }{
		{"oidc mode without the block", "auth-oidc.yaml"},
		{"disabled mode without auth.oidc", "good.yaml"},
	} {
		t.Run(file.name, func(t *testing.T) {
			for _, tc := range variables {
				t.Run(tc.variable, func(t *testing.T) {
					t.Setenv(tc.variable, tc.value)
					_, err := config.Load(fixture(file.file))
					if want := message(tc.variable, tc.block); err == nil || err.Error() != want {
						t.Fatalf("Load(%q) with %s set error = %v, want %q", fixture(file.file), tc.variable, err, want)
					}
				})
			}
		})
	}

	// `browser: {}` is not a valid block on its own;
	// the required keys arrive through their variables, which is what the chart's extraEnv is for.
	t.Run("an empty browser block accepts the override", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_OIDC_CLIENT_ID", "profgate")
		t.Setenv("PROFGATE_AUTH_OIDC_REDIRECT_URL", "https://profgate.example/auth/callback")
		t.Setenv("PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE", fixture("cookie.key"))
		t.Setenv("PROFGATE_AUTH_OIDC_SESSION_TTL", "1h")
		cfg := loadOK(t, fixture("auth-browser-empty.yaml"))
		if cfg.Auth.OIDC.Browser == nil || cfg.Auth.OIDC.Browser.SessionTTL != time.Hour {
			t.Fatalf("auth.oidc.browser = %+v, want sessionTTL 1h", cfg.Auth.OIDC.Browser)
		}
	})
	t.Run("an empty cli block accepts the override", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_OIDC_CLI_PKCE", "true")
		cfg := loadOK(t, fixture("cli-empty.yaml"))
		if cfg.Auth.OIDC.CLI == nil || !cfg.Auth.OIDC.CLI.PKCE {
			t.Fatalf("auth.oidc.cli = %+v, want pkce true", cfg.Auth.OIDC.CLI)
		}
	})
	// auth.basic is governed by auth.mode already, so its variables are outside the rule.
	t.Run("a basic-mode variable is not refused", func(t *testing.T) {
		t.Setenv("PROFGATE_AUTH_BASIC_MAX_CONCURRENT", "8")
		loadOK(t, fixture("good.yaml"))
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
		{name: "unknown key", file: "ui-unknown.yaml", wantErr: "line 16: unknown key ui.path"},
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
