package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// browserRecorder keeps what the browser flow reports: sessions minted, key
// snapshots, and file reloads.
type browserRecorder struct {
	metrics.Noop
	mu       sync.Mutex
	sessions int
	keys     [][]metrics.CookieKey
	reloads  []string
}

func (r *browserRecorder) AuthSessionIssued() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions++
}

func (r *browserRecorder) CookieKeys(keys []metrics.CookieKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, keys)
}

func (r *browserRecorder) AuthFileReload(file, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloads = append(r.reloads, file+"/"+result)
}

func (r *browserRecorder) sessionsIssued() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.sessions
}

func (r *browserRecorder) lastKeys() []metrics.CookieKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.keys) == 0 {
		return nil
	}

	return r.keys[len(r.keys)-1]
}

func (r *browserRecorder) fileReloads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.reloads...)
}

// lockedBuffer is a log sink the server goroutines and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

// browserFixture is one OIDC with the browser block behind a TLS gateway
// stand-in, against the test issuer, driven by a jar client that follows no
// redirect on its own.
type browserFixture struct {
	t       *testing.T
	issuer  *issuerFixture
	gateway *httptest.Server
	cfg     *config.Config
	o       *OIDC
	rec     *browserRecorder
	logs    *lockedBuffer
	keyPath string
	client  *http.Client
	jar     http.CookieJar
	// offset moves the gateway's clock; the issuer mints at real time.
	offset atomic.Int64
	// outcome is what ServeAuth last returned.
	mu      sync.Mutex
	outcome RouteOutcome
}

// mintIDToken is the token endpoint that answers the code with an ID token
// carrying the nonce the authorization endpoint saw; edit changes one claim.
func mintIDToken(t *testing.T, edit func(c map[string]any)) func(f *issuerFixture, w http.ResponseWriter, r *http.Request) {
	return func(f *issuerFixture, w http.ResponseWriter, _ *http.Request) {
		claims := baseClaims(time.Now(), func(c map[string]any) {
			c["iss"] = f.srv.URL
			c["nonce"] = f.authQuery.Get("nonce")
			if edit != nil {
				edit(c)
			}
		})
		token := mint(t, mintOpts{key: testKeys(t).rsa2048, kid: "k1", alg: jose.RS256, claims: claims})
		writeJSON(t, w, map[string]any{"id_token": token, "token_type": "Bearer"})
	}
}

// newBrowserFixture builds the fixture; edit changes the configuration before
// NewOIDC, and discover says whether Discover runs.
func newBrowserFixture(t *testing.T, edit func(fx *browserFixture), discover bool) *browserFixture {
	t.Helper()
	fx := &browserFixture{t: t, rec: &browserRecorder{}, logs: &lockedBuffer{}}
	fx.issuer = newIssuerFixture(t)
	fx.issuer.token = mintIDToken(t, nil)
	fx.keyPath = filepath.Join(t.TempDir(), "cookie.key")
	if err := os.WriteFile(fx.keyPath, keyFile(testKey(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.gateway = httptest.NewTLSServer(http.HandlerFunc(fx.serve))
	t.Cleanup(fx.gateway.Close)
	fx.cfg = oidcConfig(t, fx.issuer)
	fx.cfg.Auth.OIDC.Browser = &config.OIDCBrowser{
		ClientID:       testAudience,
		RedirectURL:    fx.gateway.URL + "/auth/callback",
		Scopes:         []string{"openid", "profile", "email"},
		CookieKeyFile:  fx.keyPath,
		SessionTTL:     8 * time.Hour,
		TransactionTTL: 5 * time.Minute,
	}
	if edit != nil {
		edit(fx)
	}
	o, err := NewOIDC(fx.cfg, OIDCOptions{
		Logger:   slog.New(slog.NewTextHandler(fx.logs, nil)),
		Recorder: fx.rec,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	fx.o = o
	fx.setClock()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.jar = jar
	fx.client = fx.gateway.Client()
	fx.client.Jar = jar
	fx.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if discover {
		if err := o.Discover(context.Background()); err != nil {
			t.Fatalf("Discover: %v", err)
		}
	}

	return fx
}

// setClock points every clock in the OIDC at the fixture's movable one.
func (fx *browserFixture) setClock() {
	now := func() time.Time { return time.Now().Add(time.Duration(fx.offset.Load())) }
	fx.o.now = now
	fx.o.verifier.now = now
	if fx.o.browser != nil {
		fx.o.browser.now = now
	}
}

// serve is the gateway stand-in: /auth/ goes to ServeAuth, everything else
// to Authenticate and a 200 naming the principal, or the failure's status.
func (fx *browserFixture) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/auth/") {
		out := fx.o.Routes().ServeAuth(w, r, fx.cfg)
		fx.mu.Lock()
		fx.outcome = out
		fx.mu.Unlock()

		return
	}
	p, err := fx.o.Authenticate(r.Context(), r, fx.cfg)
	if err != nil {
		var f *Failure
		if !errors.As(err, &f) {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		if f.ClearSession {
			DeleteSessionCookie(w)
		}
		if f.Redirect != "" {
			w.Header().Set("Location", f.Redirect)
			w.WriteHeader(http.StatusFound)

			return
		}
		w.WriteHeader(f.Status)

		return
	}
	_, _ = io.WriteString(w, p.Name+"/"+p.Realm)
}

func (fx *browserFixture) lastOutcome() RouteOutcome {
	fx.mu.Lock()
	defer fx.mu.Unlock()

	return fx.outcome
}

// reply is one response with its body already read and closed.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// get sends one request through the jar client and returns the reply.
func (fx *browserFixture) get(target string, headers map[string]string) *reply {
	fx.t.Helper()
	if !strings.HasPrefix(target, "https://") {
		target = fx.gateway.URL + target
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		fx.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := fx.client.Do(req)
	if err != nil {
		fx.t.Fatalf("GET %s: %v", target, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		fx.t.Fatal(err)
	}

	return &reply{status: resp.StatusCode, header: resp.Header, body: body}
}

// login starts a login for ret, follows the redirect to the issuer's
// authorization endpoint, and returns the callback URL the issuer sends the
// browser back to.
func (fx *browserFixture) login(ret string) string {
	fx.t.Helper()
	resp := fx.get("/auth/login?return="+url.QueryEscape(ret), nil)
	if resp.status != http.StatusFound {
		fx.t.Fatalf("login answered %d", resp.status)
	}
	authorize := resp.header.Get("Location")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, authorize, nil)
	if err != nil {
		fx.t.Fatal(err)
	}
	issuerClient := fx.issuer.srv.Client()
	issuerClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ir, err := issuerClient.Do(req)
	if err != nil {
		fx.t.Fatalf("authorization endpoint: %v", err)
	}
	_ = ir.Body.Close()
	if ir.StatusCode != http.StatusFound {
		fx.t.Fatalf("authorization endpoint answered %d", ir.StatusCode)
	}

	return ir.Header.Get("Location")
}

// cookie returns the jar's cookie of that name for the gateway, or nil.
func (fx *browserFixture) cookie(name string) *http.Cookie {
	fx.t.Helper()
	u, err := url.Parse(fx.gateway.URL)
	if err != nil {
		fx.t.Fatal(err)
	}
	for _, c := range fx.jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}

	return nil
}

// sessionRequest is a /v1 request carrying the jar's session cookie and the
// Sec-Fetch-Site named; "" leaves the header out.
func (fx *browserFixture) sessionRequest(site string) *http.Request {
	fx.t.Helper()
	c := fx.cookie(cookieSession)
	if c == nil {
		fx.t.Fatal("the jar holds no session cookie")
	}
	r := request("")
	r.AddCookie(c)
	if site != "" {
		r.Header.Set("Sec-Fetch-Site", site)
	}

	return r
}

// completeLogin logs alice in and leaves the session in the jar.
// The callback arrives cross-site, as a browser reports a chain that began
// at the issuer.
func (fx *browserFixture) completeLogin(ret string) *reply {
	fx.t.Helper()
	resp := fx.get(fx.login(ret), map[string]string{"Sec-Fetch-Site": "cross-site"})
	if resp.status != http.StatusOK {
		fx.t.Fatalf("callback answered %d, want 200", resp.status)
	}

	return resp
}

// assertLanding asserts the callback's landing page: a 200 HTML document
// that sends the browser to escaped, with no Location, no caching, and a
// policy that loads nothing.
func assertLanding(t *testing.T, resp *reply, escaped string) {
	t.Helper()
	if resp.status != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", resp.status, resp.body)
	}
	if loc := resp.header.Get("Location"); loc != "" {
		t.Fatalf("Location %q on the landing page", loc)
	}
	want := map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'",
	}
	for k, v := range want {
		if got := resp.header.Get(k); got != v {
			t.Fatalf("%s %q, want %q", k, got, v)
		}
	}
	body := string(resp.body)
	for _, fragment := range []string{
		`<meta http-equiv="refresh" content="0;url=` + escaped + `">`,
		`<a href="` + escaped + `">Continue</a>`,
		"Signed in. Redirecting…",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("landing page lacks %q:\n%s", fragment, body)
		}
	}
}

// setCookies parses the Set-Cookie headers of resp by name.
func setCookies(t *testing.T, resp *reply) map[string]*http.Cookie {
	t.Helper()
	out := map[string]*http.Cookie{}
	for _, line := range resp.header.Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		out[c.Name] = c
	}

	return out
}

// assertEnvelope asserts the standard error envelope with code and the fixed
// message, and no redirect.
func assertEnvelope(t *testing.T, resp *reply, status int, code string) {
	t.Helper()
	if resp.status != status {
		t.Fatalf("status %d, want %d; body %s", resp.status, status, resp.body)
	}
	var env struct{ Error, Code string }
	if err := json.Unmarshal(resp.body, &env); err != nil {
		t.Fatalf("body %q: %v", resp.body, err)
	}
	if env.Code != code || env.Error != "authentication required" {
		t.Fatalf("envelope %+v, want code %s and the fixed message", env, code)
	}
	if resp.header.Get("Location") != "" {
		t.Fatalf("Location %q on an error response", resp.header.Get("Location"))
	}
	if ct := resp.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
	if status == http.StatusServiceUnavailable && resp.header.Get("Retry-After") != "5" {
		t.Fatalf("Retry-After %q on a 503", resp.header.Get("Retry-After"))
	}
}

// navigate is the header set of a top-level browser navigation.
func navigate() map[string]string {
	return map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
}

func TestBrowserRedirect(t *testing.T) {
	ctx := context.Background()
	target := "/v1/namespaces/ns/services/svc/targets?x=1"
	rows := []struct {
		name     string
		headers  map[string]string
		redirect string
	}{
		{name: "navigation redirects", headers: navigate(), redirect: "/auth/login?return=%2Fv1%2Fnamespaces%2Fns%2Fservices%2Fsvc%2Ftargets%3Fx%3D1"},
		{name: "fetch gets 401", headers: map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}},
		{name: "no fetch metadata"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			fx := newBrowserFixture(t, nil, true)
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			_, err := fx.o.Authenticate(ctx, r, fx.cfg)
			wantFailure(t, err, http.StatusUnauthorized, ReasonMissing)
			var f *Failure
			if !errors.As(err, &f) {
				t.Fatal("not a Failure")
			}
			if f.Redirect != tc.redirect {
				t.Fatalf("Redirect = %q, want %q", f.Redirect, tc.redirect)
			}
			if f.ClearSession {
				t.Fatal("ClearSession set without a cookie")
			}
		})
	}
}

func TestBrowserLogin(t *testing.T) {
	t.Run("login sets txn", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		resp := fx.get("/auth/login?return=/v1/x", nil)
		if resp.status != http.StatusFound {
			t.Fatalf("status %d", resp.status)
		}
		loc, err := url.Parse(resp.header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if got := loc.Scheme + "://" + loc.Host + loc.Path; got != fx.issuer.srv.URL+"/auth" {
			t.Fatalf("redirected to %s, want the authorization endpoint", got)
		}
		q := loc.Query()
		want := map[string]string{
			"response_type":         "code",
			"client_id":             testAudience,
			"redirect_uri":          fx.gateway.URL + "/auth/callback",
			"scope":                 "openid profile email",
			"code_challenge_method": "S256",
		}
		for k, v := range want {
			if q.Get(k) != v {
				t.Fatalf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
		for _, k := range []string{"state", "nonce", "code_challenge"} {
			if len(q.Get(k)) != 43 {
				t.Fatalf("%s = %q, want 43 characters", k, q.Get(k))
			}
		}
		c := setCookies(t, resp)[cookieTxn]
		if c == nil || c.MaxAge != 300 {
			t.Fatalf("txn cookie %+v, want Max-Age=300", c)
		}
		assertCookieAttributes(t, c)
		if fx.cookie(cookieTxn) == nil {
			t.Fatal("the jar did not keep the transaction cookie")
		}
		if got := fx.lastOutcome(); got != (RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"}) {
			t.Fatalf("outcome %+v", got)
		}
	})

	t.Run("login vectors", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fixed := make([]byte, 96)
		for i := range fixed {
			fixed[i] = byte(i)
		}
		fx.o.browser.rand = bytes.NewReader(fixed)
		resp := fx.get("/auth/login", nil)
		loc, err := url.Parse(resp.header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := loc.Query()
		want := map[string]string{
			"state":          "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
			"nonce":          "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8",
			"code_challenge": "9F3IKnUj0Ul0zqBQ4wKOi_BqjuhVqdYwNOu0-57MIwU",
		}
		for k, v := range want {
			if q.Get(k) != v {
				t.Fatalf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
	})

	t.Run("login bad return", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		resp := fx.completeLogin("//evil.example")
		assertLanding(t, resp, "/")
	})

	t.Run("login entropy", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.o.browser.rand = errReader{}
		resp := fx.get("/auth/login?return=/v1/x", nil)
		assertEnvelope(t, resp, http.StatusServiceUnavailable, "auth_unavailable")
		if len(resp.header.Values("Set-Cookie")) != 0 {
			t.Fatalf("Set-Cookie %q after an entropy failure", resp.header.Values("Set-Cookie"))
		}
		if got := fx.lastOutcome(); got != (RouteOutcome{Status: 503, Code: "auth_unavailable", Reason: ReasonEntropy, Principal: "-"}) {
			t.Fatalf("outcome %+v", got)
		}
	})
}

func TestBrowserCallback(t *testing.T) {
	t.Run("callback ok", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		resp := fx.completeLogin("/v1/x")
		assertLanding(t, resp, "/v1/x")
		set := setCookies(t, resp)
		if s := set[cookieSession]; s == nil || s.MaxAge != 28800 {
			t.Fatalf("session cookie %+v, want Max-Age=28800", s)
		}
		assertCookieAttributes(t, set[cookieSession])
		if d := set[cookieTxn]; d == nil || d.Value != "" || d.MaxAge != -1 {
			t.Fatalf("txn cookie %+v, want a deletion", d)
		}
		if fx.cookie(cookieSession) == nil || fx.cookie(cookieTxn) != nil {
			t.Fatal("the jar must hold the session and no transaction cookie")
		}
		if fx.rec.sessionsIssued() != 1 {
			t.Fatalf("AuthSessionIssued called %d times", fx.rec.sessionsIssued())
		}
		if got := fx.lastOutcome(); got != (RouteOutcome{Status: 200, Code: "ok", Principal: "alice"}) {
			t.Fatalf("outcome %+v", got)
		}
		// callback then request: the navigation the landing page starts
		// comes from the gateway's own document
		r2 := fx.get("/v1/x", map[string]string{"Sec-Fetch-Site": "same-origin"})
		if r2.status != http.StatusOK || string(r2.body) != "alice/developer" {
			t.Fatalf("session request: %d %q", r2.status, r2.body)
		}
		// token endpoint request
		form := fx.issuer.exchanged()
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "c" ||
			form.Get("redirect_uri") != fx.gateway.URL+"/auth/callback" || form.Get("client_id") != testAudience {
			t.Fatalf("token form %v", form)
		}
		if challenge(form.Get("code_verifier")) != fx.issuer.authorized().Get("code_challenge") {
			t.Fatal("code_verifier does not match the code_challenge sent to the issuer")
		}
		if _, sent := form["client_secret"]; sent {
			t.Fatal("client_secret sent by a public client")
		}
	})

	t.Run("callback escapes the return", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		resp := fx.completeLogin(`/v1/x?a=<b>&c="d"`)
		assertLanding(t, resp, "/v1/x?a=&lt;b&gt;&amp;c=&#34;d&#34;")
		for _, raw := range []string{"<b>", `"d"`, "&c="} {
			if strings.Contains(string(resp.body), raw) {
				t.Fatalf("the landing page carries %q unescaped:\n%s", raw, resp.body)
			}
		}
	})

	t.Run("client secret", func(t *testing.T) {
		fx := newBrowserFixture(t, func(fx *browserFixture) {
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte("  s3cret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fx.cfg.Auth.OIDC.Browser.ClientSecretFile = path
		}, true)
		fx.completeLogin("/v1/x")
		if got := fx.issuer.exchanged().Get("client_secret"); got != "s3cret" {
			t.Fatalf("client_secret = %q", got)
		}
	})

	failures := []struct {
		name   string
		edit   func(fx *browserFixture)        // before NewOIDC
		before func(fx *browserFixture)        // between login and callback
		url    func(fx *browserFixture) string // the callback URL to fetch; nil fetches the one the issuer gave
		status int
		reason string
	}{
		{name: "no txn cookie", before: func(fx *browserFixture) {
			jar, _ := cookiejar.New(nil)
			fx.client.Jar = jar
		}, status: 401, reason: ReasonState},
		{name: "expired txn", before: func(fx *browserFixture) { fx.offset.Store(int64(6 * time.Minute)) }, status: 401, reason: ReasonState},
		{name: "wrong state", url: func(fx *browserFixture) string { return fx.gateway.URL + "/auth/callback?code=c&state=other" }, status: 401, reason: ReasonState},
		{name: "issuer error", url: func(fx *browserFixture) string {
			return fx.gateway.URL + "/auth/callback?error=access_denied&state=" + url.QueryEscape(fx.issuer.authorized().Get("state"))
		}, status: 401, reason: ReasonIssuerDenied},
		{name: "exchange 400", edit: func(fx *browserFixture) { fx.issuer.token = nil }, status: 401, reason: ReasonExchangeDenied},
		{name: "exchange 500", edit: func(fx *browserFixture) {
			fx.issuer.token = func(_ *issuerFixture, w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 500) }
		}, status: 503, reason: ReasonExchange},
		{name: "exchange unreachable", before: func(fx *browserFixture) { fx.issuer.srv.Close() }, status: 503, reason: ReasonExchange},
		{name: "exchange redirect", edit: func(fx *browserFixture) {
			fx.issuer.token = func(f *issuerFixture, w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, f.srv.URL+"/elsewhere", http.StatusTemporaryRedirect)
			}
		}, status: 503, reason: ReasonExchange},
		{name: "no id_token", edit: func(fx *browserFixture) {
			fx.issuer.token = func(_ *issuerFixture, w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"access_token": "x"})
			}
		}, status: 401, reason: ReasonExchangeDenied},
		{name: "id_token not a string", edit: func(fx *browserFixture) {
			fx.issuer.token = func(_ *issuerFixture, w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"id_token": 7})
			}
		}, status: 401, reason: ReasonExchangeDenied},
		{name: "nonce mismatch", edit: func(fx *browserFixture) {
			fx.issuer.token = mintIDToken(t, func(c map[string]any) { c["nonce"] = "other" })
		}, status: 401, reason: ReasonNonce},
		{name: "bad id token", edit: func(fx *browserFixture) {
			fx.issuer.token = mintIDToken(t, func(c map[string]any) { c["aud"] = "other" })
		}, status: 401, reason: ReasonAudience},
		{name: "stale keys at callback", before: func(fx *browserFixture) {
			fx.issuer.set(func(f *issuerFixture) { f.keysStatus = 500 })
			fx.offset.Store(int64(25 * time.Hour))
		}, status: 503, reason: ReasonKeysStale},
		{name: "session entropy", before: func(fx *browserFixture) { fx.o.browser.sealer.rand = errReader{} }, status: 503, reason: ReasonEntropy},
		{name: "no realm", edit: func(fx *browserFixture) {
			fx.issuer.token = mintIDToken(t, func(c map[string]any) { c["sub"] = "mallory" })
		}, status: 401, reason: ReasonNoRealm},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			fx := newBrowserFixture(t, tc.edit, true)
			callback := fx.login("/v1/x")
			if tc.name == "stale keys at callback" {
				// The transaction must be sealed at the moved clock, so log in again after moving it.
				tc.before(fx)
				callback = fx.login("/v1/x")
			} else if tc.before != nil {
				tc.before(fx)
			}
			if tc.url != nil {
				callback = tc.url(fx)
			}
			resp := fx.get(callback, nil)
			code := "unauthenticated"
			if tc.status == 503 {
				code = "auth_unavailable"
			}
			assertEnvelope(t, resp, tc.status, code)
			set := setCookies(t, resp)
			if d := set[cookieTxn]; d == nil || d.Value != "" || d.MaxAge != -1 {
				t.Fatalf("txn cookie %+v, want a deletion", d)
			}
			if set[cookieSession] != nil {
				t.Fatal("a session cookie was set on a failed callback")
			}
			if fx.rec.sessionsIssued() != 0 {
				t.Fatal("AuthSessionIssued called on a failed callback")
			}
			if got := fx.lastOutcome(); got != (RouteOutcome{Status: tc.status, Code: code, Reason: tc.reason, Principal: "-"}) {
				t.Fatalf("outcome %+v, want %d %s %s", got, tc.status, code, tc.reason)
			}
		})
	}

	t.Run("issuer error is logged, not echoed", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.login("/v1/x")
		resp := fx.get(fx.gateway.URL+"/auth/callback?error=access_denied&state="+url.QueryEscape(fx.issuer.authorized().Get("state")), nil)
		if strings.Contains(string(resp.body), "access_denied") {
			t.Fatalf("body echoes the issuer's error: %s", resp.body)
		}
		if !strings.Contains(fx.logs.String(), "access_denied") {
			t.Fatalf("log does not carry the issuer's error:\n%s", fx.logs.String())
		}
	})

	t.Run("exchange redirect target saw nothing", func(t *testing.T) {
		var hits atomic.Int32
		target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		t.Cleanup(target.Close)
		fx := newBrowserFixture(t, func(fx *browserFixture) {
			fx.issuer.token = func(_ *issuerFixture, w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/token", http.StatusTemporaryRedirect)
			}
		}, true)
		resp := fx.get(fx.login("/v1/x"), nil)
		assertEnvelope(t, resp, http.StatusServiceUnavailable, "auth_unavailable")
		if hits.Load() != 0 {
			t.Fatalf("the redirect target saw %d requests", hits.Load())
		}
	})
}

func TestBrowserSession(t *testing.T) {
	ctx := context.Background()

	t.Run("session csrf", func(t *testing.T) {
		for _, site := range []string{"cross-site", "same-site", ""} {
			fx := newBrowserFixture(t, nil, true)
			fx.completeLogin("/v1/x")
			_, err := fx.o.Authenticate(ctx, fx.sessionRequest(site), fx.cfg)
			wantFailure(t, err, http.StatusUnauthorized, ReasonCSRF)
		}
	})

	t.Run("session admitted", func(t *testing.T) {
		for _, site := range []string{"same-origin", "none"} {
			fx := newBrowserFixture(t, nil, true)
			fx.completeLogin("/v1/x")
			p, err := fx.o.Authenticate(ctx, fx.sessionRequest(site), fx.cfg)
			wantPrincipal(t, p, err, "alice", "developer")
		}
	})

	t.Run("session expired", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		fx.offset.Store(int64(8*time.Hour + time.Second))
		_, err := fx.o.Authenticate(ctx, fx.sessionRequest("none"), fx.cfg)
		wantFailure(t, err, http.StatusUnauthorized, ReasonSession)
		var f *Failure
		errors.As(err, &f)
		if !f.ClearSession || f.Redirect != "" {
			t.Fatalf("Failure %+v, want ClearSession and no Redirect without navigation headers", f)
		}
		r := fx.sessionRequest("none")
		for k, v := range navigate() {
			r.Header.Set(k, v)
		}
		_, err = fx.o.Authenticate(ctx, r, fx.cfg)
		errors.As(err, &f)
		if !f.ClearSession || f.Redirect != "/auth/login?return=%2Fv1%2Fx" {
			t.Fatalf("Failure %+v, want ClearSession and a login redirect", f)
		}
	})

	t.Run("expired session leaves the jar", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		fx.offset.Store(int64(8*time.Hour + time.Second))
		resp := fx.get("/v1/x", map[string]string{"Sec-Fetch-Site": "none"})
		if resp.status != http.StatusUnauthorized {
			t.Fatalf("status %d", resp.status)
		}
		if d := setCookies(t, resp)[cookieSession]; d == nil || d.MaxAge != -1 {
			t.Fatalf("session cookie %+v, want a deletion", d)
		}
		if fx.cookie(cookieSession) != nil {
			t.Fatal("the jar still holds the session cookie")
		}
	})

	t.Run("session tampered", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		r := request("")
		raw, err := base64.RawURLEncoding.DecodeString(fx.cookie(cookieSession).Value)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)/2] ^= 0x01
		r.AddCookie(&http.Cookie{Name: cookieSession, Value: base64.RawURLEncoding.EncodeToString(raw)}) //nolint:gosec // a request cookie carries no attributes
		r.Header.Set("Sec-Fetch-Site", "none")
		_, err = fx.o.Authenticate(ctx, r, fx.cfg)
		wantFailure(t, err, http.StatusUnauthorized, ReasonSession)
		var f *Failure
		errors.As(err, &f)
		if !f.ClearSession {
			t.Fatal("ClearSession not set for a tampered cookie")
		}
	})

	t.Run("session from previous key", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		if err := os.WriteFile(fx.keyPath, keyFile(testKey(2), testKey(1)), 0o600); err != nil {
			t.Fatal(err)
		}
		fx.o.browser.keyFile.Poll()
		p, err := fx.o.Authenticate(ctx, fx.sessionRequest("none"), fx.cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})

	t.Run("bearer beats session", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		r := fx.sessionRequest("cross-site")
		r.Header.Set("Authorization", bearer(t, fx.issuer, "k1", "alice"))
		p, err := fx.o.Authenticate(ctx, r, fx.cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})
}

func TestBrowserLogout(t *testing.T) {
	t.Run("logout with end_session", func(t *testing.T) {
		fx := newBrowserFixture(t, func(fx *browserFixture) { fx.issuer.endSession = fx.issuer.srv.URL + "/logout" }, true)
		fx.completeLogin("/v1/x")
		resp := fx.get("/auth/logout", nil)
		if resp.status != http.StatusFound {
			t.Fatalf("status %d", resp.status)
		}
		loc, err := url.Parse(resp.header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		gw, _ := url.Parse(fx.gateway.URL)
		if loc.Scheme+"://"+loc.Host+loc.Path != fx.issuer.srv.URL+"/logout" ||
			loc.Query().Get("post_logout_redirect_uri") != "https://"+gw.Host+"/" ||
			loc.Query().Get("client_id") != testAudience {
			t.Fatalf("Location %s", loc)
		}
		if d := setCookies(t, resp)[cookieSession]; d == nil || d.MaxAge != -1 {
			t.Fatalf("session cookie %+v, want a deletion", d)
		}
		if fx.cookie(cookieSession) != nil {
			t.Fatal("the jar still holds the session cookie")
		}
		if got := fx.lastOutcome(); got != (RouteOutcome{Status: 302, Code: "auth_redirect", Principal: "-"}) {
			t.Fatalf("outcome %+v", got)
		}
	})

	t.Run("logout without end_session", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		resp := fx.get("/auth/logout", nil)
		if resp.status != http.StatusFound || resp.header.Get("Location") != "/" {
			t.Fatalf("%d to %q, want 302 to /", resp.status, resp.header.Get("Location"))
		}
		if fx.cookie(cookieSession) != nil {
			t.Fatal("the jar still holds the session cookie")
		}
	})

	t.Run("logout without session", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		resp := fx.get("/auth/logout", nil)
		if resp.status != http.StatusFound {
			t.Fatalf("status %d", resp.status)
		}
		if d := setCookies(t, resp)[cookieSession]; d == nil || d.MaxAge != -1 {
			t.Fatalf("session cookie %+v, want a deletion", d)
		}
	})
}

func TestBrowserKeyFile(t *testing.T) {
	ctx := context.Background()

	t.Run("startup records the keys", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		want := []metrics.CookieKey{{Fingerprint: fingerprint(testKey(1)), Role: "current"}}
		if got := fx.rec.lastKeys(); len(got) != 1 || got[0] != want[0] {
			t.Fatalf("CookieKeys = %v, want %v", got, want)
		}
	})

	t.Run("key rotation while running", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		fx.o.browser.keyFile.read = sequenceReader(t, keyFile(testKey(2), testKey(1)))
		fx.o.browser.keyFile.Poll()
		want := []metrics.CookieKey{
			{Fingerprint: fingerprint(testKey(2)), Role: "current"},
			{Fingerprint: fingerprint(testKey(1)), Role: "previous"},
		}
		if got := fx.rec.lastKeys(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("CookieKeys = %v, want %v", got, want)
		}
		p, err := fx.o.Authenticate(ctx, fx.sessionRequest("none"), fx.cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})

	t.Run("key file bad", func(t *testing.T) {
		fx := newBrowserFixture(t, nil, true)
		fx.completeLogin("/v1/x")
		fx.o.browser.keyFile.read = sequenceReader(t, keyFile(testKey(1), testKey(2), testKey(3)))
		fx.o.browser.keyFile.Poll()
		reloads := fx.rec.fileReloads()
		if len(reloads) == 0 || reloads[len(reloads)-1] != "cookie_key/failed" {
			t.Fatalf("reloads %v, want a failed cookie_key reload last", reloads)
		}
		if len(fx.rec.lastKeys()) != 1 {
			t.Fatal("a rejected key file replaced the reported keys")
		}
		p, err := fx.o.Authenticate(ctx, fx.sessionRequest("none"), fx.cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})

	t.Run("unreadable key file fails construction", func(t *testing.T) {
		f := newIssuerFixture(t)
		cfg := oidcConfig(t, f)
		cfg.Auth.OIDC.Browser = &config.OIDCBrowser{
			ClientID: testAudience, RedirectURL: "https://gw.example/auth/callback",
			CookieKeyFile: filepath.Join(t.TempDir(), "missing"), SessionTTL: time.Hour, TransactionTTL: time.Minute,
		}
		if _, err := NewOIDC(cfg, OIDCOptions{}); err == nil {
			t.Fatal("NewOIDC accepted an unreadable cookie key file")
		}
	})
}

func TestBrowserRoutes(t *testing.T) {
	t.Run("routes without browser", func(t *testing.T) {
		f := newIssuerFixture(t)
		if routes := newTestOIDC(t, oidcConfig(t, f)).Routes(); routes != nil {
			t.Fatalf("Routes() = %v without the browser block", routes)
		}
	})

	notReady := func(t *testing.T, fx *browserFixture) {
		t.Helper()
		for _, path := range []string{"/auth/login", "/auth/logout"} {
			resp := fx.get(path, nil)
			if resp.status != http.StatusServiceUnavailable {
				t.Fatalf("%s: status %d", path, resp.status)
			}
			var env struct{ Code string }
			if err := json.Unmarshal(resp.body, &env); err != nil || env.Code != "not_ready" {
				t.Fatalf("%s: body %s", path, resp.body)
			}
			if resp.header.Get("Location") != "" || len(resp.header.Values("Set-Cookie")) != 0 {
				t.Fatalf("%s: a redirect or cookie on a not-ready answer", path)
			}
			if got := fx.lastOutcome(); got != (RouteOutcome{Status: 503, Code: "not_ready", Principal: "-"}) {
				t.Fatalf("%s: outcome %+v", path, got)
			}
		}
	}

	t.Run("routes before discovery", func(t *testing.T) {
		notReady(t, newBrowserFixture(t, nil, false))
	})

	t.Run("routes after a failed key fetch", func(t *testing.T) {
		fx := newBrowserFixture(t, func(fx *browserFixture) {
			fx.issuer.set(func(f *issuerFixture) { f.keysStatus = 500 })
		}, false)
		if err := fx.o.Discover(context.Background()); err == nil {
			t.Fatal("Discover succeeded while the keys answered 500")
		}
		notReady(t, fx)
	})
}

// TestEnvelopeBytes pins the bytes the routes write for an error to the
// envelope internal/httpapi writes, so the two stay byte-identical.
func TestEnvelopeBytes(t *testing.T) {
	w := httptest.NewRecorder()
	writeEnvelope(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
	want := "{\"error\":\"authentication required\",\"code\":\"unauthenticated\"}\n"
	if got := w.Body.String(); got != want {
		t.Fatalf("body %q, want %q", got, want)
	}
	if ct, cc := w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"); ct != "application/json" || cc != "no-store" {
		t.Fatalf("headers %q %q", ct, cc)
	}
}
