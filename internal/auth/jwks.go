package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// minRSABits is the smallest RSA modulus a signing key may carry.
const minRSABits = 2048

// allowedAlgs are the signature algorithms a token may name and a key may
// carry; none and every HMAC algorithm are outside it.
var allowedAlgs = [...]jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
}

// signatureAlgs returns a copy of the allowed signature algorithms.
func signatureAlgs() []jose.SignatureAlgorithm {
	return slices.Clone(allowedAlgs[:])
}

// algAllowed reports whether alg is one of the allowed signature algorithms.
func algAllowed(alg string) bool {
	return slices.Contains(allowedAlgs[:], jose.SignatureAlgorithm(alg))
}

// keyFetcher is what the cache calls; the tests program it and production
// is httpKeyFetcher.
type keyFetcher interface {
	fetch(ctx context.Context) (jose.JSONWebKeySet, error)
}

// httpKeyFetcher fetches one jwks_uri through the issuer client.
// Discover builds one per attempt, bound to the jwks_uri the document it
// just validated names.
type httpKeyFetcher struct {
	client *issuerClient
	url    string
}

func (f *httpKeyFetcher) fetch(ctx context.Context) (jose.JSONWebKeySet, error) {
	var set jose.JSONWebKeySet
	if err := f.client.getJSON(ctx, f.url, &set); err != nil {
		return jose.JSONWebKeySet{}, err
	}

	return set, nil
}

// issuerState is everything discovery produced, published as one value:
// the validated document and a key cache that has fetched at least one
// usable key.
// OIDC holds it behind an atomic pointer that is nil until the first
// successful Discover; the verifier and the browser routes load that pointer
// per request, so no endpoint is usable before the keys behind it are, and
// a retry replaces the whole value.
type issuerState struct {
	doc  discoveryDocument
	keys *jwksCache
}

// keySet is one immutable, validated snapshot of the issuer's usable keys.
// A verification loads one pointer and uses it for staleness, selection,
// and the signature check.
type keySet struct {
	byKID   map[string]jose.JSONWebKey
	all     []jose.JSONWebKey
	fetched time.Time
}

// stale reports whether this set is older than maxStale at now; a nil set,
// one never fetched, is stale.
func (k *keySet) stale(now time.Time, maxStale time.Duration) bool {
	return k == nil || now.Sub(k.fetched) > maxStale
}

// jwksCache holds the issuer's signing keys and replaces them on a timer,
// or on demand under a cooldown shared by every request.
type jwksCache struct {
	fetcher    keyFetcher
	now        func() time.Time
	refresh    time.Duration // timer interval
	refreshMin time.Duration // on-demand cooldown
	maxStale   time.Duration
	cur        atomic.Pointer[keySet]
	// fetchMu serializes fetch-and-store, so a slow response never lands
	// after a faster, later one and puts a retired set back with a newer
	// fetched time; the timer and every on-demand caller take it.
	fetchMu sync.Mutex
	// mu guards lastAttempt, the clock reading of the last on-demand refresh.
	mu          sync.Mutex
	lastAttempt time.Time
	log         *slog.Logger
	rec         metrics.Recorder
}

// newJWKSCache builds a cache over fetcher with an empty set; nothing is
// fetched until Refresh.
func newJWKSCache(fetcher keyFetcher, cfg *config.OIDCConfig, now func() time.Time, log *slog.Logger, rec metrics.Recorder) *jwksCache {
	return &jwksCache{
		fetcher:    fetcher,
		now:        now,
		refresh:    cfg.JWKSRefresh,
		refreshMin: cfg.JWKSRefreshMin,
		maxStale:   cfg.JWKSMaxStale,
		log:        log,
		rec:        rec,
	}
}

// Refresh fetches once and swaps the set on success; the timer and the
// tests call it.
// A fetch succeeds only when it yields at least one usable key and no
// duplicate kid; anything else leaves the previous set in place, logs at
// warn, and counts a failed refresh.
// Refreshes run one at a time: a caller that arrives while one is in flight
// waits for it and then fetches again, so the set stored last is always the
// one fetched last.
func (c *jwksCache) Refresh(ctx context.Context) error {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	raw, err := c.fetcher.fetch(ctx)
	if err == nil {
		var set *keySet
		if set, err = usableKeys(raw, c.now(), c.log); err == nil {
			c.cur.Store(set)
			c.rec.JWKSRefresh("ok")
			c.rec.JWKSKeys(len(set.all))
			c.rec.JWKSFetched(set.fetched)
			c.log.Info("signing keys fetched", "keys", len(set.all))

			return nil
		}
	}
	c.rec.JWKSRefresh("failed")
	c.log.Warn("signing key fetch failed; keeping the previous set", "error", err)

	return fmt.Errorf("auth: signing keys: %w", err)
}

// refreshOnDemand runs Refresh at most once per refreshMin across all
// callers and reports whether it ran.
// The attempt is recorded before the fetch starts, so callers arriving while
// one is in flight are inside the cooldown and do not fetch again.
func (c *jwksCache) refreshOnDemand(ctx context.Context) bool {
	now := c.now()
	c.mu.Lock()
	if !c.lastAttempt.IsZero() && now.Sub(c.lastAttempt) < c.refreshMin {
		c.mu.Unlock()

		return false
	}
	c.lastAttempt = now
	c.mu.Unlock()
	_ = c.Refresh(ctx) // already logged and counted; the caller reloads the set either way

	return true
}

// Run drives Refresh every refresh interval until ctx ends.
func (c *jwksCache) Run(ctx context.Context) {
	ticker := time.NewTicker(c.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = c.Refresh(ctx) // already logged and counted
		case <-ctx.Done():
			return
		}
	}
}

// current returns the set the last successful Refresh stored, or nil.
func (c *jwksCache) current() *keySet {
	return c.cur.Load()
}

// stale is current().stale(now(), maxStale), for the tests; the verifier
// asks the set it loaded instead.
func (c *jwksCache) stale() bool {
	return c.current().stale(c.now(), c.maxStale)
}

// usableKeys filters raw to the usable keys and indexes them by kid.
// An unusable key is dropped with a warning naming its kid; two usable keys
// sharing a kid reject the set as a whole, because a kid that names two
// keys names none; a set with no usable key is a failed fetch.
func usableKeys(raw jose.JSONWebKeySet, fetched time.Time, log *slog.Logger) (*keySet, error) {
	set := &keySet{byKID: make(map[string]jose.JSONWebKey, len(raw.Keys)), fetched: fetched}
	for _, k := range raw.Keys {
		if why := unusable(k); why != "" {
			log.Warn("signing key dropped", "kid", k.KeyID, "reason", why)

			continue
		}
		if k.KeyID != "" {
			if _, dup := set.byKID[k.KeyID]; dup {
				return nil, fmt.Errorf("two usable keys share kid %q", k.KeyID)
			}
			set.byKID[k.KeyID] = k
		}
		set.all = append(set.all, k)
	}
	if len(set.all) == 0 {
		return nil, errors.New("no usable key in the set")
	}

	return set, nil
}

// unusable says why a key cannot verify a token, or "" when it can.
func unusable(k jose.JSONWebKey) string {
	if k.Key == nil {
		return "key type is not understood"
	}
	if k.Use != "" && k.Use != "sig" {
		return "use is not sig"
	}
	switch key := k.Key.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < minRSABits {
			return fmt.Sprintf("RSA modulus under %d bits", minRSABits)
		}
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() && key.Curve != elliptic.P521() {
			return "EC curve is not P-256, P-384, or P-521"
		}
	default:
		return "kty is not RSA or EC"
	}
	if !k.Valid() {
		return "public key does not parse"
	}
	if k.Algorithm != "" && !compatible(k.Algorithm, k) {
		return "alg is not allowed for this key"
	}

	return ""
}

// compatible reports whether a token's alg may be verified with k:
// RS* and PS* need an RSA key; ES256 needs P-256, ES384 P-384, ES512 P-521;
// a key that carries alg must carry the token's.
func compatible(alg string, k jose.JSONWebKey) bool {
	if !algAllowed(alg) {
		return false
	}
	if k.Algorithm != "" && k.Algorithm != alg {
		return false
	}
	switch key := k.Key.(type) {
	case *rsa.PublicKey:
		return alg[0] == 'R' || alg[0] == 'P'
	case *ecdsa.PublicKey:
		return key.Curve == curveFor(alg)
	default:
		return false
	}
}

// curveFor names the curve an ES* algorithm signs on, or nil.
func curveFor(alg string) elliptic.Curve {
	switch alg {
	case "ES256":
		return elliptic.P256()
	case "ES384":
		return elliptic.P384()
	case "ES512":
		return elliptic.P521()
	default:
		return nil
	}
}
