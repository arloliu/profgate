package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

const authInfoPath = "/v1/auth"

// The scope list an empty cli block takes; it names offline_access so Dex issues a refresh token.
const cliDefaultScopesJSON = `["openid","offline_access"]`

// authInfoHarness is a harness under the given mode whose authenticator fails
// the test if the route reads a credential; the configuration names a
// namespace, a Service, a realm, and a principal so a leak has something to show.
func authInfoHarness(t *testing.T, mode string) *harness {
	t.Helper()

	h := newHarness(baseTarget())
	h.configure(func(cfg *config.Config) {
		cfg.Auth.Mode = mode
		cfg.Auth.AnonymousRealm = ""
		cfg.Auth.Basic = &config.BasicConfig{Users: []config.BasicUser{{Name: "alice", Realm: "developer"}}}
		cfg.Realms = map[string]config.Realm{
			"developer": {Namespaces: []string{fixtureNamespace}, Services: []string{fixtureService}, Profiles: []string{"heap"}},
		}
	})
	h.auth = &fakeAuth{onCall: func() { t.Errorf("the authenticator was called for %s", authInfoPath) }}

	return h
}

// expectNoDisclosure fails when the body names the namespace, Service, Pod, realm, or principal the configuration holds.
func expectNoDisclosure(t *testing.T, body string) {
	t.Helper()

	for _, word := range []string{fixtureNamespace, fixtureService, fixturePod, "developer", "alice"} {
		if strings.Contains(body, word) {
			t.Errorf("body %q names %q", body, word)
		}
	}
}

func TestAuthInfoBody(t *testing.T) {
	browser := &config.OIDCBrowser{ClientID: "browser-app", Scopes: []string{"openid", "profile", "email"}}
	// The block as the loader leaves an empty one: the client identifier from the audience and the default scopes.
	emptyCLI := func() *config.OIDCCLI {
		return &config.OIDCCLI{ClientID: "profgate", Scopes: []string{"openid", "offline_access"}}
	}
	oidc := oidcConfig

	for _, tc := range []struct {
		name string
		mode string
		oidc *config.OIDCConfig
		want string
	}{
		{name: "basic", mode: config.ModeBasic, want: `{"mode":"basic"}`},
		{name: "disabled", mode: config.ModeDisabled, want: `{"mode":"disabled"}`},
		{name: "oidc without a cli block", mode: config.ModeOIDC, oidc: oidc(nil, nil), want: `{"mode":"oidc"}`},
		{name: "oidc with an empty cli block", mode: config.ModeOIDC, oidc: oidc(emptyCLI(), nil),
			want: `{"mode":"oidc","oidc":{"issuer":"https://issuer.example","clientID":"profgate","tokenType":"id","scopes":` +
				cliDefaultScopesJSON + `,"pkce":false}}`},
		{name: "oidc with every cli key set", mode: config.ModeOIDC,
			oidc: func() *config.OIDCConfig {
				o := oidc(&config.OIDCCLI{ClientID: "profgate-cli", Scopes: []string{"openid", "groups", "offline_access"}, PKCE: true}, nil)
				o.TokenType = "access"

				return o
			}(),
			want: `{"mode":"oidc","oidc":{"issuer":"https://issuer.example","clientID":"profgate-cli","tokenType":"access",` +
				`"scopes":["openid","groups","offline_access"],"pkce":true}}`},
		{name: "oidc with an empty cli block beside a browser block", mode: config.ModeOIDC, oidc: oidc(emptyCLI(), browser),
			want: `{"mode":"oidc","oidc":{"issuer":"https://issuer.example","clientID":"profgate","tokenType":"id","scopes":` +
				cliDefaultScopesJSON + `,"pkce":false}}`},
		{name: "oidc with a browser block and no cli block", mode: config.ModeOIDC, oidc: oidc(nil, browser), want: `{"mode":"oidc"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := authInfoHarness(t, tc.mode)
			h.configure(func(cfg *config.Config) { cfg.Auth.OIDC = tc.oidc })
			rec := h.do(t, http.MethodGet, authInfoPath)
			expectJSON(t, rec, tc.want)
			body := rec.Body.String()
			if !strings.Contains(tc.want, `"oidc":`) && strings.Contains(body, `"oidc":`) {
				t.Errorf("body %q carries an oidc key", body)
			}
			if strings.Contains(body, "browser-app") || strings.Contains(body, "email") {
				t.Errorf("body %q carries a browser value", body)
			}
			expectNoDisclosure(t, body)
			h.expectNoAudit(t)
			h.expectMetric(t, metrics.EndpointAuth, labelNone)
			h.expectMetricCode(t, codeOK)
		})
	}

	t.Run("an empty cli block loaded from a file", func(t *testing.T) {
		// The loader, not the route, fills the client identifier and the scopes;
		// this row proves the two defaults reach the body.
		path := filepath.Join(t.TempDir(), "profgate.yaml")
		yaml := "auth:\n  mode: oidc\n  oidc:\n    issuer: https://issuer.example\n    audience: profgate\n" +
			"    mapping:\n      defaultRealm: developer\n    cli: {}\n" +
			"realms:\n  developer:\n    namespaces: [\"*\"]\n    services: [\"*\"]\n    profiles: [\"*\"]\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		h := authInfoHarness(t, config.ModeOIDC)
		h.cfg.Store(cfg)
		rec := h.do(t, http.MethodGet, authInfoPath)
		expectJSON(t, rec, `{"mode":"oidc","oidc":{"issuer":"https://issuer.example","clientID":"profgate","tokenType":"id",`+
			`"scopes":`+cliDefaultScopesJSON+`,"pkce":false}}`)
	})

	t.Run("the scope list is copied", func(t *testing.T) {
		cfg := testConfig()
		cfg.Auth.Mode = config.ModeOIDC
		cfg.Auth.OIDC = oidc(&config.OIDCCLI{ClientID: "profgate", Scopes: []string{"openid", "offline_access"}}, nil)
		view := authInfoView(cfg)
		view.OIDC.Scopes[0] = "changed"
		if got := cfg.Auth.OIDC.CLI.Scopes[0]; got != "openid" {
			t.Errorf("configuration scope = %q after editing the view; the body aliases the configuration", got)
		}
	})
}

func TestAuthInfoRoute(t *testing.T) {
	t.Run("no credential and a wrong one under basic", func(t *testing.T) {
		for name, headers := range map[string]map[string]string{
			"none":  nil,
			"wrong": {"Authorization": "Basic d3Jvbmc6d3Jvbmc="},
		} {
			t.Run(name, func(t *testing.T) {
				h := authInfoHarness(t, config.ModeBasic)
				rec := h.doWith(t, http.MethodGet, authInfoPath, headers)
				expectJSON(t, rec, `{"mode":"basic"}`)
				if got := rec.Header().Get("WWW-Authenticate"); got != "" {
					t.Errorf("WWW-Authenticate = %q, want none", got)
				}
				h.expectNoAudit(t)
				h.expectMetric(t, metrics.EndpointAuth, labelNone)
			})
		}
	})

	t.Run("POST", func(t *testing.T) {
		h := authInfoHarness(t, config.ModeBasic)
		rec := h.do(t, http.MethodPost, authInfoPath)
		h.expectRouteError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if got := rec.Header().Get("Allow"); got != "GET" {
			t.Errorf("Allow = %q, want GET", got)
		}
	})

	t.Run("not ready", func(t *testing.T) {
		h := authInfoHarness(t, config.ModeBasic)
		h.disc.synced = false
		rec := h.do(t, http.MethodGet, authInfoPath)
		h.expectRouteError(t, rec, http.StatusServiceUnavailable, "not_ready")
	})

	for name, query := range map[string]string{
		"a query parameter": "?x=1",
		"access_token":      "?access_token=x",
	} {
		t.Run(name, func(t *testing.T) {
			h := authInfoHarness(t, config.ModeBasic)
			rec := h.do(t, http.MethodGet, authInfoPath+query)
			h.expectRouteError(t, rec, http.StatusBadRequest, "invalid_parameter")
			if _, msg := errorBodyOf(t, rec); strings.Contains(msg, "access_token") {
				t.Errorf("message %q comes from the credential-placement step, want the parameter step", msg)
			}
		})
	}
}

// expectRouteError checks a gateway error on /v1/auth: the envelope, no audit record,
// and one metrics row under endpoint auth carrying the code.
func (h *harness) expectRouteError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if got, _ := errorBodyOf(t, rec); got != code {
		t.Errorf("code = %q, want %q (body %q)", got, code, rec.Body.String())
	}
	detailsOf(t, rec, code)
	h.expectNoAudit(t)
	h.expectMetric(t, metrics.EndpointAuth, labelNone)
	h.expectMetricCode(t, code)
}
