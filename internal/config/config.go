// Package config loads the gateway configuration once at startup:
// a YAML file, PROFGATE_-prefixed environment overrides, defaults,
// normalization, and validation.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
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
	Realms    map[string]Realm `yaml:"realms" validate:"required,min=1,dive"`
}

// ServerConfig holds the two listen addresses.
type ServerConfig struct {
	Listen    string `yaml:"listen"    env:"LISTEN"     default:":8080" validate:"required,hostname_port"`
	OpsListen string `yaml:"opsListen" env:"OPS_LISTEN" default:":9090" validate:"required,hostname_port"`
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
}

// defaultPprofPort is used when neither port nor portName is configured.
const defaultPprofPort = 6060

// gracePeriodSlack is added to the longest profile duration to form the grace period.
const gracePeriodSlack = 60 * time.Second

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

// RequiredGracePeriod is the Deployment grace period the limits demand:
// the longer of the CPU and trace limits plus 60 seconds.
func (c *Config) RequiredGracePeriod() time.Duration {
	return time.Duration(max(c.Limits.CPUSeconds, c.Limits.TraceSeconds))*time.Second + gracePeriodSlack
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

	normalize(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
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

// normalize fills the pprof port when neither port nor portName was given.
func normalize(cfg *Config) {
	if cfg.Discovery.Pprof.Port == 0 && cfg.Discovery.Pprof.PortName == "" {
		cfg.Discovery.Pprof.Port = defaultPprofPort
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
	return nil
}

// isDNSLabel reports whether s is a DNS-1123 label, the rule for namespace and Service names.
func isDNSLabel(s string) bool {
	return len(validation.IsDNS1123Label(s)) == 0
}
