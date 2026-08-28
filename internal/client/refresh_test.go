package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testRevocationEndpoint = testIssuer + "/revoke"
	testDiscoveryURL       = testIssuer + discoveryPath
)

// refreshTransport answers discovery with the issuer's document, the token
// endpoint with the scripted steps in order, and the revocation endpoint with
// its own steps, recording every form it saw.
type refreshTransport struct {
	t           *testing.T
	steps       []pollStep
	revokeSteps []pollStep
	tokens      []url.Values
	revokes     []url.Values
	discoveries int
}

func (r *refreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Helper()
	switch req.URL.String() {
	case testDiscoveryURL:
		r.discoveries++
		doc := `{"issuer":"` + testIssuer + `","token_endpoint":"` + testTokenEndpoint + `","revocation_endpoint":"` + testRevocationEndpoint + `"}`
		return jsonResponse(req, http.StatusOK, doc, false), nil
	case testTokenEndpoint:
		r.tokens = append(r.tokens, readForm(r.t, req))
		return r.answer(req, &r.steps, len(r.tokens))
	case testRevocationEndpoint:
		r.revokes = append(r.revokes, readForm(r.t, req))
		return r.answer(req, &r.revokeSteps, len(r.revokes))
	default:
		r.t.Fatalf("unexpected request to %s", req.URL)
		return nil, nil
	}
}

func (r *refreshTransport) answer(req *http.Request, steps *[]pollStep, n int) (*http.Response, error) {
	r.t.Helper()
	if len(*steps) == 0 {
		r.t.Fatalf("request %d has no scripted answer", n)
	}
	step := (*steps)[0]
	*steps = (*steps)[1:]
	if step.fail {
		return nil, errors.New("connection refused")
	}
	return jsonResponse(req, step.status, step.body, step.html), nil
}

func readForm(t *testing.T, req *http.Request) url.Values {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return form
}

// idTokenExpiring is a token whose payload carries exp and nothing the client
// verifies; the signature segment is filler.
func idTokenExpiring(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`))
	return header + "." + payload + ".sig"
}

type refreshFixture struct {
	store    *Store
	iss      *Issuer
	rt       *refreshTransport
	clock    *fakeClock
	verbose  *bytes.Buffer
	settings Settings
	dir      string
}

// refreshIssuer builds a store and an issuer over one clock, with the token
// endpoint scripted; the settings resolve to the entry's origin.
func refreshIssuer(t *testing.T, tokenType string, steps ...pollStep) *refreshFixture {
	t.Helper()
	store, clock, dir := testStore(t)
	rt := &refreshTransport{t: t, steps: steps}
	var verbose bytes.Buffer
	iss, err := NewIssuer(IssuerOptions{Transport: rt, Now: clock.Now, Sleep: clock.Sleep, Verbose: &verbose})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("https://profgate.example")
	if err != nil {
		t.Fatal(err)
	}
	return &refreshFixture{
		store:   store,
		iss:     iss,
		rt:      rt,
		clock:   clock,
		verbose: &verbose,
		dir:     dir,
		settings: Settings{
			ContextName: "prod",
			Context: Context{Server: u.String(), Auth: AuthSnap{
				Mode: "oidc", Issuer: testIssuer, ClientID: "profgate-cli", TokenType: tokenType,
			}},
			Server:    u,
			Origin:    CanonicalOrigin(u),
			CacheName: "prod",
		},
	}
}

// entry is a cached credential for the fixture's gateway whose token expires
// at now plus ttl; the refresh token is refresh-old.
func (f *refreshFixture) entry(ttl time.Duration) Entry {
	e := Entry{
		Origin:       f.settings.Origin,
		Issuer:       testIssuer,
		ClientID:     "profgate-cli",
		TokenType:    f.settings.Context.Auth.TokenType,
		Token:        "cached-secret",
		ExpiresAt:    f.clock.Now().Add(ttl),
		RefreshToken: "refresh-old",
		ObtainedAt:   f.clock.Now().Add(-time.Minute),
	}
	if e.TokenType == "id" {
		e.Token = idTokenExpiring(e.ExpiresAt)
	}
	return e
}

func (f *refreshFixture) write(t *testing.T, e Entry) {
	t.Helper()
	if err := f.store.Write("prod", e); err != nil {
		t.Fatal(err)
	}
}

func (f *refreshFixture) file(t *testing.T) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "prod.json")) //nolint:gosec // G304: a path this test built itself
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return data, true
}

func (f *refreshFixture) read(t *testing.T) Entry {
	t.Helper()
	e, ok, err := f.store.Read("prod")
	if err != nil || !ok {
		t.Fatalf("Read = %v, %v; want the entry", ok, err)
	}
	return e
}

// apply resolves the cached credential and applies it to one request,
// returning the Authorization header it set.
func (f *refreshFixture) apply(t *testing.T) (string, error) {
	t.Helper()
	cred := CachedCredential(f.store, f.iss, f.settings, f.clock.Now)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://profgate.example/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cred.Apply(context.Background(), req); err != nil {
		return "", err
	}
	return req.Header.Get("Authorization"), nil
}

func (f *refreshFixture) assertNoSecretPrinted(t *testing.T) {
	t.Helper()
	for _, secret := range []string{"cached-secret", "refresh-old", "refresh-new", "id-secret", "access-secret"} {
		if strings.Contains(f.verbose.String(), secret) {
			t.Fatalf("verbose output carries %q: %q", secret, f.verbose.String())
		}
	}
}

func (f *refreshFixture) lockPath() string {
	return filepath.Join(f.dir, "prod.lock")
}

func refreshed(body string) pollStep {
	return pollStep{status: http.StatusOK, body: body}
}

func TestCachedCredentialUsesAFreshToken(t *testing.T) {
	f := refreshIssuer(t, "access")
	// A refusing round tripper proves no request is built; the store must not
	// take the lock either.
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t: t}, Now: f.clock.Now, Sleep: f.clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	f.iss = iss
	f.write(t, f.entry(31*time.Second))
	if err := os.WriteFile(f.lockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := f.file(t)
	header, err := f.apply(t)
	if err != nil {
		t.Fatal(err)
	}
	if header != "Bearer cached-secret" {
		t.Fatalf("Authorization = %q, want the cached token", header)
	}
	if after, _ := f.file(t); !bytes.Equal(before, after) {
		t.Fatal("the file changed for a token that needed no refresh")
	}
}

func TestCachedCredentialRefreshRequest(t *testing.T) {
	f := refreshIssuer(t, "access", refreshed(`{"access_token":"access-secret","expires_in":300}`))
	f.write(t, f.entry(30*time.Second))
	if _, err := f.apply(t); err != nil {
		t.Fatal(err)
	}
	if len(f.rt.tokens) != 1 {
		t.Fatalf("%d token-endpoint requests, want 1", len(f.rt.tokens))
	}
	form := f.rt.tokens[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-old" || form.Get("client_id") != "profgate-cli" {
		t.Fatalf("form = %v, want grant_type=refresh_token, the refresh token, and client_id", form)
	}
	if form.Has("client_secret") {
		t.Fatal("the request carries a client secret")
	}
	f.assertNoSecretPrinted(t)
}

func TestCachedCredentialRefreshOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		tokenType string
		body      func(f *refreshFixture) string
		check     func(t *testing.T, f *refreshFixture, header string, before Entry)
	}{
		{
			name:      "a response carrying the access token replaces the cached token",
			tokenType: "access",
			body:      func(*refreshFixture) string { return `{"access_token":"access-secret","expires_in":300}` },
			check: func(t *testing.T, f *refreshFixture, header string, _ Entry) {
				if header != "Bearer access-secret" {
					t.Fatalf("Authorization = %q, want the new token", header)
				}
				e := f.read(t)
				if e.Token != "access-secret" {
					t.Fatalf("cached token = %q, want the new one", e.Token)
				}
				if !e.ObtainedAt.Equal(f.clock.Now()) {
					t.Fatalf("obtainedAt = %v, want the response time %v", e.ObtainedAt, f.clock.Now())
				}
				if !e.ExpiresAt.Equal(f.clock.Now().Add(300 * time.Second)) {
					t.Fatalf("expiresAt = %v, want obtainedAt plus 300s", e.ExpiresAt)
				}
			},
		},
		{
			name:      "a response carrying the id token replaces the cached token with its own exp",
			tokenType: "id",
			body: func(f *refreshFixture) string {
				return `{"id_token":"` + idTokenExpiring(f.clock.Now().Add(7*time.Minute)) + `","access_token":"access-secret","expires_in":300}`
			},
			check: func(t *testing.T, f *refreshFixture, header string, _ Entry) {
				e := f.read(t)
				if header != "Bearer "+e.Token || !strings.HasPrefix(e.Token, "eyJ") {
					t.Fatalf("Authorization = %q, cached token = %q; want the new id token in both", header, e.Token)
				}
				if !e.ExpiresAt.Equal(f.clock.Now().Add(7 * time.Minute)) {
					t.Fatalf("expiresAt = %v, want the id token's exp, not expires_in", e.ExpiresAt)
				}
			},
		},
		{
			name:      "a rotated refresh token replaces the stored one",
			tokenType: "access",
			body: func(*refreshFixture) string {
				return `{"access_token":"access-secret","expires_in":300,"refresh_token":"refresh-new"}`
			},
			check: func(t *testing.T, f *refreshFixture, _ string, _ Entry) {
				if e := f.read(t); e.RefreshToken != "refresh-new" {
					t.Fatalf("refreshToken = %q, want the rotation", e.RefreshToken)
				}
			},
		},
		{
			name:      "no rotation keeps the stored refresh token",
			tokenType: "access",
			body:      func(*refreshFixture) string { return `{"access_token":"access-secret","expires_in":300}` },
			check: func(t *testing.T, f *refreshFixture, _ string, _ Entry) {
				if e := f.read(t); e.RefreshToken != "refresh-old" {
					t.Fatalf("refreshToken = %q, want refresh-old kept", e.RefreshToken)
				}
			},
		},
		{
			name:      "refresh_expires_in sets refreshExpiresAt from the response time",
			tokenType: "access",
			body: func(*refreshFixture) string {
				return `{"access_token":"access-secret","expires_in":300,"refresh_expires_in":1800}`
			},
			check: func(t *testing.T, f *refreshFixture, _ string, _ Entry) {
				if e := f.read(t); !e.RefreshExpiresAt.Equal(f.clock.Now().Add(1800 * time.Second)) {
					t.Fatalf("refreshExpiresAt = %v, want the response time plus 1800s", e.RefreshExpiresAt)
				}
			},
		},
		{
			name:      "no refresh_expires_in leaves refreshExpiresAt out of the file",
			tokenType: "access",
			body:      func(*refreshFixture) string { return `{"access_token":"access-secret","expires_in":300}` },
			check: func(t *testing.T, f *refreshFixture, _ string, _ Entry) {
				data, _ := f.file(t)
				if strings.Contains(string(data), "refreshExpiresAt") {
					t.Fatalf("file %s carries refreshExpiresAt", data)
				}
			},
		},
		{
			name:      "a rotation without the selected token stores the rotation and keeps the old token",
			tokenType: "id",
			body: func(*refreshFixture) string {
				return `{"access_token":"access-secret","expires_in":300,"refresh_token":"refresh-new"}`
			},
			check: func(t *testing.T, f *refreshFixture, header string, before Entry) {
				e := f.read(t)
				if e.RefreshToken != "refresh-new" {
					t.Fatalf("refreshToken = %q, want the rotation", e.RefreshToken)
				}
				if e.Token != before.Token || !e.ExpiresAt.Equal(before.ExpiresAt) {
					t.Fatalf("token = %q expiring %v, want the old one kept until %v", e.Token, e.ExpiresAt, before.ExpiresAt)
				}
				if header != "Bearer "+before.Token {
					t.Fatalf("Authorization = %q, want the old token", header)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := refreshIssuer(t, tc.tokenType)
			before := f.entry(30 * time.Second)
			f.write(t, before)
			// The response arrives five seconds after the entry was written.
			f.clock.now = f.clock.now.Add(5 * time.Second)
			f.rt.steps = []pollStep{refreshed(tc.body(f))}
			header, err := f.apply(t)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.rt.tokens) != 1 {
				t.Fatalf("%d token-endpoint requests, want 1", len(f.rt.tokens))
			}
			tc.check(t, f, header, before)
			f.assertNoSecretPrinted(t)
		})
	}
}

func TestCachedCredentialNoTokenOfTheSelectedTypePastExpiry(t *testing.T) {
	f := refreshIssuer(t, "id", refreshed(`{"access_token":"access-secret","expires_in":300,"refresh_token":"refresh-new"}`))
	f.write(t, f.entry(-time.Second))
	_, err := f.apply(t)
	if !errors.Is(err, ErrLoginNeeded) || !strings.Contains(err.Error(), "id token") {
		t.Fatalf("err = %v, want ErrLoginNeeded naming the missing id token", err)
	}
	if e := f.read(t); e.RefreshToken != "refresh-new" {
		t.Fatalf("refreshToken = %q, want the rotation stored", e.RefreshToken)
	}
	f.assertNoSecretPrinted(t)
}

func TestCachedCredentialRefreshExpiryPast(t *testing.T) {
	f := refreshIssuer(t, "access")
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t: t}, Now: f.clock.Now, Sleep: f.clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	f.iss = iss
	e := f.entry(10 * time.Second)
	e.RefreshExpiresAt = f.clock.Now().Add(-time.Second)
	f.write(t, e)
	before, _ := f.file(t)
	if _, err := f.apply(t); !errors.Is(err, ErrLoginNeeded) {
		t.Fatalf("err = %v, want ErrLoginNeeded", err)
	}
	if after, _ := f.file(t); !bytes.Equal(before, after) {
		t.Fatal("the file changed")
	}
}

func TestCachedCredentialLeavesTheFileOnRetryableFailures(t *testing.T) {
	cases := []struct {
		name string
		step pollStep
		want string
	}{
		{"a transport failure", pollStep{fail: true}, "connection refused"},
		{"an issuer 500", pollStep{status: http.StatusInternalServerError, body: `{"error":"server_error"}`}, "500"},
		{"an issuer 429", pollStep{status: http.StatusTooManyRequests, body: `{"error":"slow_down"}`}, "429"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := refreshIssuer(t, "access", tc.step)
			f.write(t, f.entry(10*time.Second))
			before, _ := f.file(t)
			_, err := f.apply(t)
			if err == nil || errors.Is(err, ErrLoginNeeded) {
				t.Fatalf("err = %v, want a failure that is not ErrLoginNeeded", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %q does not name %q", err, tc.want)
			}
			after, ok := f.file(t)
			if !ok || !bytes.Equal(before, after) {
				t.Fatalf("the file is not byte-identical: present %v", ok)
			}
			if _, err := os.Stat(f.lockPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lock file after the refresh: %v; want it released", err)
			}
			f.assertNoSecretPrinted(t)
		})
	}
}

func TestCachedCredentialDeletesOnPermanentRefusals(t *testing.T) {
	cases := []struct {
		name string
		step pollStep
	}{
		{"invalid_grant", pollStep{status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`}},
		{"invalid_client", pollStep{status: http.StatusUnauthorized, body: `{"error":"invalid_client"}`}},
		{"unauthorized_client", pollStep{status: http.StatusBadRequest, body: `{"error":"unauthorized_client"}`}},
		{"invalid_scope", pollStep{status: http.StatusBadRequest, body: `{"error":"invalid_scope"}`}},
		{"unsupported_grant_type", pollStep{status: http.StatusBadRequest, body: `{"error":"unsupported_grant_type"}`}},
		{"an unrecognized error value", pollStep{status: http.StatusBadRequest, body: `{"error":"something_else"}`}},
		{"a 400 whose body is not the error shape", pollStep{status: http.StatusBadRequest, body: "<html>no</html>", html: true}},
		{"a 403 with no body", pollStep{status: http.StatusForbidden, body: "", html: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := refreshIssuer(t, "access", tc.step)
			f.write(t, f.entry(10*time.Second))
			var lockedDuringDelete bool
			f.store.write = func(string, string, []byte, os.FileMode) error {
				t.Fatal("the store wrote a file on a permanent refusal")
				return nil
			}
			// Wrap the transport so the lock is observed while the refusal is
			// handled: the delete must happen before the release.
			f.iss.post.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := f.rt.RoundTrip(req)
				if _, statErr := os.Stat(f.lockPath()); statErr == nil {
					lockedDuringDelete = true
				}
				return resp, err
			})
			_, err := f.apply(t)
			if !errors.Is(err, ErrLoginNeeded) || !strings.Contains(err.Error(), "profgate login") {
				t.Fatalf("err = %v, want ErrLoginNeeded", err)
			}
			if strings.Contains(err.Error(), "<html>") {
				t.Fatalf("err %q carries the body", err)
			}
			if _, ok := f.file(t); ok {
				t.Fatal("the cache file survived a permanent refusal")
			}
			if !lockedDuringDelete {
				t.Fatal("the lock was not held while the issuer answered")
			}
			if _, err := os.Stat(f.lockPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lock file afterwards: %v; want it released", err)
			}
			f.assertNoSecretPrinted(t)
		})
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCachedCredentialKeepsAFileWhoseObtainedAtMoved(t *testing.T) {
	f := refreshIssuer(t, "access", pollStep{status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`})
	f.write(t, f.entry(10*time.Second))
	// Another process, unserialized by a filesystem that ignores the lock,
	// writes a newer entry while the issuer answers.
	newer := f.entry(5 * time.Minute)
	newer.ObtainedAt = f.clock.Now()
	newer.RefreshToken = "refresh-new"
	f.iss.post.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := f.rt.RoundTrip(req)
		f.write(t, newer)
		return resp, err
	})
	if _, err := f.apply(t); !errors.Is(err, ErrLoginNeeded) {
		t.Fatalf("err = %v, want ErrLoginNeeded", err)
	}
	e := f.read(t)
	if e.RefreshToken != "refresh-new" {
		t.Fatalf("the newer entry was deleted: %+v", e)
	}
}

func TestCachedCredentialWithoutARefreshToken(t *testing.T) {
	f := refreshIssuer(t, "access")
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t: t}, Now: f.clock.Now, Sleep: f.clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	f.iss = iss
	e := f.entry(-time.Minute)
	e.RefreshToken = ""
	f.write(t, e)
	_, err = f.apply(t)
	if !errors.Is(err, ErrLoginNeeded) || !strings.Contains(err.Error(), "prod") {
		t.Fatalf("err = %v, want ErrLoginNeeded naming the context", err)
	}
	if _, ok := f.file(t); !ok {
		t.Fatal("the file was deleted")
	}
}

func TestCachedCredentialWithNoEntry(t *testing.T) {
	f := refreshIssuer(t, "access")
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t: t}, Now: f.clock.Now, Sleep: f.clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	f.iss = iss
	if _, err := f.apply(t); !errors.Is(err, ErrLoginNeeded) {
		t.Fatalf("err = %v, want ErrLoginNeeded", err)
	}
}

func TestCachedCredentialRefusesAnotherOrigin(t *testing.T) {
	f := refreshIssuer(t, "access")
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t: t}, Now: f.clock.Now, Sleep: f.clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	f.iss = iss
	e := f.entry(-time.Minute)
	e.Origin = "https://other.example:443"
	f.write(t, e)
	before, _ := f.file(t)
	_, err = f.apply(t)
	if err == nil {
		t.Fatal("Apply = nil for an entry bound to another origin")
	}
	for _, want := range []string{"https://other.example:443", "https://profgate.example:443", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q does not name %s", err, want)
		}
	}
	if after, _ := f.file(t); !bytes.Equal(before, after) {
		t.Fatal("the file changed")
	}
}

func TestCachedCredentialSerializesTwoClients(t *testing.T) {
	f := refreshIssuer(t, "access", refreshed(`{"access_token":"access-secret","expires_in":300,"refresh_token":"refresh-new"}`))
	f.write(t, f.entry(10*time.Second))
	// The first client holds the lock when the second arrives; the second
	// waits, and while it waits the first refreshes and releases.
	if err := os.WriteFile(f.lockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first := CachedCredential(f.store, f.iss, f.settings, f.clock.Now)
	firstDone := false
	f.store.sleep = func(ctx context.Context, d time.Duration) error {
		if !firstDone {
			firstDone = true
			if err := os.Remove(f.lockPath()); err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://profgate.example/v1/whoami", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Apply(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		return f.clock.Sleep(ctx, d)
	}
	header, err := f.apply(t)
	if err != nil {
		t.Fatal(err)
	}
	if !firstDone {
		t.Fatal("the second client never waited for the lock")
	}
	if header != "Bearer access-secret" {
		t.Fatalf("Authorization = %q, want the token the first client obtained", header)
	}
	if len(f.rt.tokens) != 1 {
		t.Fatalf("%d token-endpoint requests, want exactly 1", len(f.rt.tokens))
	}
	if e := f.read(t); e.RefreshToken != "refresh-new" {
		t.Fatalf("refreshToken = %q, want the first client's rotation kept", e.RefreshToken)
	}
	f.assertNoSecretPrinted(t)
}

func TestCachedCredentialReportsAHeldLock(t *testing.T) {
	f := refreshIssuer(t, "access")
	f.write(t, f.entry(10*time.Second))
	if err := os.WriteFile(f.lockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := f.file(t)
	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), f.lockPath()) {
		t.Fatalf("err = %v, want one naming %s", err, f.lockPath())
	}
	if errors.Is(err, ErrLoginNeeded) {
		t.Fatalf("err = %v is ErrLoginNeeded; a held lock is not a missing token", err)
	}
	if len(f.rt.tokens) != 0 {
		t.Fatal("a request was sent while the lock was held")
	}
	if after, _ := f.file(t); !bytes.Equal(before, after) {
		t.Fatal("the file changed")
	}
}

func TestPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  *IssuerError
		want bool
	}{
		{"invalid_grant", &IssuerError{Status: 400, Code: "invalid_grant"}, true},
		{"invalid_client", &IssuerError{Status: 401, Code: "invalid_client"}, true},
		{"unauthorized_client", &IssuerError{Status: 400, Code: "unauthorized_client"}, true},
		{"invalid_scope", &IssuerError{Status: 400, Code: "invalid_scope"}, true},
		{"unsupported_grant_type", &IssuerError{Status: 400, Code: "unsupported_grant_type"}, true},
		{"an unrecognized value", &IssuerError{Status: 400, Code: "something_else"}, true},
		{"a 400 without the error shape", &IssuerError{Status: 400}, true},
		{"a 403 without a body", &IssuerError{Status: 403}, true},
		{"a 429", &IssuerError{Status: 429, Code: "slow_down"}, false},
		{"a 429 without a body", &IssuerError{Status: 429}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanent(tc.err); got != tc.want {
				t.Fatalf("permanent(%+v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRefreshPostsTheGrant(t *testing.T) {
	f := refreshIssuer(t, "access", refreshed(`{"access_token":"access-secret","expires_in":300,"refresh_token":"refresh-new","refresh_expires_in":1800}`))
	m := Metadata{Issuer: testIssuer, TokenEndpoint: testTokenEndpoint}
	tr, err := f.iss.Refresh(context.Background(), m, "profgate-cli", "refresh-old")
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken != "access-secret" || tr.RefreshToken != "refresh-new" || tr.ExpiresIn != 300 || tr.RefreshExpiresIn != 1800 {
		t.Fatalf("TokenResponse = %+v", tr)
	}
	form := f.rt.tokens[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-old" || form.Get("client_id") != "profgate-cli" {
		t.Fatalf("form = %v", form)
	}
	f.assertNoSecretPrinted(t)
}

func TestRevoke(t *testing.T) {
	m := Metadata{Issuer: testIssuer, TokenEndpoint: testTokenEndpoint, RevocationEndpoint: testRevocationEndpoint}

	t.Run("posts the refresh token with its hint and client_id", func(t *testing.T) {
		f := refreshIssuer(t, "access")
		f.rt.revokeSteps = []pollStep{{status: http.StatusOK, body: ""}}
		if err := f.iss.Revoke(context.Background(), m, "profgate-cli", "refresh-old"); err != nil {
			t.Fatal(err)
		}
		if len(f.rt.revokes) != 1 {
			t.Fatalf("%d revocation requests, want 1", len(f.rt.revokes))
		}
		form := f.rt.revokes[0]
		if form.Get("token") != "refresh-old" || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != "profgate-cli" {
			t.Fatalf("form = %v, want token, token_type_hint=refresh_token, and client_id", form)
		}
		if form.Has("client_secret") {
			t.Fatal("the request carries a client secret")
		}
		f.assertNoSecretPrinted(t)
	})

	t.Run("a 400 is returned for the caller to decide", func(t *testing.T) {
		f := refreshIssuer(t, "access")
		f.rt.revokeSteps = []pollStep{{status: http.StatusBadRequest, body: `{"error":"unsupported_token_type"}`}}
		err := f.iss.Revoke(context.Background(), m, "profgate-cli", "refresh-old")
		var ie *IssuerError
		if !errors.As(err, &ie) || ie.Status != http.StatusBadRequest || ie.Code != "unsupported_token_type" {
			t.Fatalf("err = %v, want an IssuerError 400 unsupported_token_type", err)
		}
	})

	t.Run("a transport failure is a TransportError", func(t *testing.T) {
		f := refreshIssuer(t, "access")
		f.rt.revokeSteps = []pollStep{{fail: true}}
		err := f.iss.Revoke(context.Background(), m, "profgate-cli", "refresh-old")
		var te *TransportError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v, want a TransportError", err)
		}
		if strings.Contains(err.Error(), "/revoke") {
			t.Fatalf("err %q names the path", err)
		}
	})

	t.Run("no published endpoint is an error naming it", func(t *testing.T) {
		f := refreshIssuer(t, "access")
		err := f.iss.Revoke(context.Background(), Metadata{Issuer: testIssuer, TokenEndpoint: testTokenEndpoint}, "profgate-cli", "refresh-old")
		if err == nil || !strings.Contains(err.Error(), "revocation_endpoint") {
			t.Fatalf("err = %v, want one naming revocation_endpoint", err)
		}
		if len(f.rt.revokes) != 0 {
			t.Fatal("a request was sent")
		}
	})
}
