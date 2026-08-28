package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// refreshWindow is how close to expiry a cached token is refreshed rather
// than sent.
const refreshWindow = 30 * time.Second

// ErrLoginNeeded is what a Credential returns when no usable token exists and
// none can be obtained without a login: the process exits 3 on it.
var ErrLoginNeeded = errors.New("no valid token; run profgate login")

// Refresh posts grant_type=refresh_token with client_id.
func (i *Issuer) Refresh(ctx context.Context, m Metadata, clientID, refreshToken string) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	return i.postForm(ctx, "token", m.TokenEndpoint, form)
}

// Revoke posts the refresh token with token_type_hint=refresh_token and
// client_id, which is how a public client identifies itself there (RFC 7009).
// A 4xx is returned as an *IssuerError and a failure to connect as a
// *TransportError; the caller decides that either is a warning.
func (i *Issuer) Revoke(ctx context.Context, m Metadata, clientID, refreshToken string) error {
	if m.RevocationEndpoint == "" {
		return fmt.Errorf("the issuer %s publishes no revocation_endpoint", m.Issuer)
	}
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	}
	_, err := i.postFormBody(ctx, "revocation", m.RevocationEndpoint, form)
	return err
}

// permanent reports whether an issuer 4xx ends the refresh token's life.
// Every 4xx does except 429, which is the issuer asking for time rather than
// refusing the grant.
func permanent(err *IssuerError) bool {
	return err.Status != http.StatusTooManyRequests
}

// cachedCredential is the cached token for one resolved gateway.
type cachedCredential struct {
	store    *Store
	issuer   *Issuer
	settings Settings
	now      func() time.Time
	// applied is the entry the last Apply sent, which diagnose reads.
	applied *Entry
}

// diagnose wraps a 401 answered to the applied token with the issuer and
// client identifier it was obtained for, so a reconfigured gateway is
// diagnosed at the first refusal.
func (c *cachedCredential) diagnose(err error) error {
	if c.applied == nil {
		return err
	}
	return &AuthDiagnostic{Issuer: c.applied.Issuer, ClientID: c.applied.ClientID, Err: err}
}

// CachedCredential is the cached token for the resolved gateway, refreshed
// under the lock when it is within 30 seconds of expiry.
func CachedCredential(s *Store, iss *Issuer, set Settings, now func() time.Time) Credential {
	if now == nil {
		now = time.Now
	}
	return &cachedCredential{store: s, issuer: iss, settings: set, now: now}
}

// Apply sets the bearer token, refreshing it first when it is inside the
// window.
// The entry is read and checked before anything else; a token more than 30
// seconds from expiry is sent and no lock is taken.
func (c *cachedCredential) Apply(ctx context.Context, r *http.Request) error {
	e, ok, err := c.store.Read(c.settings.CacheName)
	if err != nil {
		return err
	}
	if !ok {
		return c.loginNeeded()
	}
	if err := e.Usable(c.settings); err != nil {
		return fmt.Errorf("%w: %w", err, c.loginNeeded())
	}
	c.applied = &e
	if c.fresh(e) {
		r.Header.Set("Authorization", "Bearer "+e.Token)
		return nil
	}
	if e.RefreshToken == "" || (!e.RefreshExpiresAt.IsZero() && !e.RefreshExpiresAt.After(c.now())) {
		return c.loginNeeded()
	}
	token, err := c.refresh(ctx)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// fresh reports whether the entry's token is more than the window from expiry.
func (c *cachedCredential) fresh(e Entry) bool {
	return e.ExpiresAt.After(c.now().Add(refreshWindow))
}

// refresh takes the lock, reads the entry again, and refreshes only an entry
// still inside the window; the response decides what the cache becomes.
func (c *cachedCredential) refresh(ctx context.Context) (token string, err error) {
	name := c.settings.CacheName
	release, err := c.store.Lock(ctx, name)
	if err != nil {
		return "", err
	}
	defer func() {
		if rerr := release(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	e, ok, err := c.store.Read(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", c.loginNeeded()
	}
	if err := e.Usable(c.settings); err != nil {
		return "", fmt.Errorf("%w: %w", err, c.loginNeeded())
	}
	if c.fresh(e) {
		// Another process refreshed while this one waited for the lock.
		return e.Token, nil
	}
	if e.RefreshToken == "" {
		return "", c.loginNeeded()
	}
	m, err := c.issuer.Discover(ctx, e.Issuer)
	if err != nil {
		return "", err
	}
	tr, err := c.issuer.Refresh(ctx, m, e.ClientID, e.RefreshToken)
	if err != nil {
		return "", c.refused(e, err)
	}
	return c.record(e, tr)
}

// refused handles a failed refresh: a permanent 4xx deletes the file, and
// only when the re-read shows the same obtainedAt the refused refresh token
// came from, because a filesystem that ignores the lock may have let another
// process write a newer entry meanwhile; everything else leaves the file as
// it was.
func (c *cachedCredential) refused(e Entry, err error) error {
	var ie *IssuerError
	if !errors.As(err, &ie) || !permanent(ie) {
		return err
	}
	current, ok, rerr := c.store.Read(c.settings.CacheName)
	if rerr != nil {
		return rerr
	}
	if ok && current.ObtainedAt.Equal(e.ObtainedAt) {
		if derr := c.store.Delete(c.settings.CacheName); derr != nil {
			return derr
		}
	}
	return fmt.Errorf("the refresh was refused (%w): %w", ie, c.loginNeeded())
}

// record writes what a successful refresh changed: the token the token type
// names when the response carries it, the rotated refresh token when it
// carries one, and the two expiries from the response time.
// Without the selected token the old one is kept until its own expiry, and
// without a rotation or a refresh lifetime the recorded refresh expiry is
// kept rather than erased.
func (c *cachedCredential) record(e Entry, tr TokenResponse) (string, error) {
	now := c.now()
	name := c.settings.CacheName
	fresh := tr.AccessToken
	if e.TokenType == "id" {
		fresh = tr.IDToken
	}
	if fresh != "" {
		expiresAt, err := ExpiryOf(e.TokenType, fresh, tr.ExpiresIn, now)
		if err != nil {
			return "", fmt.Errorf("%w: %w", err, c.loginNeeded())
		}
		e.Token = fresh
		e.ExpiresAt = expiresAt
	}
	if tr.RefreshToken != "" {
		e.RefreshToken = tr.RefreshToken
	}
	if tr.RefreshToken != "" || tr.RefreshExpiresIn > 0 {
		e.RefreshExpiresAt = RefreshExpiryOf(tr.RefreshExpiresIn, now)
	}
	e.ObtainedAt = now
	if err := c.store.Write(name, e); err != nil {
		return "", err
	}
	if fresh == "" && !e.ExpiresAt.After(now) {
		return "", fmt.Errorf("the refresh returned no %s token and the cached one expired: %w", e.TokenType, c.loginNeeded())
	}
	return e.Token, nil
}

// loginNeeded is ErrLoginNeeded naming the context when one is selected.
func (c *cachedCredential) loginNeeded() error {
	if c.settings.ContextName == "" {
		return ErrLoginNeeded
	}
	return fmt.Errorf("context %q: %w", c.settings.ContextName, ErrLoginNeeded)
}
