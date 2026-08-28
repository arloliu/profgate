package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultLoginTimeout = 10 * time.Minute
	minLoginTimeout     = time.Minute
	maxLoginTimeout     = 30 * time.Minute
)

// defaultScopes is what a login asks for when neither the gateway, the
// context, nor a flag named the scopes: offline_access is what makes the
// issuer grant a refresh token.
func defaultScopes() []string { return []string{"openid", "offline_access"} }

// Whoami is what GET /v1/whoami reported: the principal and its realm's name.
type Whoami struct {
	Principal string
	Realm     string
}

// LoginInput is everything login needs: the resolved settings, the gateway
// client without a credential, the issuer client, the store, the flags that override what /v1/auth reported, the clock, and the two writers.
type LoginInput struct {
	Settings Settings
	Gateway  *Client
	Issuer   *Issuer
	Store    *Store
	Flags    LoginFlags
	Now      func() time.Time
	Stdout   io.Writer                                 // the principal and realm
	Stderr   io.Writer                                 // the user code, the verification URIs, the basic-mode advice
	Basic    func() (user, password string, err error) // the command-side prompt, under basic
	SaveFile func(*File) error                         // writes the contexts file; nil under --server alone
	File     *File
}

// LoginFlags is what the login flags said; an unset field said nothing.
type LoginFlags struct {
	Issuer, ClientID, TokenType, IssuerCAFile string
	Scopes                                    []string
	PKCE                                      *bool
	LoginTimeout                              time.Duration
}

// LogoutInput is everything logout needs:
// the resolved settings, the issuer client for revocation, the store, and the two writers.
type LogoutInput struct {
	Settings Settings
	Issuer   *Issuer
	Store    *Store
	Stdout   io.Writer // the notice that nothing was cached
	Stderr   io.Writer // the revocation warning
}

// AuthDiagnostic wraps a 401 answered to a token the client believes valid with the issuer and the client identifier the token was obtained for,
// which is the audience under tokenType id.
type AuthDiagnostic struct {
	Issuer, ClientID string
	Err              error
}

func (d *AuthDiagnostic) Error() string {
	return fmt.Sprintf("%v; the token was obtained from %s for client %q, and the gateway refused it: its issuer or audience differs from what this token carries", d.Err, d.Issuer, d.ClientID)
}

func (d *AuthDiagnostic) Unwrap() error { return d.Err }

// PKCEFlag turns --pkce and --no-pkce into what LoginFlags carries:
// nil when neither was given, and a usage error when both were.
func PKCEFlag(pkce, noPKCE bool) (*bool, error) {
	switch {
	case pkce && noPKCE:
		return nil, fmt.Errorf("%w: --pkce and --no-pkce contradict each other", ErrUsage)
	case pkce:
		return &pkce, nil
	case noPKCE:
		v := false
		return &v, nil
	default:
		return nil, nil
	}
}

// authInfo is the /v1/auth body: the mode always, and the oidc object only
// where the operator configured the device login.
type authInfo struct {
	Mode string `json:"mode"`
	OIDC *struct {
		Issuer    string   `json:"issuer"`
		ClientID  string   `json:"clientID"`
		TokenType string   `json:"tokenType"`
		Scopes    []string `json:"scopes"`
		PKCE      bool     `json:"pkce"`
	} `json:"oidc"`
}

// whoamiBody is the part of GET /v1/whoami the login prints.
type whoamiBody struct {
	Principal string `json:"principal"`
	Realm     struct {
		Name string `json:"name"`
	} `json:"realm"`
}

// Login runs the whole login for the gateway's mode and returns the
// principal and realm it printed.
func Login(ctx context.Context, in LoginInput) (Whoami, error) {
	timeout, err := in.Flags.timeout()
	if err != nil {
		return Whoami{}, err
	}
	if in.Stdout == nil {
		in.Stdout = io.Discard
	}
	if in.Stderr == nil {
		in.Stderr = io.Discard
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	// The plaintext rule applies before the unauthenticated GET /v1/auth,
	// not only where a credential is sent: over http:// to a non-loopback
	// host the issuer that route names could be anyone's, and the device grant would run against it.
	if _, err := checkPlaintext(in.Settings.Server); err != nil {
		return Whoami{}, err
	}
	snap, err := gatewayAuth(ctx, in)
	if err != nil {
		return Whoami{}, err
	}
	var w Whoami
	switch snap.Mode {
	case "disabled":
		_, _ = fmt.Fprintln(in.Stdout, "the gateway authenticates nobody; no credential is needed")
	case "basic":
		w, err = loginBasic(ctx, in)
	default:
		w, err = loginOIDC(ctx, in, snap, timeout)
	}
	if err != nil {
		return Whoami{}, err
	}
	return w, recordLogin(in, snap)
}

// timeout applies the default and the 1m to 30m bound before any request.
func (f LoginFlags) timeout() (time.Duration, error) {
	if f.LoginTimeout == 0 {
		return defaultLoginTimeout, nil
	}
	if f.LoginTimeout < minLoginTimeout || f.LoginTimeout > maxLoginTimeout {
		return 0, fmt.Errorf("%w: --login-timeout %s is outside %s to %s", ErrUsage, f.LoginTimeout, minLoginTimeout, maxLoginTimeout)
	}
	return f.LoginTimeout, nil
}

// gatewayAuth reads GET /v1/auth and falls back, on 404 route_unknown or on
// an oidc body with no oidc object, to the context's auth block and then to
// the flags; each flag overrides what the route reported.
func gatewayAuth(ctx context.Context, in LoginInput) (AuthSnap, error) {
	snap, reported, err := readAuthInfo(ctx, in.Gateway)
	if err != nil {
		return AuthSnap{}, err
	}
	if !reported {
		snap = in.Settings.Context.Auth
	}
	if snap.Mode != "oidc" && snap.Mode != "" {
		return AuthSnap{Mode: snap.Mode}, nil
	}
	snap.Mode = "oidc"
	snap = in.Flags.apply(snap)
	if snap.IssuerCAFile == "" {
		snap.IssuerCAFile = in.Settings.IssuerCAFile
	}
	if snap.Issuer == "" || snap.ClientID == "" {
		return AuthSnap{}, fmt.Errorf("%w: the gateway does not report its login settings; write auth.mode, auth.issuer, auth.clientID, auth.tokenType, and auth.scopes into the context, or pass --issuer and --client-id", ErrUsage)
	}
	if snap.TokenType == "" {
		snap.TokenType = "id"
	}
	if len(snap.Scopes) == 0 {
		snap.Scopes = defaultScopes()
	}
	if err := validateAuth(snap); err != nil {
		return AuthSnap{}, fmt.Errorf("%w: auth.%w", ErrUsage, err)
	}
	return snap, nil
}

// readAuthInfo is the /v1/auth request; reported is false when the route is
// unknown or the body names oidc without the object, the two cases the
// fallback covers, and every other refusal is returned as it is.
func readAuthInfo(ctx context.Context, gw *Client) (AuthSnap, bool, error) {
	body, _, err := gw.JSON(ctx, Request{Method: http.MethodGet, Path: "/v1/auth"})
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Code == "route_unknown" {
			return AuthSnap{}, false, nil
		}
		return AuthSnap{}, false, err
	}
	var info authInfo
	if err := decodeOne(body, &info); err != nil {
		return AuthSnap{}, false, fmt.Errorf("/v1/auth: %w", err)
	}
	switch info.Mode {
	case "disabled", "basic":
		return AuthSnap{Mode: info.Mode}, true, nil
	case "oidc":
		if info.OIDC == nil {
			return AuthSnap{}, false, nil
		}
		return AuthSnap{
			Mode:      "oidc",
			Issuer:    info.OIDC.Issuer,
			ClientID:  info.OIDC.ClientID,
			TokenType: info.OIDC.TokenType,
			Scopes:    info.OIDC.Scopes,
			PKCE:      info.OIDC.PKCE,
		}, true, nil
	default:
		return AuthSnap{}, false, fmt.Errorf("/v1/auth reports mode %q, which is not one of disabled, basic, oidc", info.Mode)
	}
}

// apply overrides each field a flag set.
func (f LoginFlags) apply(s AuthSnap) AuthSnap {
	if f.Issuer != "" {
		s.Issuer = f.Issuer
	}
	if f.ClientID != "" {
		s.ClientID = f.ClientID
	}
	if f.TokenType != "" {
		s.TokenType = f.TokenType
	}
	if len(f.Scopes) > 0 {
		s.Scopes = f.Scopes
	}
	if f.PKCE != nil {
		s.PKCE = *f.PKCE
	}
	if f.IssuerCAFile != "" {
		s.IssuerCAFile = f.IssuerCAFile
	}
	return s
}

// loginOIDC is the device grant:
// discovery, the device request, the two lines on stderr, the poll, the cache write under the lock, and whoami.
func loginOIDC(ctx context.Context, in LoginInput, snap AuthSnap, timeout time.Duration) (Whoami, error) {
	m, err := in.Issuer.Discover(ctx, snap.Issuer)
	if err != nil {
		return Whoami{}, err
	}
	d, err := in.Issuer.Authorize(ctx, m, snap.ClientID, snap.Scopes, snap.PKCE)
	if err != nil {
		return Whoami{}, err
	}
	// No browser is opened: printing is what works over SSH, in a container,
	// and on a machine with no graphical session.
	_, _ = fmt.Fprintf(in.Stderr, "Enter the code %s\nat %s\n", d.UserCode, d.VerificationURI)
	if d.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(in.Stderr, "or open %s\n", d.VerificationURIComplete)
	}
	start := in.Now()
	deadline := start.Add(time.Duration(d.ExpiresIn) * time.Second)
	if byTimeout := start.Add(timeout); byTimeout.Before(deadline) {
		deadline = byTimeout
	}
	tr, err := in.Issuer.Poll(ctx, m, snap.ClientID, d, deadline)
	if err != nil {
		return Whoami{}, err
	}
	token := tr.AccessToken
	if snap.TokenType == "id" {
		token = tr.IDToken
	}
	if token == "" {
		return Whoami{}, fmt.Errorf("the issuer's response carries no %s token; the client %q may be registered without the scope that grants one", snap.TokenType, snap.ClientID)
	}
	now := in.Now()
	expiresAt, err := ExpiryOf(snap.TokenType, token, tr.ExpiresIn, now)
	if err != nil {
		return Whoami{}, fmt.Errorf("%w: %w", err, ErrLoginNeeded)
	}
	e := Entry{
		Origin:           in.Settings.Origin,
		Issuer:           snap.Issuer,
		ClientID:         snap.ClientID,
		TokenType:        snap.TokenType,
		Token:            token,
		ExpiresAt:        expiresAt,
		RefreshToken:     tr.RefreshToken,
		RefreshExpiresAt: RefreshExpiryOf(tr.RefreshExpiresIn, now),
		ObtainedAt:       now,
	}
	if err := writeEntry(ctx, in, e); err != nil {
		return Whoami{}, err
	}
	w, err := fetchWhoami(ctx, in.Gateway.withCredential(tokenCredential(token)))
	if err != nil {
		if unauthorized(err) {
			return Whoami{}, &AuthDiagnostic{Issuer: snap.Issuer, ClientID: snap.ClientID, Err: err}
		}
		return Whoami{}, err
	}
	printWhoami(in.Stdout, w)
	return w, nil
}

// writeEntry writes the cache under the lock.
func writeEntry(ctx context.Context, in LoginInput, e Entry) (err error) {
	release, err := in.Store.Lock(ctx, in.Settings.CacheName)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := release(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	return in.Store.Write(in.Settings.CacheName, e)
}

// loginBasic verifies the prompted credential against whoami, stores nothing, and says which two variables the next command reads.
func loginBasic(ctx context.Context, in LoginInput) (Whoami, error) {
	if in.Basic == nil {
		return Whoami{}, fmt.Errorf("%w: the gateway authenticates with a user name and password, and no prompt is available", ErrUsage)
	}
	user, password, err := in.Basic()
	if err != nil {
		return Whoami{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	cred, err := BasicCredential(user, password)
	if err != nil {
		return Whoami{}, err
	}
	w, err := fetchWhoami(ctx, in.Gateway.withCredential(cred))
	if err != nil {
		return Whoami{}, err
	}
	printWhoami(in.Stdout, w)
	_, _ = fmt.Fprintln(in.Stderr, "nothing was stored: the next command reads PROFGATE_USER and PROFGATE_PASSWORD, or -u with a prompt")
	return w, nil
}

// recordLogin makes the selected context's auth block the snapshot the login used and writes the file;
// --server alone with no context writes nothing.
// The cache entry stays when the write fails: the login succeeded and the
// snapshot did not record it.
func recordLogin(in LoginInput, snap AuthSnap) error {
	if in.SaveFile == nil || in.Settings.ContextName == "" {
		return nil
	}
	f := in.File
	if f == nil {
		f = &File{}
	}
	f.RecordLogin(in.Settings, snap)
	if err := in.SaveFile(f); err != nil {
		return fmt.Errorf("the login succeeded and its token is cached, but the contexts file was not updated: %w", err)
	}
	return nil
}

// fetchWhoami is GET /v1/whoami as the client's principal.
func fetchWhoami(ctx context.Context, c *Client) (Whoami, error) {
	body, _, err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/v1/whoami"})
	if err != nil {
		return Whoami{}, err
	}
	var b whoamiBody
	if err := decodeOne(body, &b); err != nil {
		return Whoami{}, fmt.Errorf("whoami: %w", err)
	}
	return Whoami{Principal: b.Principal, Realm: b.Realm.Name}, nil
}

func printWhoami(w io.Writer, who Whoami) {
	_, _ = fmt.Fprintf(w, "principal: %s\nrealm: %s\n", who.Principal, who.Realm)
}

// unauthorized reports a 401, whether or not the body was the envelope.
func unauthorized(err error) bool {
	var ae *APIError
	var se *StatusError
	return (errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) ||
		(errors.As(err, &se) && se.Status == http.StatusUnauthorized)
}

// Logout takes the lock, revokes the refresh token where the issuer publishes an endpoint, and deletes the entry.
// A revocation failure is a warning, because a local credential outliving a
// failed revocation is the worse outcome; a deletion failure says the
// credential remains and names the file.
func Logout(ctx context.Context, in LogoutInput) (err error) {
	if in.Stdout == nil {
		in.Stdout = io.Discard
	}
	if in.Stderr == nil {
		in.Stderr = io.Discard
	}
	name := in.Settings.CacheName
	release, err := in.Store.Lock(ctx, name)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := release(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	e, ok, err := in.Store.Read(name)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintf(in.Stdout, "nothing is cached for %s\n", in.Settings.describe())
		return nil
	}
	if e.RefreshToken != "" {
		revoke(ctx, in, e)
	}
	if err := in.Store.Delete(name); err != nil {
		return fmt.Errorf("the credential remains on disk; remove %s: %w", in.Store.path(name, ".json"), err)
	}
	return nil
}

// revoke posts the refresh token to the revocation endpoint when discovery publishes one, and warns on stderr when the issuer cannot be reached or refuses.
func revoke(ctx context.Context, in LogoutInput, e Entry) {
	m, err := in.Issuer.Discover(ctx, e.Issuer)
	if err != nil {
		_, _ = fmt.Fprintf(in.Stderr, "profgate: warning: the refresh token was not revoked: %v\n", err)
		return
	}
	if m.RevocationEndpoint == "" {
		return
	}
	if err := in.Issuer.Revoke(ctx, m, e.ClientID, e.RefreshToken); err != nil {
		_, _ = fmt.Fprintf(in.Stderr, "profgate: warning: the refresh token was not revoked: %v\n", err)
	}
}

// describe names the selected context, or the gateway origin when none is.
func (s Settings) describe() string {
	if s.ContextName != "" {
		return "context " + s.ContextName
	}
	return s.Origin
}
