package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testGateway   = "https://profgate.example"
	testPrincipal = "alice"
	testRealm     = "engineering"
)

// authOIDC is the /v1/auth body of a gateway whose operator configured the
// device login.
func authOIDC(tokenType string, pkce bool) string {
	p := "false"
	if pkce {
		p = "true"
	}
	return `{"mode":"oidc","oidc":{"issuer":"` + testIssuer + `","clientID":"profgate-cli","tokenType":"` + tokenType + `","scopes":["openid","offline_access"],"pkce":` + p + `}}`
}

// loginTransport is the gateway and every issuer in one round tripper.
// Any https host answers discovery with a document naming itself as the
// issuer and its endpoints under its own origin, so a flag that moves the
// issuer is observable by which origin was discovered.
// Every request is appended to events, in order, by a short name.
type loginTransport struct {
	t       *testing.T
	events  *[]string
	auth    pollStep
	device  string
	steps   []pollStep
	whoami  pollStep
	revokes []pollStep
	// noDevice omits device_authorization_endpoint from discovery.
	noDevice bool
	// noRevoke omits revocation_endpoint from discovery.
	noRevoke bool
	// beforeRevoke runs before the revocation endpoint answers.
	beforeRevoke func()

	discovered []string
	deviceReqs []url.Values
	polls      []url.Values
	whoamiReqs []*http.Request
	revokeReqs []url.Values
	authReqs   []*http.Request
}

func (l *loginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	l.t.Helper()
	origin := req.URL.Scheme + "://" + req.URL.Host
	switch {
	case origin == testGateway && req.URL.Path == "/v1/auth":
		l.authReqs = append(l.authReqs, req)
		*l.events = append(*l.events, "auth")
		return l.answer(req, l.auth), nil
	case origin == testGateway && req.URL.Path == "/v1/whoami":
		l.whoamiReqs = append(l.whoamiReqs, req)
		*l.events = append(*l.events, "whoami")
		return l.answer(req, l.whoami), nil
	case req.URL.Path == discoveryPath:
		l.discovered = append(l.discovered, origin)
		*l.events = append(*l.events, "discovery")
		doc := `{"issuer":"` + origin + `","token_endpoint":"` + origin + `/token"`
		if !l.noDevice {
			doc += `,"device_authorization_endpoint":"` + origin + `/device"`
		}
		if !l.noRevoke {
			doc += `,"revocation_endpoint":"` + origin + `/revoke"`
		}
		return jsonResponse(req, http.StatusOK, doc+"}", false), nil
	case req.URL.Path == "/device":
		l.deviceReqs = append(l.deviceReqs, readForm(l.t, req))
		*l.events = append(*l.events, "device")
		return jsonResponse(req, http.StatusOK, l.device, false), nil
	case req.URL.Path == "/token":
		l.polls = append(l.polls, readForm(l.t, req))
		*l.events = append(*l.events, "token")
		return l.next(req, &l.steps, len(l.polls))
	case req.URL.Path == "/revoke":
		l.revokeReqs = append(l.revokeReqs, readForm(l.t, req))
		*l.events = append(*l.events, "revoke")
		if l.beforeRevoke != nil {
			l.beforeRevoke()
		}
		return l.next(req, &l.revokes, len(l.revokeReqs))
	default:
		l.t.Fatalf("unexpected request to %s", req.URL)
		return nil, nil
	}
}

func (l *loginTransport) answer(req *http.Request, step pollStep) *http.Response {
	if step.status == 0 {
		step.status = http.StatusOK
	}
	return jsonResponse(req, step.status, step.body, step.html)
}

func (l *loginTransport) next(req *http.Request, steps *[]pollStep, n int) (*http.Response, error) {
	l.t.Helper()
	if len(*steps) == 0 {
		l.t.Fatalf("request %d has no scripted answer", n)
	}
	step := (*steps)[0]
	*steps = (*steps)[1:]
	if step.fail {
		return nil, errors.New("connection refused")
	}
	return jsonResponse(req, step.status, step.body, step.html), nil
}

// loginFixture is one login: the store, the clock, the transport, the
// writers, and what SaveFile received.
type loginFixture struct {
	t        *testing.T
	rt       *loginTransport
	store    *Store
	clock    *fakeClock
	dir      string
	settings Settings
	file     *File
	saved    *File
	saveErr  error
	noSave   bool
	events   []string
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	verbose  bytes.Buffer
	flags    LoginFlags
	basic    func() (string, string, error)
}

// newLogin builds a fixture whose gateway reports the device login with
// tokenType access, whose device endpoint answers with the complete URI,
// and whose token endpoint issues on the first poll.
func newLogin(t *testing.T) *loginFixture {
	t.Helper()
	store, clock, dir := testStore(t)
	f := &loginFixture{t: t, store: store, clock: clock, dir: dir}
	f.rt = &loginTransport{
		t:      t,
		events: &f.events,
		auth:   pollStep{body: authOIDC("access", false)},
		device: deviceBody(5),
		steps:  []pollStep{issued()},
		whoami: pollStep{body: `{"principal":"` + testPrincipal + `","realm":{"name":"` + testRealm + `"}}`},
	}
	store.write = func(dir, name string, data []byte, mode os.FileMode) error {
		f.events = append(f.events, "cache")
		return atomicWrite(dir, name, data, mode)
	}
	u, err := url.Parse(testGateway)
	if err != nil {
		t.Fatal(err)
	}
	f.settings = Settings{
		ContextName: "prod",
		Context:     Context{Server: testGateway, Auth: AuthSnap{Mode: "oidc", Issuer: "https://old.example", ClientID: "old", TokenType: "id", Scopes: []string{"openid"}}},
		Server:      u,
		Origin:      CanonicalOrigin(u),
		CacheName:   "prod",
	}
	f.file = &File{CurrentContext: "prod", Contexts: map[string]Context{"prod": f.settings.Context}}
	return f
}

// adhoc makes the fixture --server alone: no context, no file.
func (f *loginFixture) adhoc() {
	f.settings.ContextName = ""
	f.settings.Context = Context{}
	s, err := Resolve(nil, Overrides{Server: testGateway}, env(nil))
	if err != nil {
		f.t.Fatal(err)
	}
	f.settings.CacheName = s.CacheName
	f.file = nil
	f.noSave = true
}

func (f *loginFixture) input() LoginInput {
	f.t.Helper()
	gw, err := New(Options{Settings: f.settings, Transport: f.rt, Now: f.clock.Now, Verbose: &f.verbose})
	if err != nil {
		f.t.Fatal(err)
	}
	iss, err := NewIssuer(IssuerOptions{Transport: f.rt, Now: f.clock.Now, Sleep: f.clock.Sleep, Verbose: &f.verbose})
	if err != nil {
		f.t.Fatal(err)
	}
	in := LoginInput{
		Settings: f.settings,
		Gateway:  gw,
		Issuer:   iss,
		Store:    f.store,
		Flags:    f.flags,
		Now:      f.clock.Now,
		Stdout:   &f.stdout,
		Stderr:   &f.stderr,
		Basic:    f.basic,
		File:     f.file,
	}
	if !f.noSave {
		in.SaveFile = func(file *File) error {
			f.events = append(f.events, "file")
			if f.saveErr != nil {
				return f.saveErr
			}
			f.saved = file
			return nil
		}
	}
	return in
}

func (f *loginFixture) login() (Whoami, error) {
	f.t.Helper()
	return Login(context.Background(), f.input())
}

func (f *loginFixture) logout() error {
	f.t.Helper()
	in := f.input()
	return Logout(context.Background(), LogoutInput{Settings: f.settings, Issuer: in.Issuer, Store: f.store, Stdout: &f.stdout, Stderr: &f.stderr})
}

func (f *loginFixture) entry() (Entry, bool) {
	f.t.Helper()
	e, ok, err := f.store.Read(f.settings.CacheName)
	if err != nil {
		f.t.Fatal(err)
	}
	return e, ok
}

func (f *loginFixture) lockPath() string {
	return filepath.Join(f.dir, f.settings.CacheName+".lock")
}

// assertNoSecretPrinted fails when a token or the device code reached any
// writer the test supplied.
func (f *loginFixture) assertNoSecretPrinted() {
	f.t.Helper()
	for _, w := range []*bytes.Buffer{&f.stdout, &f.stderr, &f.verbose} {
		for _, secret := range []string{testDeviceCode, "id-secret", "access-secret", "refresh-secret", "cached-secret", "refresh-old"} {
			if strings.Contains(w.String(), secret) {
				f.t.Fatalf("a writer carries %q: %q", secret, w.String())
			}
		}
	}
}

func TestLoginHappyPathOrder(t *testing.T) {
	f := newLogin(t)
	w, err := f.login()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"auth", "discovery", "device", "token", "cache", "whoami", "file"}
	if !slices.Equal(f.events, want) {
		t.Fatalf("events = %v, want %v", f.events, want)
	}
	if w != (Whoami{Principal: testPrincipal, Realm: testRealm}) {
		t.Fatalf("Whoami = %+v", w)
	}
	e, ok := f.entry()
	if !ok || e.Token != "access-secret" || e.RefreshToken != "refresh-secret" || e.Origin != f.settings.Origin || e.Issuer != testIssuer || e.ClientID != "profgate-cli" || e.TokenType != "access" {
		t.Fatalf("entry = %+v, %v", e, ok)
	}
	if !e.ExpiresAt.Equal(e.ObtainedAt.Add(300 * time.Second)) {
		t.Fatalf("expiresAt = %v, want obtainedAt plus expires_in", e.ExpiresAt)
	}
	if got := f.whoamiReqs()[0].Header.Get("Authorization"); got != "Bearer access-secret" {
		t.Fatalf("whoami Authorization = %q", got)
	}
	f.assertNoSecretPrinted()
}

func (f *loginFixture) whoamiReqs() []*http.Request {
	f.t.Helper()
	if len(f.rt.whoamiReqs) == 0 {
		f.t.Fatal("no whoami request was sent")
	}
	return f.rt.whoamiReqs
}

func TestLoginStderrOutput(t *testing.T) {
	t.Run("the code, the URI, then the complete URI on its own line", func(t *testing.T) {
		f := newLogin(t)
		if _, err := f.login(); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(f.stderr.String(), "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("stderr = %q, want three lines", f.stderr.String())
		}
		if !strings.Contains(lines[0], testUserCode) || strings.Contains(lines[0], "https://") {
			t.Fatalf("line 1 = %q, want the user code alone", lines[0])
		}
		if !strings.Contains(lines[1], "https://issuer.example/device") || strings.Contains(lines[1], "user_code=") {
			t.Fatalf("line 2 = %q, want the verification URI", lines[1])
		}
		if !strings.Contains(lines[2], "https://issuer.example/device?user_code="+testUserCode) {
			t.Fatalf("line 3 = %q, want the complete URI", lines[2])
		}
	})

	t.Run("no complete URI is two lines and no empty third", func(t *testing.T) {
		f := newLogin(t)
		f.rt.device = `{"device_code":"` + testDeviceCode + `","user_code":"` + testUserCode + `","verification_uri":"https://issuer.example/device","expires_in":600,"interval":5}`
		if _, err := f.login(); err != nil {
			t.Fatal(err)
		}
		if lines := strings.Split(strings.TrimSuffix(f.stderr.String(), "\n"), "\n"); len(lines) != 2 || lines[1] == "" {
			t.Fatalf("stderr = %q, want exactly two lines", f.stderr.String())
		}
	})
}

func TestLoginWithoutDeviceEndpoint(t *testing.T) {
	f := newLogin(t)
	f.rt.noDevice = true
	_, err := f.login()
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "device grant") || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("err = %v, want a usage error naming the grant and --token-file", err)
	}
	if len(f.rt.deviceReqs) != 0 {
		t.Fatal("a device request was made")
	}
	if _, ok := f.entry(); ok {
		t.Fatal("an entry was cached")
	}
}

func TestLoginMissingToken(t *testing.T) {
	cases := []struct {
		tokenType string
		body      string
	}{
		{"id", `{"access_token":"access-secret","expires_in":300}`},
		{"access", `{"id_token":"id-secret","expires_in":300}`},
	}
	for _, tc := range cases {
		t.Run("no "+tc.tokenType+" token", func(t *testing.T) {
			f := newLogin(t)
			f.rt.auth = pollStep{body: authOIDC(tc.tokenType, false)}
			f.rt.steps = []pollStep{{status: http.StatusOK, body: tc.body}}
			_, err := f.login()
			if err == nil || !strings.Contains(err.Error(), tc.tokenType+" token") {
				t.Fatalf("err = %v, want one naming the %s token", err, tc.tokenType)
			}
			if errors.Is(err, ErrUsage) {
				t.Fatal("a missing token is not a usage error")
			}
			if _, ok := f.entry(); ok {
				t.Fatal("an entry was cached")
			}
			if len(f.rt.whoamiReqs) != 0 {
				t.Fatal("whoami was requested")
			}
			f.assertNoSecretPrinted()
		})
	}
}

func TestLoginDeadline(t *testing.T) {
	cases := []struct {
		name      string
		expiresIn string
		timeout   time.Duration
		stopsBy   time.Duration
		polls     int
	}{
		{"the code expires sooner than the timeout", "30", 10 * time.Minute, 30 * time.Second, 6},
		{"the timeout is sooner than the code's expiry", "600", time.Minute, time.Minute, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLogin(t)
			f.rt.device = `{"device_code":"` + testDeviceCode + `","user_code":"` + testUserCode + `","verification_uri":"https://issuer.example/device","expires_in":` + tc.expiresIn + `,"interval":5}`
			f.rt.steps = slices.Repeat([]pollStep{pending()}, 100)
			f.flags.LoginTimeout = tc.timeout
			start := f.clock.Now()
			_, err := f.login()
			if !errors.Is(err, errDeadline) {
				t.Fatalf("err = %v, want the deadline", err)
			}
			if elapsed := f.clock.Now().Sub(start); elapsed >= tc.stopsBy {
				t.Fatalf("polling ran %v, want it to stop before %v", elapsed, tc.stopsBy)
			}
			if len(f.rt.polls) != tc.polls {
				t.Fatalf("%d polls, want %d", len(f.rt.polls), tc.polls)
			}
			if _, ok := f.entry(); ok {
				t.Fatal("an entry was cached")
			}
		})
	}
}

func TestLoginTimeoutRange(t *testing.T) {
	for _, d := range []time.Duration{59 * time.Second, 31 * time.Minute, -time.Minute} {
		t.Run(d.String(), func(t *testing.T) {
			f := newLogin(t)
			f.rt = &loginTransport{t: t, events: &f.events}
			f.flags.LoginTimeout = d
			in := f.input()
			in.Gateway = serveRefusing(t, f.settings)
			_, err := Login(context.Background(), in)
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "1m") || !strings.Contains(err.Error(), "30m") {
				t.Fatalf("err = %v, want a usage error naming the range", err)
			}
		})
	}
}

func TestLoginPlaintextGateway(t *testing.T) {
	t.Run("a non-loopback http:// gateway is refused before any request", func(t *testing.T) {
		f := newLogin(t)
		f.rt = &loginTransport{t: t, events: &f.events}
		u, err := url.Parse("http://profgate.example")
		if err != nil {
			t.Fatal(err)
		}
		f.settings.Server = u
		f.settings.Origin = CanonicalOrigin(u)
		in := f.input()
		in.Gateway = serveRefusing(t, f.settings)
		_, err = Login(context.Background(), in)
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "https://") {
			t.Fatalf("err = %v, want a usage error naming the plaintext rule", err)
		}
		if len(f.events) != 0 {
			t.Fatalf("events = %v, want none: the gateway was never asked", f.events)
		}
		if _, ok := f.entry(); ok {
			t.Fatal("an entry was written for a refused login")
		}
	})
	t.Run("a loopback http:// gateway proceeds with the warning", func(t *testing.T) {
		f := newLogin(t)
		u, err := url.Parse("http://localhost:8080")
		if err != nil {
			t.Fatal(err)
		}
		f.settings.Server = u
		f.settings.Origin = CanonicalOrigin(u)
		f.settings.Context.Server = u.String()
		gw, err := New(Options{Settings: f.settings, Transport: rehosted{f.rt}, Now: f.clock.Now, Warn: &f.stderr})
		if err != nil {
			t.Fatal(err)
		}
		in := f.input()
		in.Gateway = gw
		if _, err := Login(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		want := []string{"auth", "discovery", "device", "token", "cache", "whoami", "file"}
		if !slices.Equal(f.events, want) {
			t.Fatalf("events = %v, want %v", f.events, want)
		}
		if !strings.Contains(f.stderr.String(), "warning: sending a credential over plaintext to http://localhost:8080/v1/whoami") {
			t.Fatalf("stderr = %q, want the loopback warning for the request that carried the token", f.stderr.String())
		}
		f.assertNoSecretPrinted()
	})
}

// rehosted answers a request to any gateway origin as the test gateway,
// so a fixture can select a loopback address and still be scripted.
type rehosted struct{ rt *loginTransport }

func (r rehosted) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/v1/auth" || req.URL.Path == "/v1/whoami" {
		req = req.Clone(req.Context())
		req.URL.Scheme = "https"
		req.URL.Host = "profgate.example"
	}
	return r.rt.RoundTrip(req)
}

// serveRefusing is a gateway client whose transport fails the test.
func serveRefusing(t *testing.T, s Settings) *Client {
	t.Helper()
	c, err := New(Options{Settings: s, Transport: refusingRoundTripper{t}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoginCacheWrittenBeforeWhoami(t *testing.T) {
	f := newLogin(t)
	f.rt.whoami = pollStep{status: http.StatusServiceUnavailable, body: `{"error":"not ready","code":"not_ready"}`}
	_, err := f.login()
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "not_ready" {
		t.Fatalf("err = %v, want the gateway's refusal", err)
	}
	if !slices.Equal(f.events, []string{"auth", "discovery", "device", "token", "cache", "whoami"}) {
		t.Fatalf("events = %v, want the cache written before whoami and no file write", f.events)
	}
	if _, ok := f.entry(); !ok {
		t.Fatal("the entry is missing: the cache is written before whoami")
	}
}

func TestLoginWhoami401Diagnostic(t *testing.T) {
	f := newLogin(t)
	f.rt.whoami = pollStep{status: http.StatusUnauthorized, body: `{"error":"authentication required","code":"unauthenticated"}`}
	_, err := f.login()
	var d *AuthDiagnostic
	if !errors.As(err, &d) || d.Issuer != testIssuer || d.ClientID != "profgate-cli" {
		t.Fatalf("err = %v, want an AuthDiagnostic naming the issuer and client", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the 401 to unwrap", err)
	}
	if msg := err.Error(); !strings.Contains(msg, testIssuer) || !strings.Contains(msg, "profgate-cli") {
		t.Fatalf("message %q lacks the issuer or the client identifier", msg)
	}
	if slices.Contains(f.events, "file") {
		t.Fatal("the file was written after a refused whoami")
	}
	f.assertNoSecretPrinted()
}

func TestLoginPrintsAndRewritesTheSnapshot(t *testing.T) {
	f := newLogin(t)
	f.rt.auth = pollStep{body: authOIDC("access", true)}
	if _, err := f.login(); err != nil {
		t.Fatal(err)
	}
	if out := f.stdout.String(); !strings.Contains(out, testPrincipal) || !strings.Contains(out, testRealm) {
		t.Fatalf("stdout = %q, want the principal and the realm", out)
	}
	if f.saved == nil {
		t.Fatal("the file was not saved")
	}
	want := AuthSnap{Mode: "oidc", Issuer: testIssuer, ClientID: "profgate-cli", TokenType: "access", Scopes: []string{"openid", "offline_access"}, PKCE: true}
	got := f.saved.Contexts["prod"].Auth
	if got.Mode != want.Mode || got.Issuer != want.Issuer || got.ClientID != want.ClientID || got.TokenType != want.TokenType || !slices.Equal(got.Scopes, want.Scopes) || got.PKCE != want.PKCE {
		t.Fatalf("auth = %+v, want %+v", got, want)
	}
	if c := f.saved.Contexts["prod"]; c.Server != testGateway || f.saved.CurrentContext != "prod" {
		t.Fatalf("the rest of the context changed: %+v", f.saved)
	}
}

func TestLoginOverwritesAnEntryForAnotherGateway(t *testing.T) {
	f := newLogin(t)
	other := testEntry()
	other.Origin = "https://someone-elses.example:443"
	if err := f.store.Write("prod", other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.login(); err != nil {
		t.Fatal(err)
	}
	if e, _ := f.entry(); e.Origin != f.settings.Origin || e.Token != "access-secret" {
		t.Fatalf("entry = %+v, want the login's", e)
	}
}

func TestLoginFlagsOverride(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name  string
		auth  string
		flags LoginFlags
		check func(t *testing.T, f *loginFixture)
	}{
		{
			name:  "--issuer",
			flags: LoginFlags{Issuer: "https://other.example"},
			check: func(t *testing.T, f *loginFixture) {
				if !slices.Equal(f.rt.discovered, []string{"https://other.example"}) {
					t.Fatalf("discovered %v", f.rt.discovered)
				}
				if e, _ := f.entry(); e.Issuer != "https://other.example" {
					t.Fatalf("entry issuer = %q", e.Issuer)
				}
			},
		},
		{
			name:  "--client-id",
			flags: LoginFlags{ClientID: "other-client"},
			check: func(t *testing.T, f *loginFixture) {
				if got := f.rt.deviceReqs[0].Get("client_id"); got != "other-client" {
					t.Fatalf("client_id = %q", got)
				}
				if got := f.rt.polls[0].Get("client_id"); got != "other-client" {
					t.Fatalf("poll client_id = %q", got)
				}
			},
		},
		{
			name:  "--token-type",
			flags: LoginFlags{TokenType: "id"},
			check: func(t *testing.T, f *loginFixture) {
				if e, _ := f.entry(); e.TokenType != "id" || !strings.HasPrefix(e.Token, "eyJ") {
					t.Fatalf("entry = %+v, want the id token", e)
				}
			},
		},
		{
			name:  "--scope",
			flags: LoginFlags{Scopes: []string{"openid", "groups"}},
			check: func(t *testing.T, f *loginFixture) {
				if got := f.rt.deviceReqs[0].Get("scope"); got != "openid groups" {
					t.Fatalf("scope = %q", got)
				}
			},
		},
		{
			name:  "--pkce",
			flags: LoginFlags{PKCE: boolPtr(true)},
			check: func(t *testing.T, f *loginFixture) {
				if !f.rt.deviceReqs[0].Has("code_challenge") || !f.rt.polls[0].Has("code_verifier") {
					t.Fatal("PKCE was not sent")
				}
				if !f.saved.Contexts["prod"].Auth.PKCE {
					t.Fatal("the snapshot does not record pkce")
				}
			},
		},
		{
			name:  "--no-pkce",
			auth:  authOIDC("access", true),
			flags: LoginFlags{PKCE: boolPtr(false)},
			check: func(t *testing.T, f *loginFixture) {
				if f.rt.deviceReqs[0].Has("code_challenge") || f.rt.polls[0].Has("code_verifier") {
					t.Fatal("PKCE was sent")
				}
				if f.saved.Contexts["prod"].Auth.PKCE {
					t.Fatal("the snapshot records pkce")
				}
			},
		},
		{
			name:  "--issuer-ca-file",
			flags: LoginFlags{IssuerCAFile: "set below"},
			check: func(t *testing.T, f *loginFixture) {
				if got := f.saved.Contexts["prod"].Auth.IssuerCAFile; got != f.flags.IssuerCAFile {
					t.Fatalf("issuerCAFile = %q, want %q", got, f.flags.IssuerCAFile)
				}
			},
		},
		{
			name:  "--login-timeout",
			flags: LoginFlags{LoginTimeout: time.Minute},
			check: func(t *testing.T, f *loginFixture) {
				if len(f.rt.polls) != 12 {
					t.Fatalf("%d polls, want 12 before a one-minute timeout", len(f.rt.polls))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLogin(t)
			if tc.auth != "" {
				f.rt.auth = pollStep{body: tc.auth}
			}
			f.flags = tc.flags
			if tc.flags.IssuerCAFile != "" {
				f.flags.IssuerCAFile = certFile(t, t.TempDir())
			}
			issuedBoth := pollStep{status: http.StatusOK, body: `{"id_token":"` + idTokenExpiring(f.clock.Now().Add(time.Hour)) + `","access_token":"access-secret","refresh_token":"refresh-secret","expires_in":300}`}
			f.rt.steps = []pollStep{issuedBoth}
			if tc.flags.LoginTimeout != 0 {
				f.rt.steps = slices.Repeat([]pollStep{pending()}, 100)
			}
			_, err := f.login()
			if tc.flags.LoginTimeout == 0 && err != nil {
				t.Fatal(err)
			}
			tc.check(t, f)
		})
	}
}

func TestPKCEFlag(t *testing.T) {
	if _, err := PKCEFlag(true, true); !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want a usage error for --pkce with --no-pkce", err)
	}
	if p, err := PKCEFlag(false, false); err != nil || p != nil {
		t.Fatalf("neither flag = %v, %v; want nil", p, err)
	}
	if p, err := PKCEFlag(true, false); err != nil || p == nil || !*p {
		t.Fatalf("--pkce = %v, %v; want true", p, err)
	}
	if p, err := PKCEFlag(false, true); err != nil || p == nil || *p {
		t.Fatalf("--no-pkce = %v, %v; want false", p, err)
	}
}

func TestLoginCreatesTheContext(t *testing.T) {
	f := newLogin(t)
	ca := certFile(t, t.TempDir())
	f.settings.ContextName = "staging"
	f.settings.CacheName = "staging"
	f.settings.Context = Context{}
	f.settings.CAFile = ca
	f.settings.ServerName = "profgate.internal"
	f.settings.Namespace = "payments"
	for _, file := range []*File{nil, f.file} {
		f.rt.steps = []pollStep{issued()}
		f.file = file
		if _, err := f.login(); err != nil {
			t.Fatal(err)
		}
		c, ok := f.saved.Contexts["staging"]
		if !ok {
			t.Fatalf("file = %+v, want a staging context", f.saved)
		}
		if c.Server != testGateway || c.CAFile != ca || c.ServerName != "profgate.internal" || c.Namespace != "payments" || c.Auth.Mode != "oidc" || c.Auth.Issuer != testIssuer {
			t.Fatalf("context = %+v", c)
		}
		if file != nil {
			if _, ok := f.saved.Contexts["prod"]; !ok || f.saved.CurrentContext != "prod" {
				t.Fatalf("the existing entries changed: %+v", f.saved)
			}
		}
	}
}

func TestLoginServerAloneWritesNoFile(t *testing.T) {
	f := newLogin(t)
	f.adhoc()
	if _, err := f.login(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.events, "file") {
		t.Fatal("the file was written")
	}
	e, ok := f.entry()
	if !ok || !strings.HasPrefix(f.settings.CacheName, "adhoc-") {
		t.Fatalf("entry %s = %+v, %v", f.settings.CacheName, e, ok)
	}
	if e.Issuer != testIssuer || e.ClientID != "profgate-cli" || e.TokenType != "access" {
		t.Fatalf("the ad-hoc entry does not carry the login's values: %+v", e)
	}
}

func TestLoginFileWriteFailureKeepsTheCache(t *testing.T) {
	f := newLogin(t)
	f.saveErr = errors.New("write /home/alice/.config/profgate/config.yaml: read-only file system")
	_, err := f.login()
	if err == nil || !strings.Contains(err.Error(), "/home/alice/.config/profgate/config.yaml") {
		t.Fatalf("err = %v, want one naming the file", err)
	}
	if _, ok := f.entry(); !ok {
		t.Fatal("the cache entry is gone: the login succeeded")
	}
	if out := f.stdout.String(); !strings.Contains(out, testPrincipal) {
		t.Fatalf("stdout = %q, want the principal printed before the file failure", out)
	}
}

func TestSaveFileErrorNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := saveFile(path, &File{}, func(string, string, []byte, os.FileMode) error { return errors.New("disk full") })
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
}

func TestCachedCredential401Diagnostic(t *testing.T) {
	f := refreshIssuer(t, "access")
	f.write(t, f.entry(time.Hour))
	c, err := New(Options{Settings: f.settings, Credential: CachedCredential(f.store, f.iss, f.settings, f.clock.Now), Now: f.clock.Now, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, http.StatusUnauthorized, `{"error":"authentication required","code":"unauthenticated"}`, false), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
	var d *AuthDiagnostic
	if !errors.As(err, &d) || d.Issuer != testIssuer || d.ClientID != "profgate-cli" {
		t.Fatalf("err = %v, want an AuthDiagnostic from the entry", err)
	}
	if msg := err.Error(); !strings.Contains(msg, testIssuer) || !strings.Contains(msg, "profgate-cli") || strings.Contains(msg, "cached-secret") {
		t.Fatalf("message = %q", msg)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the 401 to unwrap", err)
	}
}

func TestA403IsNotDiagnosed(t *testing.T) {
	f := refreshIssuer(t, "access")
	f.write(t, f.entry(time.Hour))
	c, err := New(Options{Settings: f.settings, Credential: CachedCredential(f.store, f.iss, f.settings, f.clock.Now), Now: f.clock.Now, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, http.StatusForbidden, `{"error":"denied","code":"realm_denied"}`, false), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
	var d *AuthDiagnostic
	if errors.As(err, &d) {
		t.Fatalf("err = %v is diagnosed; a new login changes nothing for a realm refusal", err)
	}
}

func TestLoginAndLogoutTakeTheLock(t *testing.T) {
	t.Run("login", func(t *testing.T) {
		f := newLogin(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.lockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := f.login()
		if err == nil || !strings.Contains(err.Error(), f.lockPath()) {
			t.Fatalf("err = %v, want one naming %s", err, f.lockPath())
		}
		if _, ok := f.entry(); ok {
			t.Fatal("an entry was written while the lock was held")
		}
		if len(f.rt.whoamiReqs) != 0 {
			t.Fatal("whoami was requested")
		}
	})

	t.Run("logout", func(t *testing.T) {
		f := newLogin(t)
		if err := f.store.Write("prod", f.cached()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.lockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := f.logout()
		if err == nil || !strings.Contains(err.Error(), f.lockPath()) {
			t.Fatalf("err = %v, want one naming %s", err, f.lockPath())
		}
		if _, ok := f.entry(); !ok {
			t.Fatal("the entry was deleted while the lock was held")
		}
		if len(f.rt.revokeReqs) != 0 {
			t.Fatal("a revocation was sent")
		}
	})
}

// cached is an entry for the fixture's gateway with a refresh token.
func (f *loginFixture) cached() Entry {
	return Entry{
		Origin:       f.settings.Origin,
		Issuer:       testIssuer,
		ClientID:     "profgate-cli",
		TokenType:    "access",
		Token:        "cached-secret",
		ExpiresAt:    f.clock.Now().Add(time.Hour),
		RefreshToken: "refresh-old",
		ObtainedAt:   f.clock.Now(),
	}
}

func TestLoginAuthFallback(t *testing.T) {
	block := AuthSnap{Mode: "oidc", Issuer: testIssuer, ClientID: "from-block", TokenType: "access", Scopes: []string{"openid"}}
	fallbacks := []struct {
		name string
		auth pollStep
	}{
		{"404 route_unknown", pollStep{status: http.StatusNotFound, body: `{"error":"no such route","code":"route_unknown"}`}},
		{"oidc with no oidc object", pollStep{body: `{"mode":"oidc"}`}},
	}
	for _, tc := range fallbacks {
		t.Run(tc.name+" falls back to the block", func(t *testing.T) {
			f := newLogin(t)
			f.rt.auth = tc.auth
			f.settings.Context.Auth = block
			if _, err := f.login(); err != nil {
				t.Fatal(err)
			}
			if got := f.rt.deviceReqs[0].Get("client_id"); got != "from-block" {
				t.Fatalf("client_id = %q, want the block's", got)
			}
		})
		t.Run(tc.name+" falls back to the flags", func(t *testing.T) {
			f := newLogin(t)
			f.rt.auth = tc.auth
			f.settings.Context.Auth = AuthSnap{}
			f.flags = LoginFlags{Issuer: testIssuer, ClientID: "from-flags"}
			f.rt.steps = []pollStep{{status: http.StatusOK, body: `{"id_token":"` + idTokenExpiring(f.clock.Now().Add(time.Hour)) + `","expires_in":300}`}}
			if _, err := f.login(); err != nil {
				t.Fatal(err)
			}
			if got := f.rt.deviceReqs[0].Get("client_id"); got != "from-flags" {
				t.Fatalf("client_id = %q, want the flag's", got)
			}
			if got := f.rt.deviceReqs[0].Get("scope"); got != "openid offline_access" {
				t.Fatalf("scope = %q, want the default scopes", got)
			}
			if e, _ := f.entry(); e.TokenType != "id" {
				t.Fatalf("tokenType = %q, want the default id", e.TokenType)
			}
		})
		t.Run(tc.name+" with neither names the keys", func(t *testing.T) {
			f := newLogin(t)
			f.rt.auth = tc.auth
			f.settings.Context.Auth = AuthSnap{}
			_, err := f.login()
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("err = %v, want a usage error", err)
			}
			for _, key := range []string{"auth.mode", "auth.issuer", "auth.clientID", "--issuer", "--client-id"} {
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("err %q does not name %s", err, key)
				}
			}
			if len(f.rt.discovered) != 0 {
				t.Fatal("discovery ran")
			}
		})
	}

	t.Run("another refusal is reported as it is", func(t *testing.T) {
		f := newLogin(t)
		f.rt.auth = pollStep{status: http.StatusServiceUnavailable, body: `{"error":"not ready","code":"not_ready"}`}
		f.settings.Context.Auth = block
		_, err := f.login()
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "not_ready" {
			t.Fatalf("err = %v, want not_ready", err)
		}
	})
}

func TestLoginBasic(t *testing.T) {
	t.Run("verifies, stores nothing, and names the variables", func(t *testing.T) {
		f := newLogin(t)
		f.rt.auth = pollStep{body: `{"mode":"basic"}`}
		f.basic = func() (string, string, error) { return "alice", "hunter2-secret", nil }
		w, err := f.login()
		if err != nil {
			t.Fatal(err)
		}
		if w.Principal != testPrincipal || w.Realm != testRealm {
			t.Fatalf("Whoami = %+v", w)
		}
		user, password, ok := f.whoamiReqs()[0].BasicAuth()
		if !ok || user != "alice" || password != "hunter2-secret" {
			t.Fatalf("whoami carried %q %q %v", user, password, ok)
		}
		if _, ok := f.entry(); ok {
			t.Fatal("something was stored")
		}
		if out := f.stdout.String(); !strings.Contains(out, testPrincipal) || !strings.Contains(out, testRealm) {
			t.Fatalf("stdout = %q", out)
		}
		if msg := f.stderr.String(); !strings.Contains(msg, "PROFGATE_USER") || !strings.Contains(msg, "PROFGATE_PASSWORD") {
			t.Fatalf("stderr = %q, want the two variables", msg)
		}
		if strings.Contains(f.stdout.String()+f.stderr.String()+f.verbose.String(), "hunter2-secret") {
			t.Fatal("the password was printed")
		}
		if got := f.saved.Contexts["prod"].Auth; got.Mode != "basic" || got.Issuer != "" {
			t.Fatalf("auth = %+v, want mode basic alone", got)
		}
		if len(f.rt.discovered) != 0 {
			t.Fatal("discovery ran under basic")
		}
	})

	t.Run("a prompt error is a usage error with no request", func(t *testing.T) {
		f := newLogin(t)
		f.rt.auth = pollStep{body: `{"mode":"basic"}`}
		f.basic = func() (string, string, error) { return "", "", errors.New("stdin is not a terminal") }
		_, err := f.login()
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "stdin is not a terminal") {
			t.Fatalf("err = %v", err)
		}
		if len(f.rt.whoamiReqs) != 0 || f.saved != nil {
			t.Fatal("a request was sent or the file was written")
		}
	})

	t.Run("the local refusals apply", func(t *testing.T) {
		f := newLogin(t)
		f.rt.auth = pollStep{body: `{"mode":"basic"}`}
		f.basic = func() (string, string, error) { return "a:b", "x", nil }
		_, err := f.login()
		if !errors.Is(err, ErrUsage) || len(f.rt.whoamiReqs) != 0 {
			t.Fatalf("err = %v, %d whoami requests", err, len(f.rt.whoamiReqs))
		}
	})

	t.Run("a 401 is the gateway's own", func(t *testing.T) {
		f := newLogin(t)
		f.rt.auth = pollStep{body: `{"mode":"basic"}`}
		f.rt.whoami = pollStep{status: http.StatusUnauthorized, body: `{"error":"authentication required","code":"unauthenticated"}`}
		f.basic = func() (string, string, error) { return "alice", "wrong", nil }
		_, err := f.login()
		var ae *APIError
		var d *AuthDiagnostic
		if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || errors.As(err, &d) {
			t.Fatalf("err = %v, want the plain 401", err)
		}
		if f.saved != nil {
			t.Fatal("the file was written")
		}
	})
}

func TestLoginDisabled(t *testing.T) {
	f := newLogin(t)
	f.rt.auth = pollStep{body: `{"mode":"disabled"}`}
	w, err := f.login()
	if err != nil {
		t.Fatal(err)
	}
	if w != (Whoami{}) {
		t.Fatalf("Whoami = %+v, want empty", w)
	}
	if out := f.stdout.String() + f.stderr.String(); !strings.Contains(out, "authenticates nobody") {
		t.Fatalf("output = %q", out)
	}
	if len(f.rt.whoamiReqs) != 0 || len(f.rt.discovered) != 0 {
		t.Fatal("a request beyond /v1/auth was sent")
	}
	if got := f.saved.Contexts["prod"].Auth; got.Mode != "disabled" || got.Issuer != "" {
		t.Fatalf("auth = %+v, want mode disabled alone", got)
	}
}

func TestLoginRewritesTheMode(t *testing.T) {
	cases := []struct {
		mode string
		auth string
	}{
		{"oidc", authOIDC("access", false)},
		{"basic", `{"mode":"basic"}`},
		{"disabled", `{"mode":"disabled"}`},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			f := newLogin(t)
			f.rt.auth = pollStep{body: tc.auth}
			f.settings.Context.Auth = AuthSnap{Mode: "basic"}
			if tc.mode == "basic" {
				f.settings.Context.Auth = AuthSnap{Mode: "disabled"}
			}
			f.file.Contexts["prod"] = f.settings.Context
			f.basic = func() (string, string, error) { return "alice", "pw", nil }
			if _, err := f.login(); err != nil {
				t.Fatal(err)
			}
			if got := f.saved.Contexts["prod"].Auth.Mode; got != tc.mode {
				t.Fatalf("mode = %q, want %q", got, tc.mode)
			}
		})
	}

	t.Run("an unknown mode is refused", func(t *testing.T) {
		f := newLogin(t)
		f.rt.auth = pollStep{body: `{"mode":"kerberos"}`}
		if _, err := f.login(); err == nil || !strings.Contains(err.Error(), "kerberos") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLogout(t *testing.T) {
	t.Run("a published endpoint gets one post, then the deletion", func(t *testing.T) {
		f := newLogin(t)
		if err := f.store.Write("prod", f.cached()); err != nil {
			t.Fatal(err)
		}
		f.rt.revokes = []pollStep{{status: http.StatusOK}}
		f.events = nil
		if err := f.logout(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.events, []string{"discovery", "revoke"}) {
			t.Fatalf("events = %v", f.events)
		}
		form := f.rt.revokeReqs[0]
		if form.Get("token") != "refresh-old" || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != "profgate-cli" {
			t.Fatalf("form = %v", form)
		}
		if _, ok := f.entry(); ok {
			t.Fatal("the entry remains")
		}
		f.assertNoSecretPrinted()
	})

	t.Run("no published endpoint is the deletion alone", func(t *testing.T) {
		f := newLogin(t)
		f.rt.noRevoke = true
		if err := f.store.Write("prod", f.cached()); err != nil {
			t.Fatal(err)
		}
		if err := f.logout(); err != nil {
			t.Fatal(err)
		}
		if len(f.rt.revokeReqs) != 0 {
			t.Fatal("a revocation was posted")
		}
		if _, ok := f.entry(); ok {
			t.Fatal("the entry remains")
		}
		if f.stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want nothing", f.stderr.String())
		}
		f.assertNoSecretPrinted()
	})

	t.Run("a revocation failure warns and still deletes", func(t *testing.T) {
		for name, step := range map[string]pollStep{
			"a 400":               {status: http.StatusBadRequest, body: `{"error":"unsupported_token_type"}`},
			"a transport failure": {fail: true},
		} {
			t.Run(name, func(t *testing.T) {
				f := newLogin(t)
				if err := f.store.Write("prod", f.cached()); err != nil {
					t.Fatal(err)
				}
				f.rt.revokes = []pollStep{step}
				if err := f.logout(); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(f.stderr.String(), "warning") {
					t.Fatalf("stderr = %q, want a warning", f.stderr.String())
				}
				if _, ok := f.entry(); ok {
					t.Fatal("the entry remains")
				}
				f.assertNoSecretPrinted()
			})
		}
	})

	t.Run("a deletion failure names the file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root deletes from a read-only directory")
		}
		f := newLogin(t)
		if err := f.store.Write("prod", f.cached()); err != nil {
			t.Fatal(err)
		}
		f.rt.revokes = []pollStep{{status: http.StatusOK}}
		// The lock is taken, then the directory turns read-only under it,
		// so the deletion is what fails.
		if err := os.WriteFile(f.lockPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		f.store.sleep = func(ctx context.Context, d time.Duration) error {
			if err := os.Remove(f.lockPath()); err != nil {
				t.Fatal(err)
			}
			f.store.sleep = f.clock.Sleep
			return f.clock.Sleep(ctx, d)
		}
		t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) }) //nolint:gosec // G302: the store's own directory mode
		f.rt.beforeRevoke = func() {
			if err := os.Chmod(f.dir, 0o500); err != nil { //nolint:gosec // G302: a read-only directory is what makes the deletion fail
				t.Fatal(err)
			}
		}
		err := f.logout()
		path := filepath.Join(f.dir, "prod.json")
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "remains") {
			t.Fatalf("err = %v, want one saying the credential remains at %s", err, path)
		}
		f.assertNoSecretPrinted()
	})

	t.Run("no entry says nothing was cached", func(t *testing.T) {
		f := newLogin(t)
		if err := f.logout(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.stdout.String(), "nothing") || !strings.Contains(f.stdout.String(), "prod") {
			t.Fatalf("stdout = %q", f.stdout.String())
		}
		if len(f.events) != 0 {
			t.Fatalf("events = %v, want none", f.events)
		}
	})
}

func TestLoginWritersDefault(t *testing.T) {
	f := newLogin(t)
	in := f.input()
	in.Stdout, in.Stderr = nil, nil
	if _, err := Login(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}
