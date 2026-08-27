package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testIssuer   = "https://issuer.example"
	testAudience = "profgate"
)

// mintOpts is one token: the private key, the header, and the claims.
type mintOpts struct {
	key    any
	kid    string
	alg    jose.SignatureAlgorithm
	typ    string
	claims map[string]any
}

// mint signs claims with go-jose and returns the compact token.
func mint(t *testing.T, o mintOpts) string {
	t.Helper()
	key := o.key
	if o.kid != "" {
		key = &jose.JSONWebKey{Key: o.key, KeyID: o.kid}
	}
	opts := &jose.SignerOptions{}
	if o.typ != "" {
		opts.WithType(jose.ContentType(o.typ))
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: o.alg, Key: key}, opts)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	payload, err := json.Marshal(o.claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("CompactSerialize: %v", err)
	}

	return token
}

// baseClaims is a valid ID token payload at now for sub alice; edit changes
// one thing per row.
func baseClaims(now time.Time, edit func(c map[string]any)) map[string]any {
	c := map[string]any{
		"iss": testIssuer,
		"sub": "alice",
		"aud": testAudience,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	if edit != nil {
		edit(c)
	}

	return c
}

// verifyFixture is a verifier over a key cache that has fetched once.
type verifyFixture struct {
	*jwksFixture
	state atomic.Pointer[issuerState]
	v     *verifier
}

// newVerifyFixture builds the cache over set, refreshes it once, publishes an
// issuerState, and points a verifier for the ID token profile at it.
func newVerifyFixture(t *testing.T, set jose.JSONWebKeySet) *verifyFixture {
	t.Helper()
	f := &verifyFixture{jwksFixture: newJWKSFixture(t, set)}
	if err := f.cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	f.state.Store(&issuerState{keys: f.cache})
	f.v = &verifier{
		issuer:        testIssuer,
		audience:      testAudience,
		tokenType:     "id",
		usernameClaim: "sub",
		groupsClaim:   "groups",
		skew:          30 * time.Second,
		state:         &f.state,
		now:           f.clock,
	}

	return f
}

// rsaSet holds the package RSA key under kid k1.
func rsaSet(t *testing.T) jose.JSONWebKeySet {
	t.Helper()

	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &testKeys(t).rsa2048.PublicKey, KeyID: "k1"}}}
}

// wantVerifyFailure asserts f names the status and reason.
func wantVerifyFailure(t *testing.T, f *Failure, status int, reason string) {
	t.Helper()
	if f == nil {
		t.Fatalf("verify succeeded, want %d %s", status, reason)
	}
	if f.Status != status || f.Reason != reason {
		t.Fatalf("Failure = %d %s, want %d %s", f.Status, f.Reason, status, reason)
	}
}

// splitToken returns the three compact segments.
func splitToken(t *testing.T, token string) (header, payload, sig string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}

	return parts[0], parts[1], parts[2]
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestVerifyShape(t *testing.T) {
	keys := testKeys(t)
	ctx := context.Background()
	type row struct {
		name   string
		token  func(t *testing.T, f *verifyFixture) string
		status int
		reason string // "" means the token verifies
	}
	rsaToken := func(f *verifyFixture, edit func(c map[string]any)) string {
		return mint(t, mintOpts{key: keys.rsa2048, kid: "k1", alg: jose.RS256, claims: baseClaims(f.clock(), edit)})
	}
	rows := []row{
		{"valid id token", func(_ *testing.T, f *verifyFixture) string { return rsaToken(f, nil) }, 0, ""},
		{"oversize", func(_ *testing.T, _ *verifyFixture) string { return strings.Repeat("a", 17<<10) },
			http.StatusUnauthorized, ReasonMalformed},
		{"not compact", func(_ *testing.T, _ *verifyFixture) string { return "a.b" }, http.StatusUnauthorized, ReasonMalformed},
		{"flattened JSON JWS", func(t *testing.T, f *verifyFixture) string {
			h, p, s := splitToken(t, rsaToken(f, nil))

			return `{"payload":"` + p + `","protected":"` + h + `","signature":"` + s + `"}`
		}, http.StatusUnauthorized, ReasonMalformed},
		{"general JSON JWS", func(t *testing.T, f *verifyFixture) string {
			h, p, s := splitToken(t, rsaToken(f, nil))

			return `{"payload":"` + p + `","signatures":[{"protected":"` + h + `","signature":"` + s + `"}]}`
		}, http.StatusUnauthorized, ReasonMalformed},
		{"alg not a string", func(t *testing.T, f *verifyFixture) string {
			_, p, s := splitToken(t, rsaToken(f, nil))

			return b64(`{"alg":1,"kid":"k1"}`) + "." + p + "." + s
		}, http.StatusUnauthorized, ReasonMalformed},
		{"alg twice", func(t *testing.T, f *verifyFixture) string {
			_, p, s := splitToken(t, rsaToken(f, nil))

			return b64(`{"alg":"RS256","alg":"RS256","kid":"k1"}`) + "." + p + "." + s
		}, http.StatusUnauthorized, ReasonMalformed},
		{"alg none", func(t *testing.T, f *verifyFixture) string {
			_, p, _ := splitToken(t, rsaToken(f, nil))

			return b64(`{"alg":"none"}`) + "." + p + "."
		}, http.StatusUnauthorized, ReasonAlg},
		{"alg HS256", func(_ *testing.T, f *verifyFixture) string {
			return mint(t, mintOpts{key: []byte("0123456789abcdef0123456789abcdef"), alg: jose.HS256, claims: baseClaims(f.clock(), nil)})
		}, http.StatusUnauthorized, ReasonAlg},
		{"bad signature", func(t *testing.T, f *verifyFixture) string {
			h, _, s := splitToken(t, rsaToken(f, nil))
			altered, _ := json.Marshal(baseClaims(f.clock(), func(c map[string]any) { c["sub"] = "mallory" }))

			return h + "." + base64.RawURLEncoding.EncodeToString(altered) + "." + s
		}, http.StatusUnauthorized, ReasonSignature},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t, rsaSet(t))
			before := f.fetch.count()
			c, fail := f.v.verify(ctx, tc.token(t, f))
			if tc.reason == "" {
				if fail != nil {
					t.Fatalf("verify: %v", fail)
				}
				if c.Username != "alice" || c.Subject != "alice" {
					t.Fatalf("claims = %+v, want Username == sub == alice", c)
				}

				return
			}
			wantVerifyFailure(t, fail, tc.status, tc.reason)
			if got := f.fetch.count(); got != before {
				t.Fatalf("fetcher called %d times during a shape rejection", got-before)
			}
		})
	}
}

func TestVerifyStaleKeys(t *testing.T) {
	keys := testKeys(t)
	ctx := context.Background()
	stale := func(t *testing.T) *verifyFixture {
		t.Helper()
		f := newVerifyFixture(t, rsaSet(t))
		f.advance(25 * time.Hour)

		return f
	}
	token := func(f *verifyFixture) string {
		return mint(t, mintOpts{key: keys.rsa2048, kid: "k1", alg: jose.RS256, claims: baseClaims(f.clock(), nil)})
	}

	t.Run("stale after alg", func(t *testing.T) {
		f := stale(t)
		f.fetch.program(jose.JSONWebKeySet{}, errors.New("issuer down"))
		_, fail := f.v.verify(ctx, token(f))
		wantVerifyFailure(t, fail, http.StatusServiceUnavailable, ReasonKeysStale)
		if got := f.fetch.count(); got != 2 {
			t.Fatalf("fetcher called %d times, want the initial fetch plus one on-demand attempt", got)
		}
	})
	t.Run("stale still 401 for alg none", func(t *testing.T) {
		f := stale(t)
		_, p, _ := splitToken(t, token(f))
		_, fail := f.v.verify(ctx, b64(`{"alg":"none"}`)+"."+p+".")
		wantVerifyFailure(t, fail, http.StatusUnauthorized, ReasonAlg)
		if got := f.fetch.count(); got != 1 {
			t.Fatalf("fetcher called %d times, want 1", got)
		}
	})
	t.Run("stale still 401 for oversize", func(t *testing.T) {
		f := stale(t)
		_, fail := f.v.verify(ctx, strings.Repeat("a", 17<<10))
		wantVerifyFailure(t, fail, http.StatusUnauthorized, ReasonMalformed)
		if got := f.fetch.count(); got != 1 {
			t.Fatalf("fetcher called %d times, want 1", got)
		}
	})
	t.Run("stale recovers", func(t *testing.T) {
		f := stale(t)
		c, fail := f.v.verify(ctx, token(f))
		if fail != nil {
			t.Fatalf("verify after a successful on-demand fetch: %v", fail)
		}
		if c.Subject != "alice" {
			t.Fatalf("Subject = %q", c.Subject)
		}
		if got := f.fetch.count(); got != 2 {
			t.Fatalf("fetcher called %d times, want 2", got)
		}
	})
	t.Run("never discovered", func(t *testing.T) {
		f := newVerifyFixture(t, rsaSet(t))
		f.state.Store(nil)
		_, fail := f.v.verify(ctx, token(f))
		wantVerifyFailure(t, fail, http.StatusServiceUnavailable, ReasonKeysStale)
		if got := f.fetch.count(); got != 1 {
			t.Fatalf("fetcher called %d times, want 1", got)
		}
	})
}

func TestVerifyKeySelection(t *testing.T) {
	keys := testKeys(t)
	ctx := context.Background()
	rsaKey := func(kid, alg string) jose.JSONWebKey {
		return jose.JSONWebKey{Key: &keys.rsa2048.PublicKey, KeyID: kid, Algorithm: alg}
	}
	set := func(ks ...jose.JSONWebKey) jose.JSONWebKeySet { return jose.JSONWebKeySet{Keys: ks} }

	t.Run("unknown kid refreshes once, then cooldown", func(t *testing.T) {
		f := newVerifyFixture(t, set(rsaKey("k1", "")))
		f.fetch.program(set(rsaKey("k1", ""), rsaKey("k2", "")), nil)
		tok := mint(t, mintOpts{key: keys.rsa2048, kid: "k2", alg: jose.RS256, claims: baseClaims(f.clock(), nil)})
		if _, fail := f.v.verify(ctx, tok); fail != nil {
			t.Fatalf("verify under a kid the refresh brings: %v", fail)
		}
		if got := f.fetch.count(); got != 2 {
			t.Fatalf("fetcher called %d times, want 2", got)
		}
		tok = mint(t, mintOpts{key: keys.rsa2048, kid: "k3", alg: jose.RS256, claims: baseClaims(f.clock(), nil)})
		_, fail := f.v.verify(ctx, tok)
		wantVerifyFailure(t, fail, http.StatusUnauthorized, ReasonSignature)
		if got := f.fetch.count(); got != 2 {
			t.Fatalf("fetcher called %d times inside the cooldown, want 2", got)
		}
	})

	type row struct {
		name  string
		held  jose.JSONWebKeySet
		token mintOpts
		ok    bool
	}
	rows := []row{
		{"kid never appears", set(rsaKey("k1", "")), mintOpts{key: keys.rsa2048, kid: "k9", alg: jose.RS256}, false},
		{"no kid, one compatible", set(rsaKey("k1", "")), mintOpts{key: keys.rsa2048, alg: jose.RS256}, true},
		{"no kid, two compatible", set(rsaKey("k1", ""), rsaKey("k2", "")), mintOpts{key: keys.rsa2048, alg: jose.RS256}, false},
		{"no kid, incompatible only", set(rsaKey("k1", "")), mintOpts{key: keys.p256, alg: jose.ES256}, false},
		{"ES256 against P-384", set(jose.JSONWebKey{Key: &keys.p384.PublicKey, KeyID: "e1"}),
			mintOpts{key: keys.p256, kid: "e1", alg: jose.ES256}, false},
		{"RS256 against EC", set(jose.JSONWebKey{Key: &keys.p256.PublicKey, KeyID: "k1"}),
			mintOpts{key: keys.rsa2048, kid: "k1", alg: jose.RS256}, false},
		{"key alg pinned", set(rsaKey("k1", "RS256")), mintOpts{key: keys.rsa2048, kid: "k1", alg: jose.RS384}, false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t, tc.held)
			tc.token.claims = baseClaims(f.clock(), nil)
			_, fail := f.v.verify(ctx, mint(t, tc.token))
			if tc.ok {
				if fail != nil {
					t.Fatalf("verify: %v", fail)
				}

				return
			}
			wantVerifyFailure(t, fail, http.StatusUnauthorized, ReasonSignature)
		})
	}
}

func TestVerifyClaims(t *testing.T) {
	keys := testKeys(t)
	ctx := context.Background()
	long := strings.Repeat("x", 257)
	type row struct {
		name   string
		edit   func(c map[string]any)
		setup  func(v *verifier)
		typ    string
		reason string // "" means the token verifies
		check  func(t *testing.T, c claims)
	}
	skew := func(d time.Duration) func(c map[string]any) {
		return func(c map[string]any) { c["iat"] = time.Unix(c["iat"].(int64), 0).Add(d).Unix() }
	}
	rows := []row{
		{name: "issuer", edit: func(c map[string]any) { c["iss"] = "https://other" }, reason: ReasonIssuer},
		{name: "sub missing", edit: func(c map[string]any) { delete(c, "sub") }, reason: ReasonClaim},
		{name: "sub empty", edit: func(c map[string]any) { c["sub"] = "" }, reason: ReasonClaim},
		{name: "sub too long", edit: func(c map[string]any) { c["sub"] = long }, reason: ReasonClaim},
		{name: "sub NUL", edit: func(c map[string]any) { c["sub"] = "a\x00b" }, reason: ReasonClaim},
		{name: "sub not a string", edit: func(c map[string]any) { c["sub"] = 42 }, reason: ReasonClaim},
		{name: "iat missing", edit: func(c map[string]any) { delete(c, "iat") }, reason: ReasonExpired},
		{name: "iat string", edit: func(c map[string]any) { c["iat"] = "123" }, reason: ReasonExpired},
		{name: "iat future", edit: skew(31 * time.Second), reason: ReasonExpired},
		{name: "iat inside skew", edit: skew(29 * time.Second)},
		{name: "exp missing", edit: func(c map[string]any) { delete(c, "exp") }, reason: ReasonExpired},
		{name: "exp past", edit: func(c map[string]any) { c["exp"] = c["iat"].(int64) - 31 }, reason: ReasonExpired},
		{name: "exp inside skew", edit: func(c map[string]any) { c["exp"] = c["iat"].(int64) - 29 }},
		{name: "exp string", edit: func(c map[string]any) { c["exp"] = "123" }, reason: ReasonExpired},
		{name: "nbf future", edit: func(c map[string]any) { c["nbf"] = c["iat"].(int64) + 31 }, reason: ReasonExpired},
		{name: "nbf inside skew", edit: func(c map[string]any) { c["nbf"] = c["iat"].(int64) + 29 }},
		{name: "nbf string", edit: func(c map[string]any) { c["nbf"] = "123" }, reason: ReasonExpired},
		// A number outside int64 converts to an implementation-defined
		// value, on amd64 the most negative int64, which the future checks
		// would accept as a date long past.
		{name: "iat huge", edit: func(c map[string]any) { c["iat"] = 1e100 }, reason: ReasonExpired},
		{name: "iat huge negative", edit: func(c map[string]any) { c["iat"] = -1e100 }, reason: ReasonExpired},
		{name: "nbf huge", edit: func(c map[string]any) { c["nbf"] = 1e100 }, reason: ReasonExpired},
		{name: "nbf huge negative", edit: func(c map[string]any) { c["nbf"] = -1e100 }, reason: ReasonExpired},
		{name: "exp huge negative", edit: func(c map[string]any) { c["exp"] = -1e100 }, reason: ReasonExpired},
		{name: "exp huge", edit: func(c map[string]any) { c["exp"] = 1e100 }, reason: ReasonExpired},
		{name: "username claim absent", setup: func(v *verifier) { v.usernameClaim = "preferred_username" }, reason: ReasonClaim},
		{name: "username claim present", setup: func(v *verifier) { v.usernameClaim = "preferred_username" },
			edit: func(c map[string]any) { c["preferred_username"] = "Alice" },
			check: func(t *testing.T, c claims) {
				if c.Username != "Alice" || c.Subject != "alice" {
					t.Fatalf("claims = %+v", c)
				}
			}},
		{name: "username claim without sub", setup: func(v *verifier) { v.usernameClaim = "preferred_username" },
			edit: func(c map[string]any) { c["preferred_username"] = "Alice"; delete(c, "sub") }, reason: ReasonClaim},
		{name: "username empty", setup: func(v *verifier) { v.usernameClaim = "preferred_username" },
			edit: func(c map[string]any) { c["preferred_username"] = "" }, reason: ReasonClaim},
		{name: "username too long", setup: func(v *verifier) { v.usernameClaim = "preferred_username" },
			edit: func(c map[string]any) { c["preferred_username"] = long }, reason: ReasonClaim},
		{name: "username NUL", setup: func(v *verifier) { v.usernameClaim = "preferred_username" },
			edit: func(c map[string]any) { c["preferred_username"] = "a\x00b" }, reason: ReasonClaim},
		{name: "username not a string", setup: func(v *verifier) { v.usernameClaim = "preferred_username" },
			edit: func(c map[string]any) { c["preferred_username"] = 42 }, reason: ReasonClaim},
		{name: "id: aud missing", edit: func(c map[string]any) { delete(c, "aud") }, reason: ReasonAudience},
		{name: "id: aud array contains", edit: func(c map[string]any) { c["aud"] = []string{"x", testAudience}; c["azp"] = testAudience }},
		{name: "id: multiple aud without azp", edit: func(c map[string]any) { c["aud"] = []string{"x", testAudience} }, reason: ReasonAudience},
		{name: "id: azp wrong", edit: func(c map[string]any) { c["aud"] = []string{"x", testAudience}; c["azp"] = "x" }, reason: ReasonAudience},
		{name: "access: typ JWT", setup: func(v *verifier) { v.tokenType = "access" }, typ: "JWT", reason: ReasonTokenType},
		{name: "access: typ missing", setup: func(v *verifier) { v.tokenType = "access" }, reason: ReasonTokenType},
		{name: "access: typ at+jwt", setup: func(v *verifier) { v.tokenType = "access" }, typ: "at+jwt"},
		{name: "access: typ AT+JWT", setup: func(v *verifier) { v.tokenType = "access" }, typ: "AT+JWT"},
		{name: "access: aud", setup: func(v *verifier) { v.tokenType = "access" }, typ: "at+jwt",
			edit: func(c map[string]any) { c["aud"] = "other" }, reason: ReasonAudience},
		{name: "groups string", edit: func(c map[string]any) { c["groups"] = "admins" },
			check: func(t *testing.T, c claims) {
				if len(c.Groups) != 1 || c.Groups[0] != "admins" {
					t.Fatalf("Groups = %v", c.Groups)
				}
			}},
		{name: "groups absent", check: func(t *testing.T, c claims) {
			if len(c.Groups) != 0 {
				t.Fatalf("Groups = %v", c.Groups)
			}
		}},
		{name: "groups object", edit: func(c map[string]any) { c["groups"] = map[string]any{} }, reason: ReasonClaim},
		{name: "groups mixed", edit: func(c map[string]any) { c["groups"] = []any{"a", 1} }, reason: ReasonClaim},
		{name: "nonce kept", edit: func(c map[string]any) { c["nonce"] = "abc" },
			check: func(t *testing.T, c claims) {
				if c.Nonce != "abc" {
					t.Fatalf("Nonce = %q", c.Nonce)
				}
			}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t, rsaSet(t))
			if tc.setup != nil {
				tc.setup(f.v)
			}
			tok := mint(t, mintOpts{key: keys.rsa2048, kid: "k1", alg: jose.RS256, typ: tc.typ, claims: baseClaims(f.clock(), tc.edit)})
			c, fail := f.v.verify(ctx, tok)
			if tc.reason != "" {
				wantVerifyFailure(t, fail, http.StatusUnauthorized, tc.reason)

				return
			}
			if fail != nil {
				t.Fatalf("verify: %v", fail)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

func TestVerifyInFlightSwap(t *testing.T) {
	keys := testKeys(t)
	f := newVerifyFixture(t, rsaSet(t))
	entered := make(chan struct{})
	release := make(chan struct{})
	f.v.onKeysLoaded = func() {
		close(entered)
		<-release
	}
	tok := mint(t, mintOpts{key: keys.rsa2048, kid: "k1", alg: jose.RS256, claims: baseClaims(f.clock(), nil)})
	type result struct {
		c    claims
		fail *Failure
	}
	done := make(chan result, 1)
	go func() {
		c, fail := f.v.verify(context.Background(), tok)
		done <- result{c, fail}
	}()
	<-entered
	f.fetch.program(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &keys.rsa2048.PublicKey, KeyID: "k2"}}}, nil)
	if err := f.cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	close(release)
	r := <-done
	if r.fail != nil {
		t.Fatalf("verify against the set loaded before the swap: %v", r.fail)
	}
	if r.c.Subject != "alice" {
		t.Fatalf("Subject = %q", r.c.Subject)
	}
	if _, held := f.cache.current().byKID["k1"]; held {
		t.Fatal("the current set still holds k1 after the swap")
	}
	if got := f.fetch.count(); got != 2 {
		t.Fatalf("fetcher called %d times, want the initial fetch plus the test's Refresh", got)
	}
}
