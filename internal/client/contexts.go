// Package client is the profgate command line's client of one gateway:
// context resolution, the transport rules, the token cache, the issuer client, and the verbs' request building.
// It is reachable only from cmd/profgate.
package client

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is the contexts file, read with KnownFields(true) so an unknown key
// at any level is an error naming the key.
type File struct {
	CurrentContext string             `yaml:"currentContext"`
	Contexts       map[string]Context `yaml:"contexts"`
}

// Context is one gateway and the authentication snapshot the last login recorded for it.
// Normal commands act on that snapshot and never read /v1/auth; only login refreshes it.
type Context struct {
	Server     string   `yaml:"server"`
	CAFile     string   `yaml:"caFile,omitempty"`
	ServerName string   `yaml:"serverName,omitempty"`
	Namespace  string   `yaml:"namespace,omitempty"`
	Auth       AuthSnap `yaml:"auth,omitempty"`
}

// AuthSnap is what GET /v1/auth reported at the last login.
// An empty Mode means no login has recorded a snapshot yet.
type AuthSnap struct {
	Mode         string   `yaml:"mode,omitempty"`
	Issuer       string   `yaml:"issuer,omitempty"`
	ClientID     string   `yaml:"clientID,omitempty"`
	TokenType    string   `yaml:"tokenType,omitempty"`
	Scopes       []string `yaml:"scopes,omitempty"`
	PKCE         bool     `yaml:"pkce,omitempty"`
	IssuerCAFile string   `yaml:"issuerCAFile,omitempty"`
}

// Overrides is what one command's flags said; an empty field said nothing.
type Overrides struct {
	Context, Server, CAFile, IssuerCAFile, ServerName, Namespace, Output string
}

// Settings is one command's resolved configuration: the context it selected,
// every value after the default-file-environment-flag order, the canonical
// origin of the server, and the token cache entry name bound to it.
type Settings struct {
	ContextName  string
	Context      Context
	Server       *url.URL
	Origin       string
	CacheName    string
	CAFile       string
	IssuerCAFile string
	ServerName   string
	Namespace    string
	Output       string
}

const (
	maxClientIDBytes = 256
	maxScopeBytes    = 64
	maxDNSNameBytes  = 253
	maxDNSLabelBytes = 63
)

// dnsLabel is a DNS-1123 label: lowercase alphanumerics and hyphens, starting and ending alphanumeric.
// Length is checked separately.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// dnsNameLabel is one label of an RFC 1123 host name, in either case.
var dnsNameLabel = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?$`)

// isDNSLabel reports whether s is a DNS-1123 label, the grammar of a
// context name and of a namespace.
func isDNSLabel(s string) bool {
	return len(s) <= maxDNSLabelBytes && dnsLabel.MatchString(s)
}

// isDNSName reports whether s is RFC 1123 labels joined by dots, with no
// trailing dot.
func isDNSName(s string) bool {
	if s == "" || len(s) > maxDNSNameBytes {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) > maxDNSLabelBytes || !dnsNameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// LoadFile reads and validates the contexts file at path.
// A missing file is (nil, nil): --server alone is a complete configuration.
func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user names the file; reading it is the purpose
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read contexts file: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateFile(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// SaveFile writes the contexts file through the atomic write.
func SaveFile(path string, f *File) error {
	return saveFile(path, f, atomicWrite)
}

// saveFile is SaveFile over an explicit write seam.
func saveFile(path string, f *File, write writeFunc) error {
	if err := validateFile(f); err != nil {
		return err
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode contexts file: %w", err)
	}
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := write(dir, name, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// RecordLogin makes the selected context's auth block the snapshot a login
// used and touches nothing else in it.
// A selected name with no entry gets one from the resolved server,
// certificate file, server name, and namespace, which is the first-run shape
// of profgate login --context <name> --server <url>.
func (f *File) RecordLogin(s Settings, snap AuthSnap) {
	if f.Contexts == nil {
		f.Contexts = map[string]Context{}
	}
	c, ok := f.Contexts[s.ContextName]
	if !ok {
		c = Context{Server: s.Server.String(), CAFile: s.CAFile, ServerName: s.ServerName, Namespace: s.Namespace}
	}
	c.Auth = snap
	f.Contexts[s.ContextName] = c
}

// validateFile checks every key of the file, not only the ones a command
// happens to read, so a hand-written file is refused at load.
func validateFile(f *File) error {
	for name, c := range f.Contexts {
		if !isDNSLabel(name) {
			return fmt.Errorf("contexts: name %q is not a DNS-1123 label", name)
		}
		if err := validateContext(c); err != nil {
			return fmt.Errorf("contexts.%s.%w", name, err)
		}
	}
	if f.CurrentContext != "" {
		if _, ok := f.Contexts[f.CurrentContext]; !ok {
			return fmt.Errorf("currentContext: %q names no context", f.CurrentContext)
		}
	}
	return nil
}

// validateContext checks one context; its errors start with the key so the
// caller can prefix the context name.
func validateContext(c Context) error {
	if _, err := parseServer(c.Server); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if c.CAFile != "" {
		if err := checkCertFile(c.CAFile); err != nil {
			return fmt.Errorf("caFile: %w", err)
		}
	}
	if c.ServerName != "" && !isDNSName(c.ServerName) {
		return fmt.Errorf("serverName: %q is not a DNS name", c.ServerName)
	}
	if c.Namespace != "" && !isDNSLabel(c.Namespace) {
		return fmt.Errorf("namespace: %q is not a DNS-1123 label", c.Namespace)
	}
	if err := validateAuth(c.Auth); err != nil {
		return fmt.Errorf("auth.%w", err)
	}
	return nil
}

func validateAuth(a AuthSnap) error {
	switch a.Mode {
	case "", "disabled", "basic":
	case "oidc":
		if err := validateOIDC(a); err != nil {
			return err
		}
	default:
		return fmt.Errorf("mode: %q is not one of disabled, basic, oidc", a.Mode)
	}
	if a.IssuerCAFile != "" {
		if err := checkCertFile(a.IssuerCAFile); err != nil {
			return fmt.Errorf("issuerCAFile: %w", err)
		}
	}
	return nil
}

func validateOIDC(a AuthSnap) error {
	if a.Issuer == "" {
		return errors.New("issuer: required under mode oidc")
	}
	if u, err := url.Parse(a.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("issuer: %q is not an https:// URL", a.Issuer)
	}
	if n := len(a.ClientID); n < 1 || n > maxClientIDBytes {
		return fmt.Errorf("clientID: 1 to %d bytes, found %d", maxClientIDBytes, n)
	}
	if a.TokenType != "id" && a.TokenType != "access" {
		return fmt.Errorf("tokenType: %q is not one of id, access", a.TokenType)
	}
	if !slices.Contains(a.Scopes, "openid") {
		return errors.New("scopes: must contain openid")
	}
	for _, s := range a.Scopes {
		if n := len(s); n < 1 || n > maxScopeBytes {
			return fmt.Errorf("scopes: entry %q is not 1 to %d bytes", s, maxScopeBytes)
		}
	}
	return nil
}

// checkCertFile reads path and requires at least one CERTIFICATE block,
// the same check the transport performs on --ca-file.
func checkCertFile(path string) error {
	pem, err := os.ReadFile(path) //nolint:gosec // the user names the file; reading it is the purpose
	if err != nil {
		return err
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return fmt.Errorf("%s: holds no CERTIFICATE block", path)
	}
	return nil
}

// parseServer applies the server rule: absolute http:// or https:// with a
// host, and no userinfo, query, or fragment.
func parseServer(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", s, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an absolute https:// or http:// URL", s)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%q carries userinfo", s)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, fmt.Errorf("%q carries a query", s)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, fmt.Errorf("%q carries a fragment", s)
	}
	return u, nil
}

// Resolve selects the context and applies the overriding order in one pass.
// getenv is the environment seam; a nil file means no context file exists.
func Resolve(f *File, o Overrides, getenv func(string) (string, bool)) (Settings, error) {
	lookup := func(name string) string {
		v, _ := getenv(name)
		return v
	}
	pick := func(file, variable, flag string) string {
		v := file
		if e := lookup(variable); e != "" {
			v = e
		}
		if flag != "" {
			v = flag
		}
		return v
	}

	var s Settings
	s.ContextName = o.Context
	if s.ContextName == "" {
		s.ContextName = lookup("PROFGATE_CONTEXT")
	}
	if s.ContextName == "" && f != nil {
		s.ContextName = f.CurrentContext
	}
	if s.ContextName != "" {
		if !isDNSLabel(s.ContextName) {
			return Settings{}, fmt.Errorf("context %q is not a DNS-1123 label", s.ContextName)
		}
		ok := false
		if f != nil {
			s.Context, ok = f.Contexts[s.ContextName]
		}
		if !ok {
			return Settings{}, fmt.Errorf("context %q is not in the contexts file", s.ContextName)
		}
	}

	server := pick(s.Context.Server, "PROFGATE_SERVER", o.Server)
	if server == "" {
		return Settings{}, errors.New("no gateway selected: pass --server, or select a context with profgate context use")
	}
	u, err := parseServer(server)
	if err != nil {
		return Settings{}, fmt.Errorf("server: %w", err)
	}
	s.Server = u
	s.Origin = CanonicalOrigin(u)
	s.CacheName = s.ContextName
	if s.CacheName == "" {
		sum := sha256.Sum256([]byte(s.Origin))
		s.CacheName = "adhoc-" + hex.EncodeToString(sum[:])[:32]
	}

	s.CAFile = pick(s.Context.CAFile, "PROFGATE_CA_FILE", o.CAFile)
	s.IssuerCAFile = pick(s.Context.Auth.IssuerCAFile, "PROFGATE_ISSUER_CA_FILE", o.IssuerCAFile)
	s.ServerName = pick(s.Context.ServerName, "PROFGATE_SERVER_NAME", o.ServerName)
	if s.ServerName != "" && !isDNSName(s.ServerName) {
		return Settings{}, fmt.Errorf("server name %q is not a DNS name", s.ServerName)
	}
	s.Namespace = pick(s.Context.Namespace, "PROFGATE_NAMESPACE", o.Namespace)
	if s.Namespace != "" && !isDNSLabel(s.Namespace) {
		return Settings{}, fmt.Errorf("namespace %q is not a DNS-1123 label", s.Namespace)
	}
	s.Output = pick("table", "PROFGATE_OUTPUT", o.Output)
	if s.Output != "table" && s.Output != "json" {
		return Settings{}, fmt.Errorf("output %q is not one of table, json", s.Output)
	}
	return s, nil
}

// ConfigPath is $XDG_CONFIG_HOME/profgate/config.yaml, falling back to
// $HOME/.config when the variable is unset or not absolute.
func ConfigPath(getenv func(string) (string, bool)) (string, error) {
	base, err := xdgBase(getenv, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "profgate", "config.yaml"), nil
}

// StatePath is $XDG_STATE_HOME/profgate/tokens, falling back to
// $HOME/.local/state when the variable is unset or not absolute.
func StatePath(getenv func(string) (string, bool)) (string, error) {
	base, err := xdgBase(getenv, "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "profgate", "tokens"), nil
}

func xdgBase(getenv func(string) (string, bool), variable, fallback string) (string, error) {
	if v, ok := getenv(variable); ok && filepath.IsAbs(v) {
		return v, nil
	}
	home, ok := getenv("HOME")
	if !ok || home == "" {
		return "", fmt.Errorf("%s is unset or not absolute and HOME is unset", variable)
	}
	return filepath.Join(home, fallback), nil
}

// CanonicalOrigin is the origin a credential is bound to: scheme and host
// lowercased, an IPv6 host in brackets, the port always explicit, and no
// path, query, fragment, or userinfo.
func CanonicalOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if scheme == "http" {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.Trim(host, "[]"), port)
}
