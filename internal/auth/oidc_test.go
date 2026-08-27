package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/arloliu/profgate/internal/config"
)

// issuerFixture is an httptest issuer whose discovery and key answers the
// test changes between calls.
type issuerFixture struct {
	srv             *httptest.Server
	mu              sync.Mutex
	discoveryStatus int
	keysStatus      int
	keys            jose.JSONWebKeySet
	jwksPath        string // the only path that serves keys; discovery advertises it
	endSession      string // published as end_session_endpoint when set
	tokenCalls      int
	// authQuery is the query the authorization endpoint last received;
	// tokenForm is the form the token endpoint last received.
	authQuery, tokenForm url.Values
	// token answers the token endpoint when set; nil answers 400.
	token func(f *issuerFixture, w http.ResponseWriter, r *http.Request)
}

func newIssuerFixture(t *testing.T) *issuerFixture {
	t.Helper()
	f := &issuerFixture{
		discoveryStatus: http.StatusOK,
		keysStatus:      http.StatusOK,
		keys:            rsaSet(t),
		jwksPath:        "/keys",
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case wellKnown:
			if f.discoveryStatus != http.StatusOK {
				http.Error(w, "down", f.discoveryStatus)

				return
			}
			doc := map[string]any{
				"issuer":                 f.srv.URL,
				"jwks_uri":               f.srv.URL + f.jwksPath,
				"authorization_endpoint": f.srv.URL + "/auth",
				"token_endpoint":         f.srv.URL + "/token",
			}
			if f.endSession != "" {
				doc["end_session_endpoint"] = f.endSession
			}
			writeJSON(t, w, doc)
		case f.jwksPath:
			if f.keysStatus != http.StatusOK {
				http.Error(w, "down", f.keysStatus)

				return
			}
			writeJSON(t, w, f.keys)
		case "/auth":
			// The authorization endpoint of a test issuer: record what the
			// gateway asked for and send the browser back with a code.
			f.authQuery = r.URL.Query()
			redirect := f.authQuery.Get("redirect_uri") + "?code=c&state=" + url.QueryEscape(f.authQuery.Get("state"))
			http.Redirect(w, r, redirect, http.StatusFound)
		case "/token":
			f.tokenCalls++
			if err := r.ParseForm(); err != nil {
				t.Errorf("token form: %v", err)
			}
			f.tokenForm = r.PostForm
			if f.token != nil {
				f.token(f, w, r)

				return
			}
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *issuerFixture) set(edit func(f *issuerFixture)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f)
}

// authorized is the query the authorization endpoint last received.
func (f *issuerFixture) authorized() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.authQuery
}

// exchanged is the form the token endpoint last received.
func (f *issuerFixture) exchanged() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.tokenForm
}

func (f *issuerFixture) tokenRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.tokenCalls
}

// oidcConfig is a valid oidc configuration against the fixture's issuer that
// maps sub alice to the developer realm.
func oidcConfig(t *testing.T, f *issuerFixture) *config.Config {
	t.Helper()

	return &config.Config{
		Auth: config.AuthConfig{
			Mode: "oidc",
			OIDC: &config.OIDCConfig{
				Issuer:         f.srv.URL,
				Audience:       testAudience,
				TokenType:      "id",
				UsernameClaim:  "sub",
				GroupsClaim:    "groups",
				CAFile:         caFile(t, f.srv),
				ClockSkew:      30 * time.Second,
				JWKSRefresh:    time.Hour,
				JWKSRefreshMin: time.Minute,
				JWKSMaxStale:   24 * time.Hour,
				Mapping: config.OIDCMapping{
					Users: []config.OIDCMappingEntry{{Name: "alice", Realm: "developer"}},
				},
			},
		},
		Realms: map[string]config.Realm{"developer": {}},
	}
}

func newTestOIDC(t *testing.T, cfg *config.Config) *OIDC {
	t.Helper()
	o, err := NewOIDC(cfg, OIDCOptions{})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}

	return o
}

// bearer mints a valid ID token for sub against the fixture's issuer under
// kid, signed with the package RSA key.
func bearer(t *testing.T, f *issuerFixture, kid, sub string) string {
	t.Helper()
	claims := baseClaims(time.Now(), func(c map[string]any) {
		c["iss"] = f.srv.URL
		c["sub"] = sub
	})

	return "Bearer " + mint(t, mintOpts{key: testKeys(t).rsa2048, kid: kid, alg: jose.RS256, claims: claims})
}

func TestOIDCAuthenticate(t *testing.T) {
	ctx := context.Background()
	rows := []struct {
		name   string
		req    func(t *testing.T, f *issuerFixture) *http.Request
		cfg    func(cfg *config.Config)
		status int
		reason string // "" means admitted as alice in developer
	}{
		{name: "bearer ok", req: func(t *testing.T, f *issuerFixture) *http.Request { return request(bearer(t, f, "k1", "alice")) }},
		{name: "no credential", req: func(*testing.T, *issuerFixture) *http.Request { return request("") },
			status: http.StatusUnauthorized, reason: ReasonMissing},
		{name: "wrong scheme", req: func(*testing.T, *issuerFixture) *http.Request { return request(basicHeader("alice", "pw")) },
			status: http.StatusUnauthorized, reason: ReasonScheme},
		{name: "bearer wins", req: func(t *testing.T, f *issuerFixture) *http.Request {
			r := request(bearer(t, f, "k1", "alice"))
			r.Header.Set("Cookie", "__Host-profgate_session=garbage")

			return r
		}},
		{name: "no realm", req: func(t *testing.T, f *issuerFixture) *http.Request { return request(bearer(t, f, "k1", "mallory")) },
			status: http.StatusUnauthorized, reason: ReasonNoRealm},
		{name: "navigation without browser block", req: func(*testing.T, *issuerFixture) *http.Request {
			r := request("")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")

			return r
		}, status: http.StatusUnauthorized, reason: ReasonMissing},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssuerFixture(t)
			cfg := oidcConfig(t, f)
			if tc.cfg != nil {
				tc.cfg(cfg)
			}
			o := newTestOIDC(t, cfg)
			if err := o.Discover(ctx); err != nil {
				t.Fatalf("Discover: %v", err)
			}
			p, err := o.Authenticate(ctx, tc.req(t, f), cfg)
			if tc.reason == "" {
				wantPrincipal(t, p, err, "alice", "developer")

				return
			}
			wantFailure(t, err, tc.status, tc.reason)
			var fail *Failure
			if errors.As(err, &fail) && fail.Redirect != "" {
				t.Fatalf("Redirect = %q without the browser block", fail.Redirect)
			}
		})
	}
}

func TestOIDCDiscover(t *testing.T) {
	ctx := context.Background()

	t.Run("discover fails", func(t *testing.T) {
		f := newIssuerFixture(t)
		f.set(func(f *issuerFixture) { f.discoveryStatus = http.StatusInternalServerError })
		cfg := oidcConfig(t, f)
		o := newTestOIDC(t, cfg)
		if err := o.Discover(ctx); err == nil {
			t.Fatal("Discover succeeded against a 500")
		}
		if o.state.Load() != nil {
			t.Fatal("state published after a failed discovery")
		}
		_, err := o.Authenticate(ctx, request(bearer(t, f, "k1", "alice")), cfg)
		wantFailure(t, err, http.StatusServiceUnavailable, ReasonKeysStale)
	})

	t.Run("discover requires a key", func(t *testing.T) {
		f := newIssuerFixture(t)
		f.set(func(f *issuerFixture) { f.keys = jose.JSONWebKeySet{} })
		o := newTestOIDC(t, oidcConfig(t, f))
		if err := o.Discover(ctx); err == nil {
			t.Fatal("Discover succeeded with an empty key set")
		}
		if o.state.Load() != nil {
			t.Fatal("state published without a usable key")
		}
	})

	t.Run("discovery ok, keys fail, retry", func(t *testing.T) {
		f := newIssuerFixture(t)
		f.set(func(f *issuerFixture) { f.keysStatus = http.StatusInternalServerError })
		cfg := oidcConfig(t, f)
		o := newTestOIDC(t, cfg)
		if err := o.Discover(ctx); err == nil {
			t.Fatal("Discover succeeded while the keys answered 500")
		}
		if o.state.Load() != nil {
			t.Fatal("state published with a good document and a bad key set")
		}
		tok := bearer(t, f, "k1", "alice")
		_, err := o.Authenticate(ctx, request(tok), cfg)
		wantFailure(t, err, http.StatusServiceUnavailable, ReasonKeysStale)
		if n := f.tokenRequests(); n != 0 {
			t.Fatalf("%d requests reached the token endpoint", n)
		}
		f.set(func(f *issuerFixture) { f.keysStatus = http.StatusOK })
		if err := o.Discover(ctx); err != nil {
			t.Fatalf("second Discover: %v", err)
		}
		st := o.state.Load()
		if st == nil || st.doc.JWKSURI != f.srv.URL+"/keys" || st.keys.current() == nil {
			t.Fatalf("state = %+v, want the document and a fetched key set together", st)
		}
		p, err := o.Authenticate(ctx, request(tok), cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})

	t.Run("retry replaces the state", func(t *testing.T) {
		f := newIssuerFixture(t)
		cfg := oidcConfig(t, f)
		o := newTestOIDC(t, cfg)
		if err := o.Discover(ctx); err != nil {
			t.Fatalf("Discover: %v", err)
		}
		old := o.state.Load()
		f.set(func(f *issuerFixture) {
			f.jwksPath = "/keys-v2"
			f.keys = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &testKeys(t).rsa2048.PublicKey, KeyID: "k2"}}}
		})
		if err := o.Discover(ctx); err != nil {
			t.Fatalf("second Discover: %v", err)
		}
		st := o.state.Load()
		if st == old || st.keys == old.keys {
			t.Fatal("second Discover did not replace the state and its key cache")
		}
		if st.doc.JWKSURI != f.srv.URL+"/keys-v2" {
			t.Fatalf("jwks_uri = %q, want the new one", st.doc.JWKSURI)
		}
		p, err := o.Authenticate(ctx, request(bearer(t, f, "k2", "alice")), cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})
}

func TestNewOIDCRequiresBlock(t *testing.T) {
	if _, err := NewOIDC(&config.Config{}, OIDCOptions{}); err == nil {
		t.Fatal("NewOIDC accepted a configuration without auth.oidc")
	}
}
