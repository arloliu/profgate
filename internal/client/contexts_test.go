package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	certOnce sync.Once
	certPEM  []byte
)

// certFile writes a self-signed certificate under dir and returns its path.
// The key pair is generated once per test binary.
func certFile(t *testing.T, dir string) string {
	t.Helper()
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "test"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	})
	path := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeYAML writes body as the contexts file under dir and returns its path.
func writeYAML(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// env builds the environment seam from a map.
func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadFileWellFormed(t *testing.T) {
	dir := t.TempDir()
	ca := certFile(t, dir)
	issuerCA := filepath.Join(dir, "issuer.crt")
	if err := os.WriteFile(issuerCA, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeYAML(t, dir, `
currentContext: prod
contexts:
  prod:
    server: https://profgate.example
    caFile: `+ca+`
    serverName: profgate.example
    namespace: payments
    auth:
      mode: oidc
      issuer: https://keycloak.example/realms/engineering
      clientID: profgate
      tokenType: id
      scopes: [openid, offline_access]
      pkce: true
      issuerCAFile: `+issuerCA+`
`)
	f, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if f.CurrentContext != "prod" {
		t.Fatalf("currentContext = %q", f.CurrentContext)
	}
	c := f.Contexts["prod"]
	want := Context{
		Server: "https://profgate.example", CAFile: ca, ServerName: "profgate.example", Namespace: "payments",
		Auth: AuthSnap{
			Mode: "oidc", Issuer: "https://keycloak.example/realms/engineering", ClientID: "profgate",
			TokenType: "id", Scopes: []string{"openid", "offline_access"}, PKCE: true, IssuerCAFile: issuerCA,
		},
	}
	if c.Server != want.Server || c.CAFile != want.CAFile || c.ServerName != want.ServerName ||
		c.Namespace != want.Namespace || c.Auth.Mode != want.Auth.Mode || c.Auth.Issuer != want.Auth.Issuer ||
		c.Auth.ClientID != want.Auth.ClientID || c.Auth.TokenType != want.Auth.TokenType ||
		!slices.Equal(c.Auth.Scopes, want.Auth.Scopes) || c.Auth.PKCE != want.Auth.PKCE ||
		c.Auth.IssuerCAFile != want.Auth.IssuerCAFile {
		t.Fatalf("context = %+v, want %+v", c, want)
	}
}

func TestLoadFileMissing(t *testing.T) {
	f, err := LoadFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || f != nil {
		t.Fatalf("LoadFile(missing) = %v, %v; want nil, nil", f, err)
	}
}

func TestLoadFileRefusals(t *testing.T) {
	dir := t.TempDir()
	ca := certFile(t, dir)
	missing := filepath.Join(dir, "missing.crt")
	unreadable := filepath.Join(dir, "unreadable.crt")
	if err := os.WriteFile(unreadable, certPEM, 0o000); err != nil {
		t.Fatal(err)
	}
	noCert := filepath.Join(dir, "nocert.crt")
	if err := os.WriteFile(noCert, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads files of any mode")
	}

	oidc := func(extra string) string {
		return `
contexts:
  prod:
    server: https://a.example
    auth:
      mode: oidc
      issuer: https://issuer.example
      clientID: profgate
      tokenType: id
      scopes: [openid]
      pkce: false
` + extra
	}
	// oidcWith replaces one auth key of the well-formed oidc block.
	oidcWith := func(key, value string) string {
		lines := strings.Split(oidc(""), "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), key+":") {
				lines[i] = "      " + key + ": " + value
			}
		}
		return strings.Join(lines, "\n")
	}
	oidcWithout := func(key string) string {
		lines := strings.Split(oidc(""), "\n")
		return strings.Join(slices.DeleteFunc(lines, func(l string) bool {
			return strings.HasPrefix(strings.TrimSpace(l), key+":")
		}), "\n")
	}
	ctx := func(body string) string {
		return "contexts:\n  prod:\n    server: https://a.example\n" + body
	}

	cases := []struct {
		name string
		body string
		want []string
	}{
		{"unknown key at the top level", "contexts: {}\nbogus: 1\n", []string{"bogus"}},
		{"unknown key inside a context", ctx("    bogus: 1\n"), []string{"bogus"}},
		{"unknown key inside auth", ctx("    auth:\n      mode: disabled\n      bogus: 1\n"), []string{"bogus"}},
		{"currentContext naming no entry", "currentContext: staging\n" + ctx(""), []string{"currentContext", "staging"}},
		{"name with a path", "contexts:\n  ../../evil:\n    server: https://a.example\n", []string{"../../evil", "DNS-1123"}},
		{"name that is an absolute path", "contexts:\n  /etc/passwd:\n    server: https://a.example\n", []string{"/etc/passwd", "DNS-1123"}},
		{"name with an upper-case letter", "contexts:\n  Prod:\n    server: https://a.example\n", []string{"Prod", "DNS-1123"}},
		{"empty name", "contexts:\n  \"\":\n    server: https://a.example\n", []string{"DNS-1123"}},
		{"server not absolute", "contexts:\n  prod:\n    server: a.example\n", []string{"contexts.prod.server", "absolute"}},
		{"server with userinfo", "contexts:\n  prod:\n    server: https://u:p@a.example\n", []string{"contexts.prod.server", "userinfo"}},
		{"server with a query", "contexts:\n  prod:\n    server: https://a.example/?x=1\n", []string{"contexts.prod.server", "query"}},
		{"server with a fragment", "contexts:\n  prod:\n    server: https://a.example/#frag\n", []string{"contexts.prod.server", "fragment"}},
		{"caFile missing", ctx("    caFile: " + missing + "\n"), []string{"contexts.prod.caFile", missing}},
		{"caFile unreadable", ctx("    caFile: " + unreadable + "\n"), []string{"contexts.prod.caFile", unreadable}},
		{"caFile without a certificate", ctx("    caFile: " + noCert + "\n"), []string{"contexts.prod.caFile", noCert, "CERTIFICATE"}},
		{"issuerCAFile missing", oidc("      issuerCAFile: " + missing + "\n"), []string{"contexts.prod.auth.issuerCAFile", missing}},
		{"issuerCAFile unreadable", oidc("      issuerCAFile: " + unreadable + "\n"), []string{"contexts.prod.auth.issuerCAFile", unreadable}},
		{"issuerCAFile without a certificate", oidc("      issuerCAFile: " + noCert + "\n"), []string{"contexts.prod.auth.issuerCAFile", noCert, "CERTIFICATE"}},
		{"serverName with an underscore", ctx("    serverName: a_b\n"), []string{"contexts.prod.serverName"}},
		{"serverName with a trailing dot", ctx("    serverName: a.example.\n"), []string{"contexts.prod.serverName"}},
		{"serverName with a 64-byte label", ctx("    serverName: " + strings.Repeat("a", 64) + ".example\n"), []string{"contexts.prod.serverName"}},
		{"namespace not a label", ctx("    namespace: Payments\n"), []string{"contexts.prod.namespace"}},
		{"auth.mode unknown", ctx("    auth:\n      mode: token\n"), []string{"contexts.prod.auth.mode"}},
		{"auth.issuer absent under oidc", oidcWithout("issuer"), []string{"contexts.prod.auth.issuer"}},
		{"auth.issuer not https", oidcWith("issuer", "http://issuer.example"), []string{"contexts.prod.auth.issuer", "https://"}},
		{"auth.clientID empty", oidcWith("clientID", `""`), []string{"contexts.prod.auth.clientID", "1 to 256"}},
		{"auth.clientID of 257 bytes", oidcWith("clientID", strings.Repeat("c", 257)), []string{"contexts.prod.auth.clientID", "1 to 256"}},
		{"auth.tokenType unknown", oidcWith("tokenType", "refresh"), []string{"contexts.prod.auth.tokenType"}},
		{"auth.scopes without openid", oidcWith("scopes", "[profile]"), []string{"contexts.prod.auth.scopes", "openid"}},
		{"auth.scopes with a 65-byte entry", oidcWith("scopes", "[openid, "+strings.Repeat("s", 65)+"]"), []string{"contexts.prod.auth.scopes"}},
		{"auth.scopes with an empty entry", oidcWith("scopes", `[openid, ""]`), []string{"contexts.prod.auth.scopes"}},
		{"auth.pkce not a boolean", oidcWith("pkce", "maybe"), []string{"pkce", "bool"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, t.TempDir(), tc.body)
			_, err := LoadFile(path)
			if err == nil {
				t.Fatalf("LoadFile = nil error, want one naming %q", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error = %q, want it to contain %q", err, w)
				}
			}
		})
	}
	_ = ca
}

// The well-formed file every precedence row starts from.
func precedenceFile(t *testing.T) (*File, string, string) {
	t.Helper()
	dir := t.TempDir()
	ca := certFile(t, dir)
	issuerCA := filepath.Join(dir, "issuer.crt")
	if err := os.WriteFile(issuerCA, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &File{
		CurrentContext: "prod",
		Contexts: map[string]Context{
			"prod": {
				Server: "https://prod.example", CAFile: ca, ServerName: "prod.example", Namespace: "payments",
				Auth: AuthSnap{Mode: "oidc", Issuer: "https://issuer.example", ClientID: "profgate", TokenType: "id",
					Scopes: []string{"openid"}, IssuerCAFile: issuerCA},
			},
			"staging": {Server: "https://staging.example", Namespace: "staging-ns"},
		},
	}
	return f, ca, issuerCA
}

func TestResolvePrecedence(t *testing.T) {
	f, ca, issuerCA := precedenceFile(t)
	// Each field: its file value, its environment variable and value, its flag value, and a reader of the result.
	fields := []struct {
		name     string
		variable string
		file     string
		fromEnv  string
		fromFlag string
		flag     func(*Overrides, string)
		read     func(Settings) string
	}{
		{"server", "PROFGATE_SERVER", "https://prod.example", "https://env.example", "https://flag.example",
			func(o *Overrides, v string) { o.Server = v }, func(s Settings) string { return s.Server.String() }},
		{"caFile", "PROFGATE_CA_FILE", ca, "/env/ca.crt", "/flag/ca.crt",
			func(o *Overrides, v string) { o.CAFile = v }, func(s Settings) string { return s.CAFile }},
		{"issuerCAFile", "PROFGATE_ISSUER_CA_FILE", issuerCA, "/env/issuer.crt", "/flag/issuer.crt",
			func(o *Overrides, v string) { o.IssuerCAFile = v }, func(s Settings) string { return s.IssuerCAFile }},
		{"serverName", "PROFGATE_SERVER_NAME", "prod.example", "env.example", "flag.example",
			func(o *Overrides, v string) { o.ServerName = v }, func(s Settings) string { return s.ServerName }},
		{"namespace", "PROFGATE_NAMESPACE", "payments", "env-ns", "flag-ns",
			func(o *Overrides, v string) { o.Namespace = v }, func(s Settings) string { return s.Namespace }},
		{"output", "PROFGATE_OUTPUT", "table", "json", "table",
			func(o *Overrides, v string) { o.Output = v }, func(s Settings) string { return s.Output }},
	}
	for _, fd := range fields {
		t.Run(fd.name, func(t *testing.T) {
			t.Run("the context file alone", func(t *testing.T) {
				s, err := Resolve(f, Overrides{}, env(nil))
				if err != nil {
					t.Fatal(err)
				}
				if got := fd.read(s); got != fd.file {
					t.Fatalf("%s = %q, want the file's %q", fd.name, got, fd.file)
				}
			})
			t.Run("the file and the environment", func(t *testing.T) {
				s, err := Resolve(f, Overrides{}, env(map[string]string{fd.variable: fd.fromEnv}))
				if err != nil {
					t.Fatal(err)
				}
				if got := fd.read(s); got != fd.fromEnv {
					t.Fatalf("%s = %q, want the environment's %q", fd.name, got, fd.fromEnv)
				}
			})
			t.Run("the file, the environment, and a flag", func(t *testing.T) {
				var o Overrides
				fd.flag(&o, fd.fromFlag)
				s, err := Resolve(f, o, env(map[string]string{fd.variable: fd.fromEnv}))
				if err != nil {
					t.Fatal(err)
				}
				if got := fd.read(s); got != fd.fromFlag {
					t.Fatalf("%s = %q, want the flag's %q", fd.name, got, fd.fromFlag)
				}
			})
		})
	}
}

func TestResolveSelection(t *testing.T) {
	f, _, _ := precedenceFile(t)

	t.Run("PROFGATE_CONTEXT alone selects", func(t *testing.T) {
		s, err := Resolve(f, Overrides{}, env(map[string]string{"PROFGATE_CONTEXT": "staging"}))
		if err != nil {
			t.Fatal(err)
		}
		if s.ContextName != "staging" || s.Server.Host != "staging.example" || s.CacheName != "staging" {
			t.Fatalf("settings = %+v, want staging", s)
		}
	})
	t.Run("--context beats PROFGATE_CONTEXT", func(t *testing.T) {
		s, err := Resolve(f, Overrides{Context: "prod"}, env(map[string]string{"PROFGATE_CONTEXT": "staging"}))
		if err != nil {
			t.Fatal(err)
		}
		if s.ContextName != "prod" || s.Server.Host != "prod.example" {
			t.Fatalf("settings = %+v, want prod", s)
		}
	})
	t.Run("PROFGATE_NAMESPACE replaces the namespace and leaves the server alone", func(t *testing.T) {
		s, err := Resolve(f, Overrides{}, env(map[string]string{"PROFGATE_CONTEXT": "staging", "PROFGATE_NAMESPACE": "other"}))
		if err != nil {
			t.Fatal(err)
		}
		if s.Namespace != "other" || s.Server.String() != "https://staging.example" {
			t.Fatalf("settings = %+v, want namespace other on staging.example", s)
		}
	})
	t.Run("a context name is checked before any path is built", func(t *testing.T) {
		for _, name := range []string{"../../evil", "/etc/passwd", "Prod"} {
			_, err := Resolve(f, Overrides{Context: name}, env(nil))
			if err == nil || !strings.Contains(err.Error(), "DNS-1123") {
				t.Fatalf("Resolve(--context %q) error = %v, want the DNS-1123 rule", name, err)
			}
		}
	})
	t.Run("a context the file does not hold", func(t *testing.T) {
		_, err := Resolve(f, Overrides{Context: "absent"}, env(nil))
		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Fatalf("error = %v, want one naming absent", err)
		}
	})
	t.Run("a context the file does not hold, with --server, is the first-run shape", func(t *testing.T) {
		s, err := Resolve(f, Overrides{Context: "absent", Server: "https://a.example"}, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if s.ContextName != "absent" || s.CacheName != "absent" || s.Origin != "https://a.example:443" || s.Context.Server != "" {
			t.Fatalf("settings = %+v, want the name selected over an empty context and the flag's server", s)
		}
	})
	t.Run("a context the file does not hold, with PROFGATE_SERVER", func(t *testing.T) {
		s, err := Resolve(nil, Overrides{Context: "absent"}, env(map[string]string{"PROFGATE_SERVER": "https://a.example"}))
		if err != nil {
			t.Fatal(err)
		}
		if s.ContextName != "absent" || s.Origin != "https://a.example:443" {
			t.Fatalf("settings = %+v", s)
		}
	})
	t.Run("no context and no --server", func(t *testing.T) {
		_, err := Resolve(nil, Overrides{}, env(nil))
		if err == nil || !strings.Contains(err.Error(), "--server") || !strings.Contains(err.Error(), "profgate context use") {
			t.Fatalf("error = %v, want one naming --server and profgate context use", err)
		}
	})
	t.Run("--server and no context file", func(t *testing.T) {
		s, err := Resolve(nil, Overrides{Server: "https://a.example"}, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if s.ContextName != "" || s.Origin != "https://a.example:443" || s.Output != "table" {
			t.Fatalf("settings = %+v", s)
		}
		if !regexp.MustCompile(`^adhoc-[0-9a-f]{32}$`).MatchString(s.CacheName) {
			t.Fatalf("cache name = %q, want adhoc- and 32 hex characters", s.CacheName)
		}
	})
	t.Run("--server not absolute", func(t *testing.T) {
		_, err := Resolve(nil, Overrides{Server: "a.example"}, env(nil))
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("error = %v, want the absolute rule", err)
		}
	})
	for _, tc := range []struct {
		name string
		o    Overrides
		env  map[string]string
	}{
		{"--output outside table and json", Overrides{Server: "https://a.example", Output: "yaml"}, nil},
		{"PROFGATE_OUTPUT outside table and json", Overrides{Server: "https://a.example"}, map[string]string{"PROFGATE_OUTPUT": "yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(nil, tc.o, env(tc.env))
			if err == nil || !strings.Contains(err.Error(), "table") || !strings.Contains(err.Error(), "json") {
				t.Fatalf("error = %v, want one naming table and json", err)
			}
		})
	}
}

func TestXDGPaths(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		config     string
		state      string
		configErr  []string
		stateErr   []string
		wantErrors bool
	}{
		{name: "both variables absolute",
			env:    map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/cfg", "XDG_STATE_HOME": "/st"},
			config: "/cfg/profgate/config.yaml", state: "/st/profgate/tokens"},
		{name: "both unset",
			env:    map[string]string{"HOME": "/home/u"},
			config: "/home/u/.config/profgate/config.yaml", state: "/home/u/.local/state/profgate/tokens"},
		{name: "both relative",
			env:    map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "cfg", "XDG_STATE_HOME": "st"},
			config: "/home/u/.config/profgate/config.yaml", state: "/home/u/.local/state/profgate/tokens"},
		{name: "both empty",
			env:    map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "", "XDG_STATE_HOME": ""},
			config: "/home/u/.config/profgate/config.yaml", state: "/home/u/.local/state/profgate/tokens"},
		{name: "HOME unset and a variable unset",
			env:        map[string]string{},
			wantErrors: true,
			configErr:  []string{"HOME", "XDG_CONFIG_HOME"},
			stateErr:   []string{"HOME", "XDG_STATE_HOME"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, cerr := ConfigPath(env(tc.env))
			state, serr := StatePath(env(tc.env))
			if tc.wantErrors {
				for _, w := range tc.configErr {
					if cerr == nil || !strings.Contains(cerr.Error(), w) {
						t.Fatalf("ConfigPath error = %v, want one naming %q", cerr, w)
					}
				}
				for _, w := range tc.stateErr {
					if serr == nil || !strings.Contains(serr.Error(), w) {
						t.Fatalf("StatePath error = %v, want one naming %q", serr, w)
					}
				}
				return
			}
			if cerr != nil || serr != nil {
				t.Fatalf("errors: %v, %v", cerr, serr)
			}
			if config != tc.config || state != tc.state {
				t.Fatalf("paths = %q, %q; want %q, %q", config, state, tc.config, tc.state)
			}
		})
	}
}

func TestCanonicalOrigin(t *testing.T) {
	origin := func(t *testing.T, server string) string {
		t.Helper()
		u, err := url.Parse(server)
		if err != nil {
			t.Fatal(err)
		}
		return CanonicalOrigin(u)
	}
	cacheName := func(t *testing.T, server string) string {
		t.Helper()
		s, err := Resolve(nil, Overrides{Server: server}, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		return s.CacheName
	}

	if got := origin(t, "https://a.example"); got != "https://a.example:443" {
		t.Fatalf("origin = %q", got)
	}
	if got := origin(t, "https://a.example:443"); got != "https://a.example:443" {
		t.Fatalf("origin with an explicit 443 = %q", got)
	}
	if cacheName(t, "https://a.example") != cacheName(t, "https://a.example:443") {
		t.Fatal("https://a.example and https://a.example:443 name different cache files")
	}
	if got := origin(t, "http://a.example"); got != "http://a.example:80" {
		t.Fatalf("http origin = %q", got)
	}
	if cacheName(t, "http://a.example") == cacheName(t, "https://a.example") {
		t.Fatal("http:// and https:// share a cache file")
	}
	if got := origin(t, "https://A.Example:8443"); got != "https://a.example:8443" {
		t.Fatalf("mixed-case origin = %q", got)
	}
	if got := origin(t, "https://[2001:db8::1]:8443"); got != "https://[2001:db8::1]:8443" {
		t.Fatalf("IPv6 origin = %q", got)
	}
	if !regexp.MustCompile(`^adhoc-[0-9a-f]{32}$`).MatchString(cacheName(t, "https://[2001:db8::1]:8443")) {
		t.Fatal("an IPv6 authority has no ad-hoc cache name")
	}
	if cacheName(t, "https://a.example:8443") == cacheName(t, "https://a-example:8443") {
		t.Fatal("a.example:8443 and a-example:8443 share a cache file")
	}
	if origin(t, "https://a.example:8443") == origin(t, "https://a-example:8443") {
		t.Fatal("a.example:8443 and a-example:8443 share an origin")
	}
	if got := origin(t, "https://u:p@A.example:8443/path?q=1#f"); got != "https://a.example:8443" {
		t.Fatalf("origin keeps path, query, fragment, or userinfo: %q", got)
	}
}

func TestSaveFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profgate")
	path := filepath.Join(dir, "config.yaml")
	f := &File{CurrentContext: "prod", Contexts: map[string]Context{"prod": {Server: "https://a.example"}}}

	var seen []string
	spy := func(d, name string, data []byte, mode os.FileMode) error {
		seen = append(seen, d+"|"+name+"|"+mode.String())
		return atomicWrite(d, name, data, mode)
	}
	if err := saveFile(path, f, spy); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != dir+"|config.yaml|-rw-------" {
		t.Fatalf("write seam saw %v", seen)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, want 0700", info.Mode().Perm())
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.CurrentContext != "prod" || back.Contexts["prod"].Server != "https://a.example" {
		t.Fatalf("round trip = %+v", back)
	}
	if err := SaveFile(path, f); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
}

func TestUseContext(t *testing.T) {
	t.Run("sets currentContext to an existing name", func(t *testing.T) {
		f := &File{CurrentContext: "prod", Contexts: map[string]Context{"prod": {Server: "https://a.example"}, "dev": {Server: "https://b.example"}}}
		if err := UseContext(f, "dev"); err != nil {
			t.Fatal(err)
		}
		if f.CurrentContext != "dev" {
			t.Fatalf("currentContext = %q, want dev", f.CurrentContext)
		}
	})
	t.Run("a name with no entry is a usage error and changes nothing", func(t *testing.T) {
		f := &File{CurrentContext: "prod", Contexts: map[string]Context{"prod": {Server: "https://a.example"}}}
		err := UseContext(f, "staging")
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "staging") {
			t.Fatalf("err = %v, want a usage error naming staging", err)
		}
		if f.CurrentContext != "prod" {
			t.Fatalf("currentContext = %q, want prod untouched", f.CurrentContext)
		}
	})
	t.Run("a nil file is a usage error", func(t *testing.T) {
		if err := UseContext(nil, "prod"); !errors.Is(err, ErrUsage) {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})
}

// deleteFixture is one DeleteContext run:
// a two-context file with prod current, a Store over a fresh directory, and a save that records what it saw of the lock and the cache entry at the moment it ran.
type deleteFixture struct {
	f         *File
	store     *Store
	dir       string
	saves     int
	lockHeld  bool // the lock file existed when save ran
	entryGone bool // the cache entry was gone when save ran
	saveErr   error
}

func newDeleteFixture(t *testing.T) *deleteFixture {
	t.Helper()
	store, _, dir := testStore(t)
	return &deleteFixture{
		f:     &File{CurrentContext: "prod", Contexts: map[string]Context{"prod": {Server: "https://a.example"}, "dev": {Server: "https://b.example"}}},
		store: store,
		dir:   dir,
	}
}

func (d *deleteFixture) save(*File) error {
	d.saves++
	_, err := os.Stat(filepath.Join(d.dir, "prod.lock"))
	d.lockHeld = err == nil
	_, err = os.Stat(filepath.Join(d.dir, "prod.json"))
	d.entryGone = errors.Is(err, os.ErrNotExist)
	return d.saveErr
}

func (d *deleteFixture) writeEntry(t *testing.T) {
	t.Helper()
	if err := d.store.Write("prod", testEntry()); err != nil {
		t.Fatal(err)
	}
}

func (d *deleteFixture) entryExists(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(d.dir, "prod.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func (d *deleteFixture) lockExists(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(d.dir, "prod.lock"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestDeleteContext(t *testing.T) {
	ctx := context.Background()

	t.Run("takes the lock, deletes the entry, then saves, and releases", func(t *testing.T) {
		d := newDeleteFixture(t)
		d.writeEntry(t)
		cleared, err := DeleteContext(ctx, d.f, "prod", d.store, d.save)
		if err != nil {
			t.Fatal(err)
		}
		if !cleared {
			t.Fatal("cleared = false, want true: prod was the selected context")
		}
		if d.saves != 1 || !d.lockHeld || !d.entryGone {
			t.Fatalf("saves = %d, lock held at save = %v, entry gone at save = %v; want one save under the lock after the deletion", d.saves, d.lockHeld, d.entryGone)
		}
		if _, ok := d.f.Contexts["prod"]; ok {
			t.Fatal("prod is still in the file")
		}
		if _, ok := d.f.Contexts["dev"]; !ok {
			t.Fatal("dev was removed too")
		}
		if d.f.CurrentContext != "" {
			t.Fatalf("currentContext = %q, want empty: it named the deleted entry", d.f.CurrentContext)
		}
		if d.lockExists(t) {
			t.Fatal("the lock was not released")
		}
	})

	t.Run("leaves currentContext alone when it names another entry", func(t *testing.T) {
		d := newDeleteFixture(t)
		cleared, err := DeleteContext(ctx, d.f, "dev", d.store, d.save)
		if err != nil {
			t.Fatal(err)
		}
		if cleared {
			t.Fatal("cleared = true, want false: prod is still selected")
		}
		if d.f.CurrentContext != "prod" {
			t.Fatalf("currentContext = %q, want prod", d.f.CurrentContext)
		}
	})

	t.Run("succeeds with no cache entry", func(t *testing.T) {
		d := newDeleteFixture(t)
		if _, err := DeleteContext(ctx, d.f, "prod", d.store, d.save); err != nil {
			t.Fatal(err)
		}
		if d.saves != 1 {
			t.Fatalf("saves = %d, want 1", d.saves)
		}
	})

	t.Run("a lock held past its bound names the lock file and touches nothing", func(t *testing.T) {
		d := newDeleteFixture(t)
		d.writeEntry(t)
		lock := filepath.Join(d.dir, "prod.lock")
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := DeleteContext(ctx, d.f, "prod", d.store, d.save)
		if err == nil || !strings.Contains(err.Error(), lock) {
			t.Fatalf("err = %v, want one naming %s", err, lock)
		}
		if errors.Is(err, ErrUsage) {
			t.Fatalf("err = %v is a usage error; a held lock is exit 1", err)
		}
		if d.saves != 0 || !d.entryExists(t) {
			t.Fatalf("saves = %d, entry exists = %v; want nothing touched", d.saves, d.entryExists(t))
		}
		if _, ok := d.f.Contexts["prod"]; !ok {
			t.Fatal("prod was removed from the file")
		}
	})

	t.Run("a failed cache deletion names the cache file and leaves the file unwritten", func(t *testing.T) {
		d := newDeleteFixture(t)
		// A non-empty directory in the entry's place: it passes the mode check and cannot be removed.
		entry := filepath.Join(d.dir, "prod.json")
		if err := os.MkdirAll(filepath.Join(entry, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := DeleteContext(ctx, d.f, "prod", d.store, d.save)
		if err == nil || !strings.Contains(err.Error(), entry) {
			t.Fatalf("err = %v, want one naming %s", err, entry)
		}
		if d.saves != 0 {
			t.Fatalf("saves = %d, want 0: the context must still name the credential that still exists", d.saves)
		}
		if _, ok := d.f.Contexts["prod"]; !ok || d.f.CurrentContext != "prod" {
			t.Fatalf("file = %+v, want prod still present and current", d.f)
		}
		if d.lockExists(t) {
			t.Fatal("the lock was not released")
		}
	})

	t.Run("a failed save names the context file, the credential is gone, and a second run completes", func(t *testing.T) {
		d := newDeleteFixture(t)
		d.writeEntry(t)
		d.saveErr = errors.New("write /home/alice/.config/profgate/config.yaml: disk full")
		_, err := DeleteContext(ctx, d.f, "prod", d.store, d.save)
		if err == nil || !strings.Contains(err.Error(), "/home/alice/.config/profgate/config.yaml") {
			t.Fatalf("err = %v, want one naming the context file", err)
		}
		if d.entryExists(t) {
			t.Fatal("the cache entry survived the deletion")
		}
		if d.lockExists(t) {
			t.Fatal("the lock was not released")
		}
		d.saveErr = nil
		// The caller reloads the file before running again; the entry is still in it because the save failed.
		d.f = &File{CurrentContext: "prod", Contexts: map[string]Context{"prod": {Server: "https://a.example"}, "dev": {Server: "https://b.example"}}}
		if _, err := DeleteContext(ctx, d.f, "prod", d.store, d.save); err != nil {
			t.Fatalf("second run: %v", err)
		}
		if _, ok := d.f.Contexts["prod"]; ok || d.f.CurrentContext != "" {
			t.Fatalf("file after the second run = %+v", d.f)
		}
	})

	t.Run("a name with no entry is a usage error before the lock", func(t *testing.T) {
		d := newDeleteFixture(t)
		_, err := DeleteContext(ctx, d.f, "staging", d.store, d.save)
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "staging") {
			t.Fatalf("err = %v, want a usage error naming staging", err)
		}
		if d.saves != 0 {
			t.Fatalf("saves = %d, want 0", d.saves)
		}
		if _, err := os.Stat(d.dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the tokens directory was created for a name with no entry: %v", err)
		}
	})

	t.Run("a nil file is a usage error", func(t *testing.T) {
		d := newDeleteFixture(t)
		if _, err := DeleteContext(ctx, nil, "prod", d.store, d.save); !errors.Is(err, ErrUsage) {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})
}

// TestContextsFileKeyRefusal pins what a misspelled key in the contexts file is refused with:
// the file, the line, and the key, and no name from this program's source.
func TestContextsFileKeyRefusal(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "contexts:\n  prod:\n    server: https://a.example\nsrever: 1\n")
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("LoadFile = nil error, want one naming the key")
	}
	want := path + ": line 4: srever is not a contexts-file key"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
