package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// packageKeys holds the signing keys every test in the package shares; they
// are generated once because RSA generation is the slow part of the suite.
type packageKeys struct {
	rsa2048 *rsa.PrivateKey
	rsa1024 *rsa.PrivateKey
	p256    *ecdsa.PrivateKey
	p384    *ecdsa.PrivateKey
}

var generateKeys = sync.OnceValues(func() (*packageKeys, error) {
	k := &packageKeys{}
	var err error
	if k.rsa2048, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		return nil, err
	}
	if k.rsa1024, err = rsa.GenerateKey(rand.Reader, 1024); err != nil { //nolint:gosec // the weak key is what the usability filter must drop
		return nil, err
	}
	if k.p256, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		return nil, err
	}
	if k.p384, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader); err != nil {
		return nil, err
	}

	return k, nil
})

func testKeys(t *testing.T) *packageKeys {
	t.Helper()
	k, err := generateKeys()
	if err != nil {
		t.Fatalf("generate keys: %v", err)
	}

	return k
}

// fakeFetcher returns the programmed set or error and counts calls.
type fakeFetcher struct {
	mu    sync.Mutex
	set   jose.JSONWebKeySet
	err   error
	calls int
}

func (f *fakeFetcher) fetch(context.Context) (jose.JSONWebKeySet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return jose.JSONWebKeySet{}, f.err
	}

	return f.set, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *fakeFetcher) program(set jose.JSONWebKeySet, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set, f.err = set, err
}

// gatedFetcher passes every fetch to inner. The first fetch reads its
// result from inner, closes entered, and then holds that result until
// release is closed, so a set programmed afterwards is not what it returns.
// Later fetches pass straight through, so the gate itself never serializes
// two callers; only the cache can.
type gatedFetcher struct {
	inner   *fakeFetcher
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (g *gatedFetcher) fetch(ctx context.Context) (jose.JSONWebKeySet, error) {
	g.mu.Lock()
	g.calls++
	first := g.calls == 1
	g.mu.Unlock()
	set, err := g.inner.fetch(ctx)
	if first {
		close(g.entered)
		<-g.release
	}

	return set, err
}

// jwksRecorder records the signing-key metrics.
type jwksRecorder struct {
	metrics.Noop
	mu      sync.Mutex
	refresh []string
	keys    []int
	fetched []time.Time
}

func (r *jwksRecorder) JWKSRefresh(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh = append(r.refresh, result)
}

func (r *jwksRecorder) JWKSKeys(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, n)
}

func (r *jwksRecorder) JWKSFetched(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetched = append(r.fetched, at)
}

// jwksFixture is one cache over a fake fetcher and a clock the test moves.
type jwksFixture struct {
	cache *jwksCache
	fetch *fakeFetcher
	rec   *jwksRecorder
	logs  *bytes.Buffer
	mu    sync.Mutex
	now   time.Time
}

func newJWKSFixture(t *testing.T, set jose.JSONWebKeySet) *jwksFixture {
	t.Helper()
	f := &jwksFixture{
		fetch: &fakeFetcher{set: set},
		rec:   &jwksRecorder{},
		logs:  &bytes.Buffer{},
		now:   time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
	}
	cfg := &config.OIDCConfig{
		JWKSRefresh:    time.Hour,
		JWKSRefreshMin: time.Minute,
		JWKSMaxStale:   24 * time.Hour,
	}
	f.cache = newJWKSCache(f.fetch, cfg, f.clock, slog.New(slog.NewTextHandler(f.logs, nil)), f.rec)

	return f
}

func (f *jwksFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

func (f *jwksFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func goodSet(t *testing.T) jose.JSONWebKeySet {
	t.Helper()
	keys := testKeys(t)

	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &keys.rsa2048.PublicKey, KeyID: "rsa", Use: "sig"},
		{Key: &keys.p256.PublicKey, KeyID: "ec"},
	}}
}

func kids(ks *keySet) []string {
	if ks == nil {
		return nil
	}
	out := make([]string, 0, len(ks.all))
	for _, k := range ks.all {
		out = append(out, k.KeyID)
	}

	return out
}

func TestJWKSCacheUsable(t *testing.T) {
	keys := testKeys(t)

	t.Run("usable filter", func(t *testing.T) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &keys.rsa2048.PublicKey, KeyID: "good-rsa", Use: "sig"},
			{Key: &keys.p256.PublicKey, KeyID: "good-ec"},
			{Key: &keys.rsa2048.PublicKey, KeyID: "enc-key", Use: "enc"},
			{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "oct-key"},
			{Key: &keys.rsa1024.PublicKey, KeyID: "weak-rsa"},
			{Key: &keys.p256.PublicKey, KeyID: "hmac-alg", Algorithm: "HS256"},
		}}
		f := newJWKSFixture(t, set)
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		cur := f.cache.current()
		if got := kids(cur); len(got) != 2 || got[0] != "good-rsa" || got[1] != "good-ec" {
			t.Fatalf("held kids = %v, want [good-rsa good-ec]", got)
		}
		if _, ok := cur.byKID["good-rsa"]; !ok {
			t.Fatal("good-rsa not indexed by kid")
		}
		for _, dropped := range []string{"enc-key", "oct-key", "weak-rsa", "hmac-alg"} {
			if !strings.Contains(f.logs.String(), dropped) {
				t.Errorf("no warning names dropped key %q; log:\n%s", dropped, f.logs.String())
			}
		}
		if len(f.rec.keys) != 1 || f.rec.keys[0] != 2 {
			t.Fatalf("JWKSKeys calls = %v, want [2]", f.rec.keys)
		}
	})

	failing := []struct {
		name string
		set  jose.JSONWebKeySet
	}{
		{"empty set", jose.JSONWebKeySet{}},
		{"all weak", jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &keys.rsa1024.PublicKey, KeyID: "w1"},
			{Key: &keys.rsa1024.PublicKey, KeyID: "w2"},
		}}},
		{"duplicate kid", jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &keys.rsa2048.PublicKey, KeyID: "same"},
			{Key: &keys.p256.PublicKey, KeyID: "same"},
		}}},
	}
	for _, tc := range failing {
		t.Run(tc.name, func(t *testing.T) {
			f := newJWKSFixture(t, goodSet(t))
			if err := f.cache.Refresh(context.Background()); err != nil {
				t.Fatalf("first Refresh: %v", err)
			}
			before := f.cache.current()
			f.fetch.program(tc.set, nil)
			if err := f.cache.Refresh(context.Background()); err == nil {
				t.Fatal("Refresh accepted a set with no usable keys")
			}
			if f.cache.current() != before {
				t.Fatal("a failed fetch replaced the previous set")
			}
			if got := f.rec.refresh; len(got) != 2 || got[0] != "ok" || got[1] != "failed" {
				t.Fatalf("JWKSRefresh calls = %v, want [ok failed]", got)
			}
		})
	}

	t.Run("one bad among good", func(t *testing.T) {
		set := goodSet(t)
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &keys.rsa1024.PublicKey, KeyID: "weak"})
		f := newJWKSFixture(t, set)
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if got := kids(f.cache.current()); len(got) != 2 {
			t.Fatalf("held kids = %v, want the two usable ones", got)
		}
	})
}

func TestJWKSCacheRefresh(t *testing.T) {
	t.Run("swap is atomic", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		loaded := f.cache.current()
		keys := testKeys(t)
		f.fetch.program(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &keys.p384.PublicKey, KeyID: "new"}}}, nil)
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := kids(loaded); len(got) != 2 || got[0] != "rsa" {
			t.Fatalf("the set loaded before Refresh now holds %v", got)
		}
		if got := kids(f.cache.current()); len(got) != 1 || got[0] != "new" {
			t.Fatalf("current holds %v, want [new]", got)
		}
	})

	// Two refreshes overlap: the first fetch has read the old set and is
	// held open while a second caller arrives with a newer set. Refreshes
	// are serialized, so the second fetch begins only after the first
	// stored its old set, and the set fetched last is the one that stays
	// current. Without the serialization the second would store the new set
	// first and the released first fetch would put the old set back.
	t.Run("serialized", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		keys := testKeys(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		gate := &gatedFetcher{inner: f.fetch, entered: entered, release: release}
		f.cache.fetcher = gate

		first := make(chan error, 1)
		go func() { first <- f.cache.Refresh(context.Background()) }()
		<-entered // the first fetch holds the old set and is blocked

		newer := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &keys.p384.PublicKey, KeyID: "new"}}}
		second := make(chan error, 1)
		programmed := make(chan struct{})
		go func() {
			f.fetch.program(newer, nil)
			f.advance(time.Minute)
			close(programmed)
			second <- f.cache.Refresh(context.Background())
		}()
		<-programmed // the newer set is in place before the first fetch is released
		select {
		case err := <-second:
			t.Fatalf("the second Refresh returned %v while the first fetch was still in flight", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if got := kids(f.cache.current()); len(got) != 1 || got[0] != "new" {
			t.Fatalf("current holds %v, want [new]: the slower fetch put the old set back over the newer one", got)
		}
		if f.fetch.count() != 2 {
			t.Fatalf("fetches = %d, want 2", f.fetch.count())
		}
	})

	t.Run("timer-free", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		if f.cache.current() != nil {
			t.Fatal("a cache holds keys before any Refresh")
		}
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		first := f.cache.current().fetched
		f.advance(time.Minute)
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := f.cache.current().fetched; !got.Equal(first.Add(time.Minute)) {
			t.Fatalf("fetched = %v, want %v", got, first.Add(time.Minute))
		}
		if f.fetch.count() != 2 {
			t.Fatalf("fetches = %d, want 2: only Refresh fetches", f.fetch.count())
		}
	})

	t.Run("on-demand once", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		if !f.cache.refreshOnDemand(context.Background()) {
			t.Fatal("the first on-demand refresh did not run")
		}
		f.advance(30 * time.Second)
		if f.cache.refreshOnDemand(context.Background()) {
			t.Fatal("a second on-demand refresh ran inside the cooldown")
		}
		if f.fetch.count() != 1 {
			t.Fatalf("fetches = %d, want 1", f.fetch.count())
		}
	})

	t.Run("on-demand after cooldown", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		f.cache.refreshOnDemand(context.Background())
		f.advance(time.Minute + time.Second)
		if !f.cache.refreshOnDemand(context.Background()) {
			t.Fatal("an on-demand refresh past the cooldown did not run")
		}
		if f.fetch.count() != 2 {
			t.Fatalf("fetches = %d, want 2", f.fetch.count())
		}
	})

	t.Run("concurrent on-demand", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() { f.cache.refreshOnDemand(context.Background()) })
		}
		wg.Wait()
		if n := f.fetch.count(); n > 1 {
			t.Fatalf("fetches = %d, want at most 1", n)
		}
	})

	t.Run("failed refresh keeps keys", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		before := f.cache.current()
		f.fetch.program(jose.JSONWebKeySet{}, errors.New("issuer down"))
		if err := f.cache.Refresh(context.Background()); err == nil {
			t.Fatal("Refresh hid the fetch error")
		}
		if f.cache.current() != before {
			t.Fatal("a failed fetch replaced the set")
		}
		if got := f.rec.refresh; len(got) != 2 || got[1] != "failed" {
			t.Fatalf("JWKSRefresh calls = %v, want [ok failed]", got)
		}
		if !strings.Contains(f.logs.String(), "level=WARN") {
			t.Fatalf("no warning logged:\n%s", f.logs.String())
		}
	})

	t.Run("stale", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		if !f.cache.stale() {
			t.Fatal("a cache that never fetched is not stale")
		}
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.cache.stale() {
			t.Fatal("a set fetched just now is stale")
		}
		f.advance(24*time.Hour + time.Second)
		if !f.cache.stale() {
			t.Fatal("a set past jwksMaxStale is not stale")
		}
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.cache.stale() {
			t.Fatal("a successful Refresh left the set stale")
		}
	})

	t.Run("fetched recorded", func(t *testing.T) {
		f := newJWKSFixture(t, goodSet(t))
		f.advance(90 * time.Minute)
		if err := f.cache.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.rec.fetched) != 1 || !f.rec.fetched[0].Equal(f.clock()) {
			t.Fatalf("JWKSFetched calls = %v, want [%v]", f.rec.fetched, f.clock())
		}
	})
}

func TestCompatible(t *testing.T) {
	keys := testKeys(t)
	rsaKey := jose.JSONWebKey{Key: &keys.rsa2048.PublicKey}
	p256 := jose.JSONWebKey{Key: &keys.p256.PublicKey}
	p384 := jose.JSONWebKey{Key: &keys.p384.PublicKey}
	rows := []struct {
		name string
		alg  string
		key  jose.JSONWebKey
		want bool
	}{
		{"RS256 rsa", "RS256", rsaKey, true},
		{"PS512 rsa", "PS512", rsaKey, true},
		{"RS256 ec", "RS256", p256, false},
		{"ES256 p256", "ES256", p256, true},
		{"ES256 p384", "ES256", p384, false},
		{"ES384 p384", "ES384", p384, true},
		{"ES512 p384", "ES512", p384, false},
		{"ES256 rsa", "ES256", rsaKey, false},
		{"HS256 rsa", "HS256", rsaKey, false},
		{"none rsa", "none", rsaKey, false},
		{"key alg matches", "RS256", jose.JSONWebKey{Key: &keys.rsa2048.PublicKey, Algorithm: "RS256"}, true},
		{"key alg differs", "RS384", jose.JSONWebKey{Key: &keys.rsa2048.PublicKey, Algorithm: "RS256"}, false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			if got := compatible(tc.alg, tc.key); got != tc.want {
				t.Fatalf("compatible(%q, %T) = %v, want %v", tc.alg, tc.key.Key, got, tc.want)
			}
		})
	}
}
