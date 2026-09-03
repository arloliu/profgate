// Package config loads the gateway configuration once at startup:
// a YAML file, PROFGATE_-prefixed environment overrides, defaults,
// normalization, and validation.
package config

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
	UI        UIConfig         `yaml:"ui"`
	Realms    map[string]Realm `yaml:"realms" validate:"required,min=1,dive"`
}

// ServerConfig holds the two listen addresses, the log level,
// and how long the drain waits before it closes the API listener.
type ServerConfig struct {
	Listen    string `yaml:"listen"    env:"LISTEN"     default:":8080" validate:"required,hostname_port"`
	OpsListen string `yaml:"opsListen" env:"OPS_LISTEN" default:":9090" validate:"required,hostname_port"`
	LogLevel  string `yaml:"logLevel"  env:"LOG_LEVEL"  default:"info"  validate:"oneof=debug info warn error"`
	// DrainDelay is the window between /readyz turning 503 and the API
	// listener closing, so the EndpointSlice controllers and every kube-proxy
	// have time to stop routing new requests to this replica.
	// A preStop hook is where a deployment usually buys that window;
	// the image is distroless and has no shell to run one,
	// and the lifecycle "sleep" action is newer than the Kubernetes baseline.
	// Zero is allowed, and turns the wait off for local runs and tests.
	DrainDelay time.Duration `yaml:"drainDelay" env:"DRAIN_DELAY" default:"5s" validate:"min=0,max=60s"`
	// TLS turns the API listener into an HTTPS listener.
	// A block naming neither file is the plaintext default.
	TLS TLSConfig `yaml:"tls"`
}

// TLSConfig names the certificate the API listener serves and the key that
// signs its handshakes.
// There is no enabled flag: the two paths are the statement, the way
// nats.credsFile is, because they start no subsystem.
// The ops listener has no TLS block at all and is always plaintext.
type TLSConfig struct {
	CertFile   string `yaml:"certFile"   env:"TLS_CERT_FILE"`
	KeyFile    string `yaml:"keyFile"    env:"TLS_KEY_FILE"`
	MinVersion string `yaml:"minVersion" env:"TLS_MIN_VERSION" default:"1.2" validate:"oneof=1.2 1.3"`
}

// Enabled reports whether the API listener serves HTTPS.
// Validation has already rejected a block naming one file and not the other,
// so either path answers the question.
func (t TLSConfig) Enabled() bool {
	return t.CertFile != ""
}

// DiscoveryConfig controls how Pods are matched and which port serves pprof.
type DiscoveryConfig struct {
	VersionLabel string      `yaml:"versionLabel" env:"VERSION_LABEL" default:"app.kubernetes.io/version" validate:"required"`
	Pprof        PprofConfig `yaml:"pprof"`
}

// PprofConfig names the default pprof port by number or by container-port name
// and bounds what a request may name instead.
// Port 0 means unset; normalization sets it to 6060 when PortName is also empty.
// AllowedSelections is default-deny: an empty list admits only the configured default.
type PprofConfig struct {
	Port     int32  `yaml:"port"     env:"PPROF_PORT" validate:"min=0,max=65535"`
	PortName string `yaml:"portName" env:"PPROF_PORT_NAME"`
	// AllowedSelections carries no env tag on purpose. fuda applies an env tag
	// whenever the variable is set, the empty value included, and reads a
	// slice from one CSV record, which cannot express the empty list an empty
	// PROFGATE_PPROF_ALLOWED_SELECTIONS must produce; Load reads the variable itself.
	AllowedSelections []Selection `yaml:"allowedSelections"`
}

// SelectionKind is the request parameter one allowedSelections entry bounds.
type SelectionKind string

// The two request parameters an entry may bound.
const (
	SelectionPort     SelectionKind = "port"
	SelectionPortName SelectionKind = "portName"
)

// AnySelection is the wildcard an entry carries in place of a concrete value.
const AnySelection = "*"

// Selection is one entry of discovery.pprof.allowedSelections:
// the parameter it bounds and the value it admits, which is a decimal port
// number, a container-port name, or AnySelection.
type Selection struct {
	Kind  SelectionKind
	Value string
}

// UnmarshalYAML reads one entry: a mapping carrying exactly one of port and
// portName, whose scalar is a number, a name, or "*".
// A custom decoder is outside the strict unknown-key pass, so the entry
// refuses every other shape itself. It counts the keys it is handed rather
// than assigning them, so a mapping that repeats one key is two keys.
func (s *Selection) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return errors.New("discovery.pprof.allowedSelections: an entry names exactly one of port and portName")
	}
	key, value := node.Content[0], node.Content[1]
	kind := SelectionKind(key.Value)
	if (kind != SelectionPort && kind != SelectionPortName) || value.Kind != yaml.ScalarNode {
		return errors.New("discovery.pprof.allowedSelections: an entry names exactly one of port and portName")
	}
	sel, err := newSelection(kind, value.Value)
	if err != nil {
		return fmt.Errorf("discovery.pprof.allowedSelections: %w", err)
	}
	*s = sel

	return nil
}

// MarshalJSON writes the one-key object /v1/limits reports:
// {"port":6061}, {"port":"*"}, {"portName":"pprof-alt"}, {"portName":"*"}.
func (s Selection) MarshalJSON() ([]byte, error) {
	var value any = s.Value
	if s.Kind == SelectionPort && s.Value != AnySelection {
		n, err := strconv.ParseInt(s.Value, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("selection %s: %w", s, err)
		}
		value = n
	}

	return json.Marshal(map[string]any{string(s.Kind): value})
}

// String is the environment token form: "port:6061", "portName:*".
func (s Selection) String() string {
	return string(s.Kind) + ":" + s.Value
}

// ParseSelection reads one environment token:
// port:<number>, portName:<name>, port:*, or portName:*.
func ParseSelection(token string) (Selection, error) {
	kind, value, ok := strings.Cut(token, ":")
	if !ok || (kind != string(SelectionPort) && kind != string(SelectionPortName)) {
		return Selection{}, fmt.Errorf("token %q: want port:<number>, portName:<name>, port:*, or portName:*", token)
	}

	return newSelection(SelectionKind(kind), value)
}

// newSelection checks the value's grammar for its kind:
// the wildcard, a port in 1-65535, or a container-port name.
func newSelection(kind SelectionKind, value string) (Selection, error) {
	if value == AnySelection {
		return Selection{Kind: kind, Value: value}, nil
	}
	switch kind {
	case SelectionPort:
		n, err := strconv.ParseInt(value, 10, 32)
		if err != nil || n < 1 || n > 65535 {
			return Selection{}, fmt.Errorf("port %q: must be 1-65535 or %q", value, AnySelection)
		}
		value = strconv.FormatInt(n, 10)
	case SelectionPortName:
		if msgs := validation.IsValidPortName(value); len(msgs) > 0 {
			return Selection{}, fmt.Errorf("portName %q: %s", value, strings.Join(msgs, "; "))
		}
	}

	return Selection{Kind: kind, Value: value}, nil
}

// AllowsPort reports whether a request may name port n: the configured
// default always, an entry holding that number, or the port wildcard.
// A gateway whose default is a name has no default number,
// which is why the zero Port is not a default anything matches.
func (p PprofConfig) AllowsPort(n int32) bool {
	if p.Port != 0 && n == p.Port {
		return true
	}
	value := strconv.FormatInt(int64(n), 10)
	for _, sel := range p.AllowedSelections {
		if sel.Kind == SelectionPort && (sel.Value == AnySelection || sel.Value == value) {
			return true
		}
	}

	return false
}

// AllowsPortName reports whether a request may name that container port:
// the configured default always, an entry holding that name, or the name
// wildcard.
// A gateway whose default is numeric has no default name,
// which is why the empty name is not a default anything matches.
func (p PprofConfig) AllowsPortName(name string) bool {
	if p.PortName != "" && name == p.PortName {
		return true
	}
	for _, sel := range p.AllowedSelections {
		if sel.Kind == SelectionPortName && (sel.Value == AnySelection || sel.Value == name) {
			return true
		}
	}

	return false
}

// LimitsConfig caps profile durations and concurrency.
type LimitsConfig struct {
	CPUSeconds            int `yaml:"cpuSeconds"            env:"LIMIT_CPU_SECONDS"             default:"60" validate:"min=1,max=86400"`
	TraceSeconds          int `yaml:"traceSeconds"          env:"LIMIT_TRACE_SECONDS"           default:"60" validate:"min=1,max=86400"`
	MaxConcurrentProfiles int `yaml:"maxConcurrentProfiles" env:"LIMIT_MAX_CONCURRENT_PROFILES" default:"16" validate:"min=1,max=1024"`
}

// AuthConfig selects the authentication mode and the block that mode reads.
// AnonymousRealm is the realm every request gets in disabled mode, and is a
// validation error in the other two, so a mode change cannot silently activate
// a realm nobody re-read.
// Basic and OIDC are pointers so an absent block and one present with only
// defaults are distinguishable: the loader never walks a nil pointer, which is
// what stops an environment default from making an absent block look configured.
type AuthConfig struct {
	Mode           string       `yaml:"mode"           env:"AUTH_MODE"       default:"disabled" validate:"oneof=disabled basic oidc"`
	AnonymousRealm string       `yaml:"anonymousRealm" env:"ANONYMOUS_REALM"`
	Basic          *BasicConfig `yaml:"basic"`
	OIDC           *OIDCConfig  `yaml:"oidc"`
}

// BasicConfig is the user list basic mode authenticates against and the gate
// that bounds what unauthenticated traffic can spend on bcrypt.
type BasicConfig struct {
	Users          []BasicUser `yaml:"users"`
	UsersFile      string      `yaml:"usersFile"      env:"AUTH_BASIC_USERS_FILE"`
	AllowPlaintext bool        `yaml:"allowPlaintext" env:"AUTH_BASIC_ALLOW_PLAINTEXT" default:"false"`
	MaxConcurrent  int         `yaml:"maxConcurrent"  env:"AUTH_BASIC_MAX_CONCURRENT"  default:"16" validate:"min=1,max=1024"`
}

// OIDCConfig points oidc mode at an issuer and says how its tokens are read.
// Everything that establishes trust — the issuer, the audience, the CA, the
// client, the key paths — is read once at startup.
type OIDCConfig struct {
	Issuer           string        `yaml:"issuer"           env:"AUTH_OIDC_ISSUER"`
	Audience         string        `yaml:"audience"         env:"AUTH_OIDC_AUDIENCE"`
	TokenType        string        `yaml:"tokenType"        env:"AUTH_OIDC_TOKEN_TYPE"        default:"id" validate:"oneof=id access"`
	UsernameClaim    string        `yaml:"usernameClaim"    env:"AUTH_OIDC_USERNAME_CLAIM"    default:"sub"    validate:"min=1,max=64"`
	GroupsClaim      string        `yaml:"groupsClaim"      env:"AUTH_OIDC_GROUPS_CLAIM"      default:"groups" validate:"min=1,max=64"`
	CAFile           string        `yaml:"caFile"           env:"AUTH_OIDC_CA_FILE"`
	HTTPProxy        string        `yaml:"httpProxy"        env:"AUTH_OIDC_HTTP_PROXY"`
	DiscoveryTimeout time.Duration `yaml:"discoveryTimeout" env:"AUTH_OIDC_DISCOVERY_TIMEOUT" default:"30s" validate:"min=1s,max=10m"`
	ClockSkew        time.Duration `yaml:"clockSkew"        env:"AUTH_OIDC_CLOCK_SKEW"        default:"30s" validate:"min=0,max=5m"`
	JWKSRefresh      time.Duration `yaml:"jwksRefresh"      env:"AUTH_OIDC_JWKS_REFRESH"      default:"1h"  validate:"min=1m,max=24h"`
	JWKSRefreshMin   time.Duration `yaml:"jwksRefreshMin"   env:"AUTH_OIDC_JWKS_REFRESH_MIN"  default:"1m"  validate:"min=1s,max=1h"`
	JWKSMaxStale     time.Duration `yaml:"jwksMaxStale"     env:"AUTH_OIDC_JWKS_MAX_STALE"    default:"24h" validate:"max=168h"`
	Mapping          OIDCMapping   `yaml:"mapping"`
	Browser          *OIDCBrowser  `yaml:"browser"`
	CLI              *OIDCCLI      `yaml:"cli"`
}

// OIDCMappingEntry maps one username or one group name to a realm.
type OIDCMappingEntry struct {
	Name  string `yaml:"name"`
	Realm string `yaml:"realm"`
}

// OIDCMapping turns a verified token into a realm: the username list first,
// then the group list in the order written, then DefaultRealm when it is set.
type OIDCMapping struct {
	Users        []OIDCMappingEntry `yaml:"users"`
	Groups       []OIDCMappingEntry `yaml:"groups"`
	DefaultRealm string             `yaml:"defaultRealm" env:"AUTH_OIDC_DEFAULT_REALM"`
}

// OIDCBrowser is the optional relying-party block that turns an
// authorization-code login into an encrypted session cookie.
// Its presence is what creates the three /auth/ routes.
type OIDCBrowser struct {
	ClientID         string        `yaml:"clientID"         env:"AUTH_OIDC_CLIENT_ID"`
	ClientSecretFile string        `yaml:"clientSecretFile" env:"AUTH_OIDC_CLIENT_SECRET_FILE"`
	RedirectURL      string        `yaml:"redirectURL"      env:"AUTH_OIDC_REDIRECT_URL"`
	Scopes           []string      `yaml:"scopes"`
	CookieKeyFile    string        `yaml:"cookieKeyFile"    env:"AUTH_OIDC_COOKIE_KEY_FILE"`
	SessionTTL       time.Duration `yaml:"sessionTTL"       env:"AUTH_OIDC_SESSION_TTL"     default:"8h" validate:"min=5m,max=24h"`
	TransactionTTL   time.Duration `yaml:"transactionTTL"   env:"AUTH_OIDC_TRANSACTION_TTL" default:"5m" validate:"min=1m,max=15m"`
}

// OIDCCLI is the optional block that tells a command-line client this gateway's issuer admits a device login.
// Its presence is what makes GET /v1/auth report an oidc object;
// the gateway performs no device grant of its own and holds no client secret for the command line.
type OIDCCLI struct {
	ClientID string   `yaml:"clientID" env:"AUTH_OIDC_CLI_CLIENT_ID"`
	Scopes   []string `yaml:"scopes"`
	PKCE     bool     `yaml:"pkce"     env:"AUTH_OIDC_CLI_PKCE" default:"false"`
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
	MaxSampleBytes       int64         `yaml:"maxSampleBytes"       env:"PGO_LIMIT_MAX_SAMPLE_BYTES"       default:"16777216" validate:"min=1048576,max=268435456"`
	MaxMergedBytes       int64         `yaml:"maxMergedBytes"       env:"PGO_LIMIT_MAX_MERGED_BYTES"       default:"33554432" validate:"max=1073741824"`
	MaxTargetsPerRound   int           `yaml:"maxTargetsPerRound"   env:"PGO_LIMIT_MAX_TARGETS_PER_ROUND"  default:"32"       validate:"min=1,max=256"`
	MaxActiveCollections int           `yaml:"maxActiveCollections" env:"PGO_LIMIT_MAX_ACTIVE_COLLECTIONS" default:"1"        validate:"min=1"`
	OnDemandPerMinute    int           `yaml:"onDemandPerMinute"    env:"PGO_LIMIT_ON_DEMAND_PER_MINUTE"   default:"10"       validate:"min=1,max=600"`
	MaxLiveCollections   int           `yaml:"maxLiveCollections"   env:"PGO_LIMIT_MAX_LIVE_COLLECTIONS"   default:"64"       validate:"min=1,max=1024"`
}

// PGODefaults is the policy a Service gets before any override.
// It carries no environment overrides: it is policy, like realms.
// Each field holds the floor the Collection API holds an operator override to,
// so a value written here and the same value sent to the API are judged alike.
type PGODefaults struct {
	Schedule PGOScheduleDefaults `yaml:"schedule"`
	Sampling PGOSamplingDefaults `yaml:"sampling"`
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
	Duration      time.Duration `yaml:"duration"      default:"30s"  validate:"min=1s"`
	Rounds        int           `yaml:"rounds"        default:"2"    validate:"min=1"`
	RoundInterval time.Duration `yaml:"roundInterval" default:"30s"  validate:"min=0,max=10m"`
	Replicas      string        `yaml:"replicas"      default:"all"`
	MaxParallel   int           `yaml:"maxParallel"   default:"4"    validate:"min=1"`
}

// PGOArtifactDefaults is how long a finished profile is kept by default.
type PGOArtifactDefaults struct {
	Retention time.Duration `yaml:"retention" default:"24h" validate:"min=1m"`
}

// UIConfig is the console block.
// Enabled is restart-only: it decides which routes the handler registers.
type UIConfig struct {
	Enabled bool `yaml:"enabled" env:"UI_ENABLED" default:"false"`
}

// defaultPprofPort is used when neither port nor portName is configured.
const defaultPprofPort = 6060

// The three authentication modes, as auth.mode names them; cmd/profgate
// branches on them when it builds the authenticator.
const (
	ModeDisabled = "disabled"
	ModeBasic    = "basic"
	ModeOIDC     = "oidc"
)

// maxAudienceBytes and maxMappingNameBytes bound the token values a mapping
// compares, so a token cannot make the gateway hold an unbounded string.
const (
	maxAudienceBytes    = 256
	maxMappingNameBytes = 256
)

// callbackPath is the only path the browser flow accepts as a redirect target,
// because it is the path the gateway serves the callback on.
const callbackPath = "/auth/callback"

// maxClientSecretBytes bounds the trimmed contents of clientSecretFile.
const maxClientSecretBytes = 1024

// maxScopeBytes bounds one authorization-request scope.
const maxScopeBytes = 64

// openidScope is the scope that makes the authorization request an OpenID
// Connect request; without it the issuer returns no ID token.
const openidScope = "openid"

// defaultScopes is the browser flow's scope list when the operator writes none.
// A default tag cannot express a list, so normalize applies it.
var defaultScopes = [...]string{openidScope, "profile", "email"}

// browserDefaultScopes returns a copy of the browser flow's default scope list.
func browserDefaultScopes() []string {
	return slices.Clone(defaultScopes[:])
}

// cliScopes is the command-line client's scope list when the operator writes none.
// It differs from the browser flow's
// because Dex issues a refresh token only when offline_access is requested.
var cliScopes = [...]string{openidScope, "offline_access"}

// cliDefaultScopes returns a copy of the command-line client's default scopes.
func cliDefaultScopes() []string {
	return slices.Clone(cliScopes[:])
}

// proxySchemes are the schemes auth.oidc.httpProxy may name.
var proxySchemes = [...]string{"http", "https", "socks5"}

// proxySchemeNames returns a copy of the schemes auth.oidc.httpProxy may name.
func proxySchemeNames() []string {
	return slices.Clone(proxySchemes[:])
}

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
// server.drainDelay, then the longer of the CPU and trace limits plus 60 seconds.
func (c *Config) RequiredGracePeriod() time.Duration {
	return c.Server.DrainDelay +
		time.Duration(max(c.Limits.CPUSeconds, c.Limits.TraceSeconds))*time.Second + gracePeriodSlack
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
	// PGOGatewayBaseMemory is what the gateway process costs before it decodes
	// a profile: the Go runtime, the informer caches,
	// and limits.maxConcurrentProfiles transfer buffers.
	// It is what the container needs with collection off,
	// and it does not stop existing when collection is on.
	PGOGatewayBaseMemory = 512 << 20
)

// PGOMemoryBytes is the PGO working set at the configured ceilings, and nothing else:
// per active Collection, every in-flight sample as compressed bytes,
// decompressed bytes, and a decoded profile;
// the running merged profile; and the serialized copy written to the store.
// It is a sizing rule, not a proof: the decoded sizes are an estimate.
func (c *Config) PGOMemoryBytes() int64 {
	l := c.PGO.Limits
	perCollection := int64(l.MaxParallel)*PGODecodeFactor*l.MaxSampleBytes + 2*PGODecodeFactor*l.MaxMergedBytes

	return int64(l.MaxActiveCollections) * perCollection
}

// GatewayMemoryBytes is the container memory limit this configuration needs.
// With collection off it is what the process costs before it decodes anything;
// with collection on it is that footprint plus the working set PGOMemoryBytes sizes.
// This is the figure the Deployment carries.
func (c *Config) GatewayMemoryBytes() int64 {
	if !c.PGO.Enabled {
		return PGOGatewayBaseMemory
	}

	return PGOGatewayBaseMemory + c.PGOMemoryBytes()
}

// Load reads the YAML file at path, rejects unknown keys at any nesting level,
// applies defaults and PROFGATE_-prefixed environment overrides, normalizes, and validates.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is the operator's --config flag; reading it is the purpose
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// The removed names are refused first, by name, so an operator carrying an
	// older deployment forward reads what to write instead of "unknown key".
	// A variable no field claims is invisible to fuda, so the two variables
	// are looked up here; presence is what is refused, the empty value included.
	for _, name := range []string{"PROFGATE_PPROF_ALLOWED_PORTS", "PROFGATE_PPROF_ALLOWED_PORT_NAMES"} {
		if _, ok := os.LookupEnv(name); ok {
			return nil, fmt.Errorf("config: %s has been removed; set PROFGATE_PPROF_ALLOWED_SELECTIONS (discovery.pprof.allowedSelections) instead", name)
		}
	}
	if err := refuseRemovedPprofKeys(b); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := refuseRemovedPGOTarget(b); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
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
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if value, ok := os.LookupEnv("PROFGATE_PPROF_ALLOWED_SELECTIONS"); ok {
		selections, err := parseSelections(value)
		if err != nil {
			return nil, fmt.Errorf("config: PROFGATE_PPROF_ALLOWED_SELECTIONS: %w", err)
		}
		cfg.Discovery.Pprof.AllowedSelections = selections
	}

	normalize(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// refuseRemovedPprofKeys fails when the file still sets discovery.pprof.allowedPorts
// or discovery.pprof.allowedPortNames, whatever their value: null and [] are set keys too.
// Each mapping on the way down is decoded into a map, which is what lets a
// merge key count: yaml.v3 resolves << while decoding a mapping into a map,
// so a removed key carried in by an anchored mapping, or by a sequence of
// them, is present, where a walk over the mapping's own key nodes would see
// only <<. A missing discovery or pprof mapping means neither key is set,
// and a value that is not a mapping is left to the strict decode to refuse.
func refuseRemovedPprofKeys(b []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil //nolint:nilerr // the strict decode reports the syntax error with its own message
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	node := *doc.Content[0]
	for _, key := range []string{"discovery", "pprof"} {
		child, ok := mappingKeys(node)[key]
		if !ok {
			return nil
		}
		node = child
	}
	for _, key := range []string{"allowedPorts", "allowedPortNames"} {
		if _, ok := mappingKeys(node)[key]; ok {
			return fmt.Errorf("discovery.pprof.%s has been removed; list what a client may name under discovery.pprof.allowedSelections or PROFGATE_PPROF_ALLOWED_SELECTIONS instead", key)
		}
	}

	return nil
}

// refuseRemovedPGOTarget fails when the file still sets pgo.defaults.target,
// whatever the mapping holds: versionPolicy was its only key, so the block goes with it.
// The walk is the one refuseRemovedPprofKeys makes, for the same reason:
// mappingKeys resolves << while decoding, so a block carried in by an anchored
// mapping counts, where a walk over the mapping's own key nodes would see only <<.
// A missing pgo or defaults mapping means the block is not set,
// and a value that is not a mapping is left to the strict decode to refuse.
// No PROFGATE_ variable ever set this key, so the file is the only place it can appear.
func refuseRemovedPGOTarget(b []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil //nolint:nilerr // the strict decode reports the syntax error with its own message
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	node := *doc.Content[0]
	for _, key := range []string{"pgo", "defaults"} {
		child, ok := mappingKeys(node)[key]
		if !ok {
			return nil
		}
		node = child
	}
	if _, ok := mappingKeys(node)["target"]; ok {
		return errors.New("pgo.defaults.target has been removed; every Collection pins the one version its Pods agree on, so nothing replaces it")
	}

	return nil
}

// mappingKeys returns the keys a mapping node carries, merge keys resolved,
// each with its value node; nil for anything that is not a mapping.
// Values stay nodes, so nothing below the mapping is decoded here.
func mappingKeys(node yaml.Node) map[string]yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	var keys map[string]yaml.Node
	if err := node.Decode(&keys); err != nil {
		return nil
	}

	return keys
}

// parseSelections reads the comma-separated tokens of PROFGATE_PPROF_ALLOWED_SELECTIONS.
// The empty value is the empty list; inside a non-empty value an empty
// token is an error rather than a dropped entry.
func parseSelections(value string) ([]Selection, error) {
	if value == "" {
		return []Selection{}, nil
	}
	tokens := strings.Split(value, ",")
	selections := make([]Selection, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, errors.New("empty token; a leading, trailing, or doubled comma")
		}
		sel, err := ParseSelection(token)
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}

	return selections, nil
}

// normalize fills the pprof port,
// whose default depends on another key and so cannot be a `default` tag:
// a file that names the port through portName leaves port absent,
// and fuda fills an absent field from its tag.
func normalize(cfg *Config) {
	if cfg.Discovery.Pprof.Port == 0 && cfg.Discovery.Pprof.PortName == "" {
		cfg.Discovery.Pprof.Port = defaultPprofPort
	}
	if oidc := cfg.Auth.OIDC; oidc != nil && oidc.Browser != nil && len(oidc.Browser.Scopes) == 0 {
		oidc.Browser.Scopes = browserDefaultScopes()
	}
	if oidc := cfg.Auth.OIDC; oidc != nil && oidc.CLI != nil {
		if oidc.CLI.ClientID == "" {
			oidc.CLI.ClientID = oidc.Audience
		}
		if len(oidc.CLI.Scopes) == 0 {
			oidc.CLI.Scopes = cliDefaultScopes()
		}
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
	if err := validateSelections(pprof.AllowedSelections); err != nil {
		return err
	}
	if msgs := validation.IsQualifiedName(cfg.Discovery.VersionLabel); len(msgs) > 0 {
		return fmt.Errorf("discovery.versionLabel %q: %s", cfg.Discovery.VersionLabel, strings.Join(msgs, "; "))
	}
	if cfg.Server.Listen == cfg.Server.OpsListen {
		return fmt.Errorf("server.opsListen must differ from server.listen (both %q)", cfg.Server.Listen)
	}
	if err := validateTLS(cfg.Server.TLS); err != nil {
		return err
	}
	for name, realm := range cfg.Realms {
		// A realm name is sealed into the session cookie, which is what holds
		// it to a DNS-1123 label and that label's 63-byte bound.
		if !isDNSLabel(name) {
			return fmt.Errorf("realms.%s: not a DNS-1123 label", name)
		}
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
	if err := validateAuth(cfg); err != nil {
		return err
	}
	if err := validateUI(cfg); err != nil {
		return err
	}

	return validatePGO(cfg)
}

// validateUI holds an enabled console to the browser flow under oidc:
// a console that cannot log a browser in serves nobody.
// It runs after validateAuth, which has already required auth.oidc under
// oidc, so the pointer is safe to read.
// The other two modes need nothing: basic authenticates the page's requests
// from the browser's own credential prompt, and disabled authenticates nothing.
func validateUI(cfg *Config) error {
	if cfg.UI.Enabled && cfg.Auth.Mode == ModeOIDC && cfg.Auth.OIDC.Browser == nil {
		return errors.New("ui.enabled requires auth.oidc.browser when auth.mode is oidc")
	}

	return nil
}

// validatePGO runs the rules that relate a PGO key to another key.
// The rules that judge the pgo block against itself run whether or not
// pgo.enabled, so a block that contradicts itself fails at startup rather than
// on the day collection is turned on.
// One rule waits for pgo.enabled because it measures a PGO ceiling against an interactive limit,
// which a gateway that never collects is free to set low:
// limits.cpuSeconds may sit under the shipped maxDuration.
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

	if cpu := time.Duration(cfg.Limits.CPUSeconds) * time.Second; limits.MaxDuration > cpu {
		return fmt.Errorf("pgo.limits.maxDuration %v must be at most limits.cpuSeconds %v", limits.MaxDuration, cpu)
	}
	return nil
}

// validateTLS holds the API listener's certificate to being whole:
// both files or neither, and both readable.
// Opening them here is what turns a path typo into a startup failure that
// names the key, rather than a listener that answers every handshake with an
// error only the client can see.
// The pair itself is parsed by internal/tlscert, which serves it.
func validateTLS(tls TLSConfig) error {
	switch {
	case tls.CertFile == "" && tls.KeyFile == "":
		return nil
	case tls.CertFile == "":
		return errors.New("server.tls.certFile is required when server.tls.keyFile is set")
	case tls.KeyFile == "":
		return errors.New("server.tls.keyFile is required when server.tls.certFile is set")
	}
	for _, file := range []struct{ key, path string }{
		{"server.tls.certFile", tls.CertFile},
		{"server.tls.keyFile", tls.KeyFile},
	} {
		f, err := os.Open(file.path) //nolint:gosec // the operator names the file; reading it is the purpose
		if err != nil {
			return fmt.Errorf("%s: %w", file.key, err)
		}
		_ = f.Close()
	}

	return nil
}

// validateAuth runs the authentication rules that relate one key to another.
// It runs after the realm loop, so every rule that names a realm can look it up.
// The mode decides which block must be present and which must be absent,
// so a block that does not apply cannot be mistaken for one that does.
func validateAuth(cfg *Config) error {
	auth := &cfg.Auth
	if auth.Mode == ModeDisabled {
		if auth.AnonymousRealm == "" {
			return errors.New("auth.anonymousRealm is required when auth.mode is disabled")
		}
	} else if auth.AnonymousRealm != "" {
		return fmt.Errorf("auth.anonymousRealm %q must not be set when auth.mode is %q", auth.AnonymousRealm, auth.Mode)
	}
	if auth.AnonymousRealm != "" {
		if _, ok := cfg.Realms[auth.AnonymousRealm]; !ok {
			return fmt.Errorf("auth.anonymousRealm %q is not a realm", auth.AnonymousRealm)
		}
	}
	if auth.Basic != nil && auth.Mode != ModeBasic {
		return fmt.Errorf("auth.basic must not be set when auth.mode is %q", auth.Mode)
	}
	if auth.OIDC != nil && auth.Mode != ModeOIDC {
		return fmt.Errorf("auth.oidc must not be set when auth.mode is %q", auth.Mode)
	}

	switch auth.Mode {
	case ModeBasic:
		if auth.Basic == nil {
			return errors.New("auth.basic is required when auth.mode is basic")
		}

		return validateBasic(cfg)
	case ModeOIDC:
		if auth.OIDC == nil {
			return errors.New("auth.oidc is required when auth.mode is oidc")
		}

		return validateOIDC(cfg)
	}

	return nil
}

// validateBasic checks the user set as one list and then the transport it
// travels over, because Basic sends the password on every request.
func validateBasic(cfg *Config) error {
	basic := cfg.Auth.Basic
	var file []BasicUser
	if basic.UsersFile != "" {
		users, err := LoadUsersFile(basic.UsersFile)
		if err != nil {
			return fmt.Errorf("auth.basic.usersFile: %w", err)
		}
		file = users
	}
	if _, err := ValidateBasicUsers(basic.Users, file, cfg.Realms); err != nil {
		return err
	}
	if !cfg.Server.TLS.Enabled() && !basic.AllowPlaintext {
		return errors.New("auth.basic.allowPlaintext must be true to run basic authentication without server.tls")
	}

	return nil
}

// validateOIDC checks the issuer the gateway will trust, the transport it
// reaches that issuer over, and the mapping that turns a token into a realm.
// The files are opened here so a path typo names its key at startup rather
// than failing every discovery fetch once the process is running.
func validateOIDC(cfg *Config) error {
	oidc := cfg.Auth.OIDC
	if oidc.Issuer == "" {
		return errors.New("auth.oidc.issuer is required when auth.mode is oidc")
	}
	if err := validateHTTPSURL("auth.oidc.issuer", oidc.Issuer); err != nil {
		return err
	}
	if n := len(oidc.Audience); n < 1 || n > maxAudienceBytes {
		return fmt.Errorf("auth.oidc.audience: 1 to %d bytes, found %d", maxAudienceBytes, n)
	}
	if oidc.CAFile != "" {
		pem, err := os.ReadFile(oidc.CAFile) //nolint:gosec // the operator names the file; reading it is the purpose
		if err != nil {
			return fmt.Errorf("auth.oidc.caFile: %w", err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return fmt.Errorf("auth.oidc.caFile %q: holds no certificate", oidc.CAFile)
		}
	}
	if oidc.HTTPProxy != "" {
		proxy, err := url.Parse(oidc.HTTPProxy)
		if err != nil {
			return fmt.Errorf("auth.oidc.httpProxy %q: %w", oidc.HTTPProxy, err)
		}
		if proxy.Host == "" || !slices.Contains(proxySchemeNames(), proxy.Scheme) {
			return fmt.Errorf("auth.oidc.httpProxy %q: scheme must be one of %s and a host is required",
				oidc.HTTPProxy, strings.Join(proxySchemeNames(), ", "))
		}
	}
	// A set trusted for less time than the gateway waits to refresh it would
	// go stale on every cycle.
	if oidc.JWKSMaxStale < oidc.JWKSRefresh {
		return fmt.Errorf("auth.oidc.jwksMaxStale %v must be at least auth.oidc.jwksRefresh %v",
			oidc.JWKSMaxStale, oidc.JWKSRefresh)
	}
	if err := validateMapping(&oidc.Mapping, cfg.Realms); err != nil {
		return err
	}
	if oidc.CLI != nil {
		if err := validateCLI(oidc); err != nil {
			return err
		}
	}
	if oidc.Browser != nil {
		return validateBrowser(cfg)
	}

	return nil
}

// validateCLI checks the block that lets a command-line client log in by device code.
// An ID token's aud carries the client it was requested for
// and the gateway requires aud to contain the audience,
// so under ID tokens the client must be the audience itself:
// a second registration would produce a multi-valued aud with an azp the gateway refuses.
func validateCLI(oidc *OIDCConfig) error {
	cli := oidc.CLI
	if n := len(cli.ClientID); n < 1 || n > maxAudienceBytes {
		return fmt.Errorf("auth.oidc.cli.clientID: 1 to %d bytes, found %d", maxAudienceBytes, n)
	}
	if oidc.TokenType == "id" && cli.ClientID != oidc.Audience {
		return fmt.Errorf("auth.oidc.cli.clientID %q must equal auth.oidc.audience %q when auth.oidc.tokenType is id",
			cli.ClientID, oidc.Audience)
	}
	if err := validateScopes("auth.oidc.cli.scopes", cli.Scopes); err != nil {
		return err
	}
	// Under ID tokens the registration is shared with the browser flow, and a
	// registration holding a secret is confidential, which a device grant sent without a secret cannot use.
	if oidc.TokenType == "id" && oidc.Browser != nil && oidc.Browser.ClientSecretFile != "" {
		return errors.New("auth.oidc.cli requires a public client: unset auth.oidc.browser.clientSecretFile or leave auth.oidc.cli out")
	}

	return nil
}

// validateMapping checks the lists that turn a verified token into a realm.
// A mapping that names no user, no group, and no default admits nobody,
// which is a configuration nobody meant to write.
func validateMapping(mapping *OIDCMapping, realms map[string]Realm) error {
	if len(mapping.Users) == 0 && len(mapping.Groups) == 0 && mapping.DefaultRealm == "" {
		return errors.New("auth.oidc.mapping: at least one of users, groups, or defaultRealm is required; oidc mode would otherwise admit nobody")
	}
	for _, list := range []struct {
		key     string
		entries []OIDCMappingEntry
	}{
		{"auth.oidc.mapping.users", mapping.Users},
		{"auth.oidc.mapping.groups", mapping.Groups},
	} {
		names := make([]string, 0, len(list.entries))
		for i, entry := range list.entries {
			key := fmt.Sprintf("%s[%d]", list.key, i)
			if n := len(entry.Name); n < 1 || n > maxMappingNameBytes {
				return fmt.Errorf("%s.name: 1 to %d bytes, found %d", key, maxMappingNameBytes, n)
			}
			names = append(names, entry.Name)
			if entry.Realm == "" {
				return fmt.Errorf("%s.realm is required", key)
			}
			if _, ok := realms[entry.Realm]; !ok {
				return fmt.Errorf("%s.realm %q is not a realm", key, entry.Realm)
			}
		}
		if name, ok := firstDuplicate(names); ok {
			return fmt.Errorf("%s: duplicate entry %q", list.key, name)
		}
	}
	if mapping.DefaultRealm != "" {
		if _, ok := realms[mapping.DefaultRealm]; !ok {
			return fmt.Errorf("auth.oidc.mapping.defaultRealm %q is not a realm", mapping.DefaultRealm)
		}
	}

	return nil
}

// validateBrowser checks the relying-party block.
// The session cookie carries Secure and a __Host- prefix, which a plaintext
// listener cannot set, so this block requires server.tls with no escape hatch.
func validateBrowser(cfg *Config) error {
	oidc := cfg.Auth.OIDC
	browser := oidc.Browser
	if browser.ClientID == "" {
		return errors.New("auth.oidc.browser.clientID is required")
	}
	// The ID token the flow receives carries the client ID in aud and is
	// verified against audience; two values would validate and admit nobody.
	if browser.ClientID != oidc.Audience {
		return fmt.Errorf("auth.oidc.browser.clientID %q must equal auth.oidc.audience %q", browser.ClientID, oidc.Audience)
	}
	if oidc.TokenType != "id" {
		return fmt.Errorf("auth.oidc.tokenType must be id when auth.oidc.browser is set, found %q", oidc.TokenType)
	}
	if !cfg.Server.TLS.Enabled() {
		return errors.New("server.tls is required when auth.oidc.browser is set")
	}
	if browser.RedirectURL == "" {
		return errors.New("auth.oidc.browser.redirectURL is required")
	}
	if err := validateHTTPSURL("auth.oidc.browser.redirectURL", browser.RedirectURL); err != nil {
		return err
	}
	redirect, _ := url.Parse(browser.RedirectURL)
	if redirect.RawQuery != "" || redirect.ForceQuery {
		return fmt.Errorf("auth.oidc.browser.redirectURL %q: must carry no query", browser.RedirectURL)
	}
	if redirect.Path != callbackPath {
		return fmt.Errorf("auth.oidc.browser.redirectURL %q: path must be %s", browser.RedirectURL, callbackPath)
	}
	if err := validateScopes("auth.oidc.browser.scopes", browser.Scopes); err != nil {
		return err
	}
	if browser.CookieKeyFile == "" {
		return errors.New("auth.oidc.browser.cookieKeyFile is required")
	}
	f, err := os.Open(browser.CookieKeyFile) //nolint:gosec // the operator names the file; reading it is the purpose
	if err != nil {
		return fmt.Errorf("auth.oidc.browser.cookieKeyFile: %w", err)
	}
	_ = f.Close()
	if browser.ClientSecretFile == "" {
		return nil
	}
	secret, err := os.ReadFile(browser.ClientSecretFile) //nolint:gosec // the operator names the file; reading it is the purpose
	if err != nil {
		return fmt.Errorf("auth.oidc.browser.clientSecretFile: %w", err)
	}
	if n := len(strings.TrimSpace(string(secret))); n < 1 || n > maxClientSecretBytes {
		return fmt.Errorf("auth.oidc.browser.clientSecretFile %q: the trimmed contents must be 1 to %d bytes, found %d",
			browser.ClientSecretFile, maxClientSecretBytes, n)
	}

	return nil
}

// validateHTTPSURL holds a URL the gateway will send a browser or a client
// secret to: https, a host, no userinfo, and no fragment.
func validateHTTPSURL(key, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q: %w", key, raw, err)
	}
	switch {
	case parsed.Scheme != "https":
		return fmt.Errorf("%s %q: must be an https:// URL", key, raw)
	case parsed.Host == "":
		return fmt.Errorf("%s %q: must name a host", key, raw)
	case parsed.User != nil:
		return fmt.Errorf("%s %q: must carry no userinfo", key, raw)
	case parsed.Fragment != "":
		return fmt.Errorf("%s %q: must carry no fragment", key, raw)
	}

	return nil
}

// validateScopes checks an authorization request's scope list; key is the configuration key an error names.
// Without openid the issuer returns no ID token, and both the browser flow
// and the command-line client mint what they hold from the ID token.
func validateScopes(key string, scopes []string) error {
	if !slices.Contains(scopes, openidScope) {
		return fmt.Errorf("%s %v: must contain %q", key, scopes, openidScope)
	}
	for _, scope := range scopes {
		if n := len(scope); n < 1 || n > maxScopeBytes {
			return fmt.Errorf("%s %q: 1 to %d bytes, found %d", key, scope, maxScopeBytes, n)
		}
		for i := range len(scope) {
			if !isScopeByte(scope[i]) {
				return fmt.Errorf("%s %q: %q is not a scope character", key, scope, scope[i])
			}
		}
	}
	if scope, ok := firstDuplicate(scopes); ok {
		return fmt.Errorf("%s: duplicate entry %q", key, scope)
	}

	return nil
}

// isScopeByte reports whether b is one of the characters RFC 6749 allows in a
// scope token: printable ASCII without the space, the double quote, and the
// backslash.
func isScopeByte(b byte) bool {
	return b == 0x21 || (b >= 0x23 && b <= 0x5B) || (b >= 0x5D && b <= 0x7E)
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
	// A retention shorter than the interval that produces the artifact leaves
	// the Service with no downloadable profile for the tail of every interval.
	if defaults.Artifact.Retention < defaults.Schedule.Every {
		return fmt.Errorf("pgo.defaults.artifact.retention %v must be at least pgo.defaults.schedule.every %v",
			defaults.Artifact.Retention, defaults.Schedule.Every)
	}
	return nil
}

// validateSelections holds the list rules, which judge the list the environment
// may have replaced: no duplicate entry, and no wildcard beside a concrete
// entry of its own kind, which the wildcard has already decided.
func validateSelections(list []Selection) error {
	if sel, ok := firstDuplicate(list); ok {
		return fmt.Errorf("discovery.pprof.allowedSelections: duplicate entry %s", sel)
	}
	for _, kind := range []SelectionKind{SelectionPort, SelectionPortName} {
		var wildcard, concrete *Selection
		for i := range list {
			sel := &list[i]
			switch {
			case sel.Kind != kind:
			case sel.Value == AnySelection:
				wildcard = sel
			case concrete == nil:
				concrete = sel
			}
		}
		if wildcard != nil && concrete != nil {
			return fmt.Errorf("discovery.pprof.allowedSelections: %s beside %s; the wildcard already admits every %s", wildcard, concrete, kind)
		}
	}

	return nil
}

// firstDuplicate returns the first entry that already appears earlier in list.
// A repeated entry in an allowlist says the operator meant two different values
// and wrote one twice, so it is worth naming rather than merging silently.
// The scan is quadratic, which the allowlists' size makes the cheaper choice.
func firstDuplicate[T comparable](list []T) (T, bool) {
	for i, entry := range list {
		if slices.Contains(list[:i], entry) {
			return entry, true
		}
	}
	var zero T

	return zero, false
}

// isDNSLabel reports whether s is a DNS-1123 label, the rule for namespace and Service names.
func isDNSLabel(s string) bool {
	return len(validation.IsDNS1123Label(s)) == 0
}
