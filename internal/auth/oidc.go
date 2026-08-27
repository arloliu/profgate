package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// bearerScheme is the RFC 6750 scheme token, compared case-insensitively.
const bearerScheme = "Bearer"

// OIDCOptions is what OIDC needs from the outside.
type OIDCOptions struct {
	Logger   *slog.Logger
	Recorder metrics.Recorder
	// PollInterval replaces the 30-second cookie-key-file poll; zero keeps it.
	// Mirrors tlscert.Options.Interval.
	PollInterval time.Duration
}

// OIDC is the oidc-mode authenticator: bearer tokens always, the browser
// flow when configured.
// Trust is the startup snapshot: the issuer, the audience, the CA, and the
// token profile are read once by NewOIDC; the mapping comes from the
// configuration snapshot each request carries.
type OIDC struct {
	cfg    *config.OIDCConfig // the startup snapshot
	client *issuerClient
	// pollInterval is handed to the cookie key file poller.
	pollInterval time.Duration
	// state is nil until Discover succeeds and is replaced whole on every
	// later success; the verifier loads it per request.
	state    atomic.Pointer[issuerState]
	verifier *verifier
	// browser is the relying party, nil unless the browser block is
	// configured.
	browser *browser
	now     func() time.Time
	log     *slog.Logger
	rec     metrics.Recorder
}

// NewOIDC builds the client, the verifier, and the relying party from the
// startup snapshot; it performs no network I/O.
// With the browser block it reads the client secret and the cookie key file
// once, and a file that cannot be read or parsed fails construction.
// The key cache is not built here: its fetcher is bound to the jwks_uri that
// discovery returns.
func NewOIDC(cfg *config.Config, opts OIDCOptions) (*OIDC, error) {
	oc := cfg.Auth.OIDC
	if oc == nil {
		return nil, errors.New("auth: oidc mode needs auth.oidc")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Recorder == nil {
		opts.Recorder = metrics.Noop{}
	}
	client, err := newIssuerClient(issuerOptions{CAFile: oc.CAFile, HTTPProxy: oc.HTTPProxy})
	if err != nil {
		return nil, err
	}
	o := &OIDC{
		cfg:          oc,
		client:       client,
		pollInterval: opts.PollInterval,
		now:          time.Now,
		log:          opts.Logger,
		rec:          opts.Recorder,
	}
	o.verifier = &verifier{
		issuer:        oc.Issuer,
		audience:      oc.Audience,
		tokenType:     oc.TokenType,
		usernameClaim: oc.UsernameClaim,
		groupsClaim:   oc.GroupsClaim,
		skew:          oc.ClockSkew,
		state:         &o.state,
		now:           o.now,
	}
	if oc.Browser != nil {
		if o.browser, err = newBrowser(o, oc.Browser); err != nil {
			return nil, err
		}
	}

	return o, nil
}

// Discover fetches and validates the discovery document, then the key set
// behind its jwks_uri, and publishes both as one issuerState only when both
// succeeded; the caller retries with backoff.
// A failure at any step stores nothing, so the previous state (nil before
// the first success) stays in force and no endpoint becomes usable between
// a good document and a bad key set.
func (o *OIDC) Discover(ctx context.Context) error {
	doc, err := discover(ctx, o.client, o.cfg.Issuer, o.browser != nil)
	if err != nil {
		return err
	}
	cache := newJWKSCache(&httpKeyFetcher{client: o.client, url: doc.JWKSURI}, o.cfg, o.now, o.log, o.rec)
	if err := cache.Refresh(ctx); err != nil {
		return err
	}
	o.state.Store(&issuerState{doc: doc, keys: cache})

	return nil
}

// Run drives the refresh timer of the published key cache and, with the
// browser flow, the cookie key file poller, until ctx ends.
// The caller starts it only after Discover succeeded; it returns at once
// when nothing is published.
func (o *OIDC) Run(ctx context.Context) {
	st := o.state.Load()
	if st == nil {
		return
	}
	var wg sync.WaitGroup
	if o.browser != nil {
		wg.Go(func() { o.browser.keyFile.Run(ctx) })
	}
	st.keys.Run(ctx)
	wg.Wait()
}

// Authenticate implements Authenticator.
// An Authorization header is judged on its own: the scheme must be Bearer,
// the token must verify, and the claims must map to a realm.
// Without one, a session cookie is judged when the browser flow is
// configured; otherwise there is no credential, and a browser navigation
// is sent to login.
func (o *OIDC) Authenticate(ctx context.Context, r *http.Request, cfg *config.Config) (Principal, error) {
	if cfg.Auth.OIDC == nil {
		return Principal{}, errors.New("auth: oidc mode needs auth.oidc")
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		if o.browser != nil {
			if c, err := r.Cookie(cookieSession); err == nil {
				return o.browser.session(r, c.Value)
			}
		}

		return Principal{}, &Failure{Status: http.StatusUnauthorized, Reason: ReasonMissing, Redirect: o.browser.redirect(r)}
	}
	scheme, token, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, bearerScheme) {
		return Principal{}, &Failure{Status: http.StatusUnauthorized, Reason: ReasonScheme}
	}
	c, fail := o.verifier.verify(ctx, strings.TrimSpace(token))
	if fail != nil {
		return Principal{}, fail
	}
	realm, ok := mapRealm(cfg.Auth.OIDC.Mapping, c)
	if !ok {
		return Principal{}, &Failure{Status: http.StatusUnauthorized, Reason: ReasonNoRealm}
	}

	return Principal{Name: c.Username, Realm: realm}, nil
}
