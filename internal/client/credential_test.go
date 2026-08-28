package client

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// credFixture is one ResolveCredential call: a store, an issuer whose
// transport fails the test, and settings under one mode.
type credFixture struct {
	t       *testing.T
	store   *Store
	clock   *fakeClock
	dir     string
	in      CredentialInput
	env     map[string]string
	prompts []string
	writes  int
}

// newCred builds the fixture under mode, with the store's write seam
// counting every call, because no token source may write the cache.
func newCred(t *testing.T, mode string) *credFixture {
	t.Helper()
	store, clock, dir := testStore(t)
	f := &credFixture{t: t, store: store, clock: clock, dir: dir, env: map[string]string{}}
	store.write = func(dir, name string, data []byte, perm os.FileMode) error {
		f.writes++
		return atomicWrite(dir, name, data, perm)
	}
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t: t}, Now: clock.Now, Sleep: clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	s := testSettings(t, "https://profgate.example")
	s.Context.Auth = AuthSnap{Mode: mode}
	if mode == "oidc" {
		e := testEntry()
		s.Context.Auth = AuthSnap{Mode: mode, Issuer: e.Issuer, ClientID: e.ClientID, TokenType: e.TokenType}
	}
	f.in = CredentialInput{
		Getenv:   env(f.env),
		Settings: s,
		Store:    store,
		Issuer:   iss,
		Now:      clock.Now,
	}
	return f
}

// adhoc makes the fixture --server alone: no context and no snapshot.
func (f *credFixture) adhoc() {
	f.in.Settings.ContextName = ""
	f.in.Settings.Context = Context{}
	s, err := Resolve(nil, Overrides{Server: "https://profgate.example"}, env(nil))
	if err != nil {
		f.t.Fatal(err)
	}
	f.in.Settings.CacheName = s.CacheName
}

// withPrompt answers every prompt with password and records the user asked for.
func (f *credFixture) withPrompt(password string) {
	f.in.Prompt = func(user string) (string, error) {
		f.prompts = append(f.prompts, user)
		return password, nil
	}
}

// refusingPrompt fails the test when the prompt runs.
func (f *credFixture) refusingPrompt() {
	f.in.Prompt = func(user string) (string, error) {
		f.t.Fatalf("the prompt ran for %q", user)
		return "", nil
	}
}

// fresh writes an entry for the fixture's gateway that is far from expiry.
func (f *credFixture) fresh() {
	f.t.Helper()
	e := testEntry()
	e.Origin = f.in.Settings.Origin
	e.Token = "cached-secret"
	e.ExpiresAt = f.clock.Now().Add(time.Hour)
	if err := f.store.Write(f.in.Settings.CacheName, e); err != nil {
		f.t.Fatal(err)
	}
	f.writes = 0
}

// tokenFile writes content to a file in the fixture's directory and returns its path.
func (f *credFixture) tokenFile(content string) string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// apply resolves the credential and applies it to one request, returning
// the request so the test reads the header it set.
func (f *credFixture) apply() (*http.Request, Credential, error) {
	f.t.Helper()
	cred, err := ResolveCredential(f.in)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://profgate.example/v1/whoami", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if cred == nil {
		return req, nil, nil
	}
	if err := cred.Apply(context.Background(), req); err != nil {
		return nil, cred, err
	}
	return req, cred, nil
}

// bearer resolves and requires a bearer token, failing on anything else.
func (f *credFixture) bearer() string {
	f.t.Helper()
	req, cred, err := f.apply()
	if err != nil {
		f.t.Fatal(err)
	}
	if cred == nil {
		f.t.Fatal("no credential was chosen")
	}
	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Bearer ") {
		f.t.Fatalf("Authorization = %q, want a bearer token", got)
	}
	return strings.TrimPrefix(got, "Bearer ")
}

// basic resolves and requires a basic credential, returning the pair.
func (f *credFixture) basic() (string, string) {
	f.t.Helper()
	req, cred, err := f.apply()
	if err != nil {
		f.t.Fatal(err)
	}
	if cred == nil {
		f.t.Fatal("no credential was chosen")
	}
	user, password, ok := req.BasicAuth()
	if !ok {
		f.t.Fatalf("Authorization = %q, want basic", req.Header.Get("Authorization"))
	}
	return user, password
}

// usage resolves and requires a usage error whose message names each want.
func (f *credFixture) usage(want ...string) {
	f.t.Helper()
	_, err := ResolveCredential(f.in)
	if !errors.Is(err, ErrUsage) {
		f.t.Fatalf("err = %v, want a usage error", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			f.t.Fatalf("error %q does not name %q", err, w)
		}
	}
}

func (f *credFixture) assertNoWrite() {
	f.t.Helper()
	if f.writes != 0 {
		f.t.Fatalf("the store was written %d times", f.writes)
	}
}

func TestResolveCredentialTokenSources(t *testing.T) {
	t.Run("--token-file alone is the file's token, trimmed", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenFile = f.tokenFile("  file-secret\n")
		if got := f.bearer(); got != "file-secret" {
			t.Fatalf("token = %q", got)
		}
		f.assertNoWrite()
	})

	t.Run("--token-stdin alone is stdin's token, trimmed", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenStdin = true
		f.in.Stdin = strings.NewReader("\tstdin-secret\r\n")
		if got := f.bearer(); got != "stdin-secret" {
			t.Fatalf("token = %q", got)
		}
		f.assertNoWrite()
	})

	t.Run("--token-file and --token-stdin together", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenFile = f.tokenFile("file-secret")
		f.in.TokenStdin = true
		f.in.Stdin = strings.NewReader("stdin-secret")
		f.usage("--token-file", "--token-stdin")
	})

	t.Run("--token-file naming an empty file", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenFile = f.tokenFile(" \n")
		f.usage("--token-file", f.in.TokenFile)
	})

	t.Run("--token-stdin with empty input", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenStdin = true
		f.in.Stdin = strings.NewReader("\n")
		f.usage("--token-stdin")
	})

	t.Run("--token-file naming a missing file", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenFile = filepath.Join(t.TempDir(), "missing")
		f.usage("--token-file", f.in.TokenFile)
	})

	t.Run("PROFGATE_TOKEN alone", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.env["PROFGATE_TOKEN"] = " env-secret "
		if got := f.bearer(); got != "env-secret" {
			t.Fatalf("token = %q", got)
		}
		f.assertNoWrite()
	})

	t.Run("PROFGATE_TOKEN set and empty", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.env["PROFGATE_TOKEN"] = " "
		f.usage("PROFGATE_TOKEN")
	})

	t.Run("--token-file wins over PROFGATE_TOKEN", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.TokenFile = f.tokenFile("file-secret")
		f.env["PROFGATE_TOKEN"] = "env-secret"
		if got := f.bearer(); got != "file-secret" {
			t.Fatalf("token = %q", got)
		}
	})

	t.Run("PROFGATE_TOKEN wins over a fresh cache entry, which is never read", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.fresh()
		// A directory that grants group read makes every read of the store an
		// error, so a resolver that reads the cache fails here.
		info, err := os.Stat(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.dir, info.Mode().Perm()|0o040); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.store.Read(f.in.Settings.CacheName); err == nil {
			t.Fatal("the store still reads; the fixture cannot prove the cache was skipped")
		}
		f.env["PROFGATE_TOKEN"] = "env-secret"
		if got := f.bearer(); got != "env-secret" {
			t.Fatalf("token = %q", got)
		}
	})

	t.Run("a token source under basic sends the token, not the pair", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.env["PROFGATE_PASSWORD"] = "pw"
		f.env["PROFGATE_TOKEN"] = "env-secret"
		f.refusingPrompt()
		if got := f.bearer(); got != "env-secret" {
			t.Fatalf("token = %q", got)
		}
	})
}

func TestResolveCredentialCachedPath(t *testing.T) {
	t.Run("a fresh entry under oidc is the cached token", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.fresh()
		if got := f.bearer(); got != "cached-secret" {
			t.Fatalf("token = %q", got)
		}
		f.assertNoWrite()
	})

	t.Run("no entry under oidc is ErrLoginNeeded before any request", func(t *testing.T) {
		f := newCred(t, "oidc")
		_, err := ResolveCredential(f.in)
		if !errors.Is(err, ErrLoginNeeded) {
			t.Fatalf("err = %v, want ErrLoginNeeded", err)
		}
		if !strings.Contains(err.Error(), "prod") {
			t.Fatalf("error %q does not name the context", err)
		}
	})

	t.Run("an entry under basic is ignored and the basic path applies", func(t *testing.T) {
		f := newCred(t, "basic")
		f.fresh()
		f.in.User = "alice"
		f.env["PROFGATE_PASSWORD"] = "pw"
		user, password := f.basic()
		if user != "alice" || password != "pw" {
			t.Fatalf("pair = %q %q", user, password)
		}
	})

	t.Run("-u under oidc does not skip the cached path", func(t *testing.T) {
		f := newCred(t, "oidc")
		f.in.User = "alice"
		f.env["PROFGATE_PASSWORD"] = "pw"
		_, err := ResolveCredential(f.in)
		if !errors.Is(err, ErrLoginNeeded) {
			t.Fatalf("err = %v, want ErrLoginNeeded", err)
		}
	})
}

func TestResolveCredentialBasic(t *testing.T) {
	t.Run("-u and PROFGATE_PASSWORD, with the prompt never called", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.env["PROFGATE_PASSWORD"] = "pw"
		f.refusingPrompt()
		user, password := f.basic()
		if user != "alice" || password != "pw" {
			t.Fatalf("pair = %q %q", user, password)
		}
		f.assertNoWrite()
	})

	t.Run("-u alone with a prompt is the prompt's password", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.withPrompt("prompted")
		user, password := f.basic()
		if user != "alice" || password != "prompted" {
			t.Fatalf("pair = %q %q", user, password)
		}
		if len(f.prompts) != 1 || f.prompts[0] != "alice" {
			t.Fatalf("prompts = %v, want one for alice", f.prompts)
		}
	})

	t.Run("-u alone with no prompt names PROFGATE_PASSWORD", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.usage("PROFGATE_PASSWORD")
	})

	t.Run("a prompt that fails is a usage error", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.in.Prompt = func(string) (string, error) { return "", errors.New("password is empty") }
		f.usage("password is empty")
	})

	t.Run("PROFGATE_USER and PROFGATE_PASSWORD", func(t *testing.T) {
		f := newCred(t, "basic")
		f.env["PROFGATE_USER"] = "bob"
		f.env["PROFGATE_PASSWORD"] = "pw"
		f.refusingPrompt()
		user, password := f.basic()
		if user != "bob" || password != "pw" {
			t.Fatalf("pair = %q %q", user, password)
		}
	})

	t.Run("-u wins over PROFGATE_USER", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.env["PROFGATE_USER"] = "bob"
		f.env["PROFGATE_PASSWORD"] = "pw"
		user, _ := f.basic()
		if user != "alice" {
			t.Fatalf("user = %q, want the flag's", user)
		}
	})

	t.Run("--token-stdin and -u with no PROFGATE_PASSWORD both read stdin", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.TokenStdin = true
		f.in.Stdin = strings.NewReader("stdin-secret")
		f.in.User = "alice"
		f.withPrompt("prompted")
		f.usage("--token-stdin", "-u", "PROFGATE_PASSWORD")
		if len(f.prompts) != 0 {
			t.Fatal("the prompt ran")
		}
	})

	t.Run("--token-stdin and -u with PROFGATE_PASSWORD is the token", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.TokenStdin = true
		f.in.Stdin = strings.NewReader("stdin-secret")
		f.in.User = "alice"
		f.env["PROFGATE_PASSWORD"] = "pw"
		if got := f.bearer(); got != "stdin-secret" {
			t.Fatalf("token = %q", got)
		}
	})

	t.Run("no user names -u and PROFGATE_USER", func(t *testing.T) {
		f := newCred(t, "basic")
		f.env["PROFGATE_PASSWORD"] = "pw"
		f.usage("-u", "PROFGATE_USER")
	})

	t.Run("a user name with a colon", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "ali:ce"
		f.env["PROFGATE_PASSWORD"] = "pw"
		f.usage("colon")
	})

	t.Run("a password of 73 bytes", func(t *testing.T) {
		f := newCred(t, "basic")
		f.in.User = "alice"
		f.env["PROFGATE_PASSWORD"] = strings.Repeat("p", 73)
		f.usage("72")
	})
}

func TestResolveCredentialDisabled(t *testing.T) {
	f := newCred(t, "disabled")
	f.fresh()
	f.in.User = "alice"
	f.env["PROFGATE_PASSWORD"] = "pw"
	f.refusingPrompt()
	_, cred, err := f.apply()
	if err != nil {
		t.Fatal(err)
	}
	if cred != nil {
		t.Fatalf("credential = %T, want none under disabled", cred)
	}
}

// Without a snapshot — --server alone, or a context no login has recorded —
// the mode is unknown: an entry the login wrote is used, a named user is a
// basic pair, and otherwise nothing is sent.
func TestResolveCredentialWithoutSnapshot(t *testing.T) {
	t.Run("an ad-hoc entry is compared against its own values", func(t *testing.T) {
		f := newCred(t, "")
		f.adhoc()
		f.fresh()
		if got := f.bearer(); got != "cached-secret" {
			t.Fatalf("token = %q", got)
		}
	})

	t.Run("an ad-hoc entry for another origin is refused", func(t *testing.T) {
		f := newCred(t, "")
		f.adhoc()
		e := testEntry()
		e.Origin = "https://someone-elses.example:443"
		e.ExpiresAt = f.clock.Now().Add(time.Hour)
		if err := f.store.Write(f.in.Settings.CacheName, e); err != nil {
			t.Fatal(err)
		}
		_, _, err := f.apply()
		if !errors.Is(err, ErrLoginNeeded) || !strings.Contains(err.Error(), "someone-elses") {
			t.Fatalf("err = %v, want ErrLoginNeeded naming the entry's origin", err)
		}
	})

	t.Run("no entry and a user is the basic pair", func(t *testing.T) {
		f := newCred(t, "")
		f.adhoc()
		f.env["PROFGATE_USER"] = "bob"
		f.env["PROFGATE_PASSWORD"] = "pw"
		user, password := f.basic()
		if user != "bob" || password != "pw" {
			t.Fatalf("pair = %q %q", user, password)
		}
	})

	t.Run("no entry and no user sends nothing", func(t *testing.T) {
		f := newCred(t, "")
		f.adhoc()
		_, cred, err := f.apply()
		if err != nil {
			t.Fatal(err)
		}
		if cred != nil {
			t.Fatalf("credential = %T, want none", cred)
		}
	})
}
