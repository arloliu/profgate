package httpapi

import (
	"net/http"

	"github.com/arloliu/profgate/internal/config"
)

// authInfoBody is what GET /v1/auth reports: the mode always, and the oidc object only where auth.oidc.cli is configured.
type authInfoBody struct {
	Mode string       `json:"mode"`
	OIDC *authInfoCLI `json:"oidc,omitempty"`
}

// authInfoCLI is the device login an operator configured:
// never derived from auth.oidc.browser,
// because a browser registration and a device-flow registration are the same one under tokenType id and may differ under access.
type authInfoCLI struct {
	Issuer    string   `json:"issuer"`
	ClientID  string   `json:"clientID"`
	TokenType string   `json:"tokenType"`
	Scopes    []string `json:"scopes"`
	PKCE      bool     `json:"pkce"`
}

// authInfoView builds the body from one configuration snapshot.
// The client identifier and the scopes are read as the loader left them:
// it fills the audience and the default scope list into an empty cli block.
func authInfoView(cfg *config.Config) authInfoBody {
	body := authInfoBody{Mode: cfg.Auth.Mode}
	oidc := cfg.Auth.OIDC
	if cfg.Auth.Mode != config.ModeOIDC || oidc == nil || oidc.CLI == nil {
		return body
	}
	body.OIDC = &authInfoCLI{
		Issuer:    oidc.Issuer,
		ClientID:  oidc.CLI.ClientID,
		TokenType: oidc.TokenType,
		Scopes:    cloneList(oidc.CLI.Scopes),
		PKCE:      oidc.CLI.PKCE,
	}

	return body
}

// serveAuthInfo answers GET /v1/auth after the readiness step.
// The route has no credential-placement, authentication, or realm step:
// it is what a client reads before it holds a credential, and it names nothing a realm bounds.
// Any query parameter is refused, which is what answers a token in the URL here.
func (s *server) serveAuthInfo(w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config) {
	if r.URL.RawQuery != "" {
		q.fail(w, noParameters(r.URL.RawQuery))

		return
	}
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	writeJSON(w, http.StatusOK, authInfoView(cfg))
}
