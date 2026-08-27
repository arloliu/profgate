// Package auth resolves a request to a principal and the realm it acts in.
// It holds the three modes the configuration names — disabled, basic, and
// oidc — and is the only package that imports go-jose and x/crypto, so the
// code that judges a credential can be read in one place.
package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/arloliu/profgate/internal/config"
)

// Authenticator resolves a request to a principal and the name of its realm,
// judged against the configuration snapshot the request loaded.
// A failure carries a Reason for the audit log and never the credential.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request, cfg *config.Config) (Principal, error)
}

// Principal is who a request acts as and where it may act.
type Principal struct {
	Name  string // audit log and PGO CreatedBy/UpdatedBy
	Realm string // key into cfg.Realms
}

// Failure is the error Authenticate returns when the request is not admitted.
type Failure struct {
	Status   int    // 401, 429, or 503
	Reason   string // one value from the audit reason table
	Redirect string // non-empty when a navigation should be sent to login instead
	// ClearSession asks the caller to delete the session cookie before answering;
	// set when the cookie was unopenable or expired.
	ClearSession bool
}

// Error names the status and reason and nothing else: a Failure may be
// logged, and the credential must never travel with it.
func (f *Failure) Error() string {
	return fmt.Sprintf("auth: %d %s", f.Status, f.Reason)
}

// The audit reasons, one constant per row of the audit table in
// docs/specs/auth.md.
// A Failure names one of these rather than a string, and Reasons returns them
// all, so the set the package can emit is closed and testable.
const (
	ReasonMissing        = "missing"
	ReasonScheme         = "scheme"
	ReasonMalformed      = "malformed"
	ReasonBadCredential  = "bad_credential" //nolint:gosec // an audit label, not a credential
	ReasonThrottled      = "throttled"
	ReasonSignature      = "signature"
	ReasonAlg            = "alg"
	ReasonIssuer         = "issuer"
	ReasonAudience       = "audience"
	ReasonTokenType      = "token_type"
	ReasonExpired        = "expired"
	ReasonClaim          = "claim"
	ReasonNonce          = "nonce"
	ReasonNoRealm        = "no_realm"
	ReasonSession        = "session"
	ReasonState          = "state"
	ReasonIssuerDenied   = "issuer_denied"
	ReasonExchangeDenied = "exchange_denied"
	ReasonExchange       = "exchange"
	ReasonCSRF           = "csrf"
	ReasonKeysStale      = "keys_stale"
	ReasonEntropy        = "entropy"
	// ReasonInternal is an Authenticate error that is not a *Failure;
	// internal/httpapi assigns it.
	ReasonInternal = "internal"
)

// Reasons returns every reason in table order.
// The metrics recorder takes the strings without knowing the set; the
// response-uniformity test walks this list.
func Reasons() []string {
	return []string{
		ReasonMissing, ReasonScheme, ReasonMalformed, ReasonBadCredential,
		ReasonThrottled, ReasonSignature, ReasonAlg, ReasonIssuer,
		ReasonAudience, ReasonTokenType, ReasonExpired, ReasonClaim,
		ReasonNonce, ReasonNoRealm, ReasonSession, ReasonState,
		ReasonIssuerDenied, ReasonExchangeDenied, ReasonExchange,
		ReasonCSRF, ReasonKeysStale, ReasonEntropy, ReasonInternal,
	}
}

// The mode names as config validates them.
const (
	modeBasic = "basic"
	modeOIDC  = "oidc"
)

// Challenge is the WWW-Authenticate value a 401 carries under mode:
// the scheme the mode accepts, or "" for disabled, which never answers 401.
func Challenge(mode string) string {
	switch mode {
	case modeBasic:
		return `Basic realm="profgate"`
	case modeOIDC:
		return `Bearer realm="profgate"`
	default:
		return ""
	}
}

// Disabled is the mode the gateway ships with: every request is the
// anonymous principal in cfg.Auth.AnonymousRealm.
type Disabled struct{}

// Authenticate implements Authenticator.
func (Disabled) Authenticate(_ context.Context, _ *http.Request, cfg *config.Config) (Principal, error) {
	return Principal{Name: "anonymous", Realm: cfg.Auth.AnonymousRealm}, nil
}
