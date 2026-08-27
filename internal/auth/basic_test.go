package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/arloliu/profgate/internal/config"
)

// Precomputed hashes, so nothing is minted per run and every fixture sits
// inside the 10–14 cost range ValidateBasicUsers accepts.
//
//nolint:gosec // test fixtures of known passwords, not credentials
const (
	// hashSecret10 is bcrypt("secret") at cost 10.
	hashSecret10 = "$2a$10$iEzNhYcKH9sx5MXRmPwYZ.dnErkytXkzBO/iq.noAigX5vrin6GKC"
	// hashHunter10 is bcrypt("hunter2") at cost 10.
	hashHunter10 = "$2a$10$IFsv/3pWMaxq1Uc.1tJGsOt4IpAL79xvq7R1SGU3uYjY6IUdfq/wq"
	// hashSecret12 is bcrypt("secret") at cost 12.
	hashSecret12 = "$2a$12$.Hw.f2CXg1nJmFzqOT2hguLZ7eeewKR0Czrn0vOEqlfzunzu1s1PG"
)

// countingComparer runs bcrypt and counts how often, so a row can prove that
// a rejected request never reached the comparison.
// When block is set, every comparison signals started and then waits on
// block, so a test knows the slot is held before it sends the next request.
type countingComparer struct {
	calls   atomic.Int32
	block   chan struct{}
	started chan struct{}
}

func (c *countingComparer) compare(hash, password []byte) error {
	c.calls.Add(1)
	if c.block != nil {
		c.started <- struct{}{}
		<-c.block
	}

	return bcrypt.CompareHashAndPassword(hash, password)
}

// basicConfig is a validated-shaped basic-mode configuration with alice
// inline at cost 10 and the developer realm.
func basicConfig(maxConcurrent int) *config.Config {
	return &config.Config{
		Auth: config.AuthConfig{
			Mode: "basic",
			Basic: &config.BasicConfig{
				Users:         []config.BasicUser{{Name: "alice", PasswordHash: hashSecret10, Realm: "developer"}},
				MaxConcurrent: maxConcurrent,
			},
		},
		Realms: map[string]config.Realm{"developer": {}, "ops": {}},
	}
}

// newTestBasic builds Basic over cfg with a counting comparer.
func newTestBasic(t *testing.T, cfg *config.Config) (*Basic, *countingComparer) {
	t.Helper()
	b, err := NewBasic(cfg, BasicOptions{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("NewBasic error = %v", err)
	}
	cmp := &countingComparer{}
	b.compare = cmp

	return b, cmp
}

func basicHeader(name, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(name+":"+password))
}

func request(header string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), "GET", "/v1/x", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}

	return r
}

// wantFailure asserts err is a Failure with the status and reason.
func wantFailure(t *testing.T, err error, status int, reason string) {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("error = %v, want a *Failure with %d %s", err, status, reason)
	}
	if f.Status != status || f.Reason != reason {
		t.Fatalf("Failure = %d %s, want %d %s", f.Status, f.Reason, status, reason)
	}
}

func wantPrincipal(t *testing.T, p Principal, err error, name, realm string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Authenticate error = %v, want principal %s in %s", err, name, realm)
	}
	if p.Name != name || p.Realm != realm {
		t.Fatalf("Principal = %+v, want %s in %s", p, name, realm)
	}
}

func TestBasicAuthenticate(t *testing.T) {
	cases := []struct {
		name        string
		header      string
		status      int
		reason      string
		principal   string
		comparisons int32
	}{
		{name: "correct password", header: basicHeader("alice", "secret"), principal: "alice", comparisons: 1},
		{name: "wrong password", header: basicHeader("alice", "nope"), status: 401, reason: ReasonBadCredential, comparisons: 1},
		{name: "unknown user", header: basicHeader("mallory", "x"), status: 401, reason: ReasonBadCredential, comparisons: 1},
		{name: "no header", status: 401, reason: ReasonMissing},
		{name: "wrong scheme", header: "Bearer abc", status: 401, reason: ReasonScheme},
		{name: "scheme case", header: "basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret")), principal: "alice", comparisons: 1},
		{name: "malformed base64", header: "Basic !!!", status: 401, reason: ReasonMalformed},
		{name: "no colon", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), status: 401, reason: ReasonMalformed},
		{name: "oversize header", header: "Basic " + strings.Repeat("A", 1025-len("Basic ")), status: 401, reason: ReasonMalformed},
		{name: "oversize password", header: basicHeader("alice", strings.Repeat("p", 73)), status: 401, reason: ReasonMalformed},
		{name: "72-byte password", header: basicHeader("alice", strings.Repeat("p", 72)), status: 401, reason: ReasonBadCredential, comparisons: 1},
		{name: "exact name", header: basicHeader("Alice", "secret"), status: 401, reason: ReasonBadCredential, comparisons: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, cmp := newTestBasic(t, basicConfig(16))
			p, err := b.Authenticate(context.Background(), request(tc.header), basicConfig(16))
			if tc.principal != "" {
				wantPrincipal(t, p, err, tc.principal, "developer")
			} else {
				wantFailure(t, err, tc.status, tc.reason)
			}
			if got := cmp.calls.Load(); got != tc.comparisons {
				t.Fatalf("comparisons = %d, want %d", got, tc.comparisons)
			}
		})
	}
}

// TestBasicUnknownUserHash proves the unknown-user comparison runs against a
// hash that belongs to no user, at the shared cost, so it costs what a real
// comparison costs without ever admitting anyone.
func TestBasicUnknownUserHash(t *testing.T) {
	cfg := basicConfig(16)
	b, err := NewBasic(cfg, BasicOptions{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	var seen []byte
	b.compare = comparerFunc(func(hash, password []byte) error {
		seen = hash

		return bcrypt.CompareHashAndPassword(hash, password)
	})
	_, err = b.Authenticate(context.Background(), request(basicHeader("mallory", "secret")), cfg)
	wantFailure(t, err, 401, ReasonBadCredential)
	if string(seen) == hashSecret10 || string(seen) == hashHunter10 {
		t.Fatal("the unknown user was compared against a real user's hash")
	}
	cost, err := bcrypt.Cost(seen)
	if err != nil || cost != 10 {
		t.Fatalf("dummy cost = %d (%v), want 10", cost, err)
	}
}

type comparerFunc func(hash, password []byte) error

func (f comparerFunc) compare(hash, password []byte) error { return f(hash, password) }

func TestBasicDummyCost(t *testing.T) {
	b, _ := newTestBasic(t, basicConfig(16))
	cost, err := bcrypt.Cost(b.set.Load().dummy)
	if err != nil {
		t.Fatalf("the dummy hash does not parse: %v", err)
	}
	if cost != 10 {
		t.Fatalf("dummy cost = %d, want the inline users' 10", cost)
	}
}

func TestBasicGate(t *testing.T) {
	t.Run("gate full", func(t *testing.T) {
		cfg := basicConfig(1)
		b, cmp := newTestBasic(t, cfg)
		cmp.block, cmp.started = make(chan struct{}), make(chan struct{}, 1)
		first := make(chan error, 1)
		go func() {
			_, err := b.Authenticate(context.Background(), request(basicHeader("alice", "secret")), cfg)
			first <- err
		}()
		<-cmp.started
		_, err := b.Authenticate(context.Background(), request(basicHeader("alice", "secret")), cfg)
		wantFailure(t, err, 429, ReasonThrottled)
		if got := cmp.calls.Load(); got != 1 {
			t.Fatalf("comparisons = %d, want 1: the throttled request must not compare", got)
		}
		close(cmp.block)
		if err := <-first; err != nil {
			t.Fatalf("first request error = %v", err)
		}
		p, err := b.Authenticate(context.Background(), request(basicHeader("alice", "secret")), cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})

	t.Run("gate released on failure", func(t *testing.T) {
		cfg := basicConfig(1)
		b, _ := newTestBasic(t, cfg)
		_, err := b.Authenticate(context.Background(), request(basicHeader("alice", "nope")), cfg)
		wantFailure(t, err, 401, ReasonBadCredential)
		p, err := b.Authenticate(context.Background(), request(basicHeader("alice", "secret")), cfg)
		wantPrincipal(t, p, err, "alice", "developer")
	})
}
