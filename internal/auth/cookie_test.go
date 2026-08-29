package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKey returns a 32-byte key whose every byte is b.
func testKey(b byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = b
	}

	return k
}

// keyLine is one line of the key file for k.
func keyLine(k [32]byte) string {
	return base64.StdEncoding.EncodeToString(k[:])
}

// keyFile is the key file with keys in the order given.
func keyFile(keys ...[32]byte) []byte {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(keyLine(k))
		b.WriteString("\n")
	}

	return []byte(b.String())
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// newTestSealer seals under current with previous (or nil) also opening,
// reading nonces from the real random source.
func newTestSealer(current [32]byte, previous *[32]byte) *sealer {
	s := newSealer()
	s.keys.Store(&cookieKeys{current: current, previous: previous})

	return s
}

func TestParseCookieKeys(t *testing.T) {
	a, b := testKey(1), testKey(2)

	t.Run("parse one key", func(t *testing.T) {
		got, err := parseCookieKeys([]byte(keyLine(a)))
		if err != nil {
			t.Fatal(err)
		}
		if got.current != a || got.previous != nil {
			t.Fatalf("got current %x previous %v, want a and nil", got.current[:2], got.previous)
		}
	})

	t.Run("parse two keys", func(t *testing.T) {
		got, err := parseCookieKeys(keyFile(a, b))
		if err != nil {
			t.Fatal(err)
		}
		if got.current != a || got.previous == nil || *got.previous != b {
			t.Fatal("keys are not [a, b] in order")
		}
	})

	t.Run("parse errors", func(t *testing.T) {
		short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 31))
		for name, raw := range map[string][]byte{
			"zero lines":   []byte(""),
			"only newline": []byte("\n"),
			"three lines":  keyFile(a, b, a),
			"31-byte key":  []byte(short + "\n"),
			"not base64":   []byte("!!! not base64 !!!\n"),
		} {
			if _, err := parseCookieKeys(raw); err == nil {
				t.Errorf("%s: parsed without error", name)
			}
		}
	})

	t.Run("fingerprint", func(t *testing.T) {
		sum := sha256.Sum256(a[:])
		want := hex.EncodeToString(sum[:])[:8]
		if got := fingerprint(a); got != want {
			t.Fatalf("fingerprint = %q, want %q", got, want)
		}
	})
}

func TestSealer(t *testing.T) {
	a, b := testKey(1), testKey(2)
	plain := []byte("plaintext bytes")

	t.Run("seal opens", func(t *testing.T) {
		s := newTestSealer(a, nil)
		v, fail := s.seal(cookieSession, plain)
		if fail != nil {
			t.Fatal(fail)
		}
		got, ok := s.open(cookieSession, v)
		if !ok || !bytes.Equal(got, plain) {
			t.Fatalf("open = %q, %v", got, ok)
		}
	})

	t.Run("previous opens", func(t *testing.T) {
		s := newTestSealer(a, nil)
		v, _ := s.seal(cookieSession, plain)
		s.keys.Store(&cookieKeys{current: b, previous: &a})
		if got, ok := s.open(cookieSession, v); !ok || !bytes.Equal(got, plain) {
			t.Fatal("a cookie sealed under the previous key must still open")
		}
	})

	t.Run("removed key", func(t *testing.T) {
		s := newTestSealer(a, nil)
		v, _ := s.seal(cookieSession, plain)
		s.keys.Store(&cookieKeys{current: b})
		if _, ok := s.open(cookieSession, v); ok {
			t.Fatal("a cookie sealed under a removed key must not open")
		}
	})

	t.Run("name is AAD", func(t *testing.T) {
		s := newTestSealer(a, nil)
		v, _ := s.seal(cookieTxn, plain)
		if _, ok := s.open(cookieSession, v); ok {
			t.Fatal("a transaction cookie must not open as a session cookie")
		}
	})

	t.Run("tamper", func(t *testing.T) {
		s := newTestSealer(a, nil)
		v, _ := s.seal(cookieSession, plain)
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		for i := range raw {
			flipped := bytes.Clone(raw)
			flipped[i] ^= 0x01
			if _, ok := s.open(cookieSession, base64.RawURLEncoding.EncodeToString(flipped)); ok {
				t.Fatalf("byte %d flipped and the value still opened", i)
			}
		}
	})

	t.Run("bad base64", func(t *testing.T) {
		s := newTestSealer(a, nil)
		if _, ok := s.open(cookieSession, "!!!"); ok {
			t.Fatal("opened")
		}
	})

	t.Run("short value", func(t *testing.T) {
		s := newTestSealer(a, nil)
		short := base64.RawURLEncoding.EncodeToString(make([]byte, 12+16-1))
		if _, ok := s.open(cookieSession, short); ok {
			t.Fatal("opened a value shorter than nonce plus tag")
		}
	})

	t.Run("entropy", func(t *testing.T) {
		s := newTestSealer(a, nil)
		s.rand = errReader{}
		_, fail := s.seal(cookieSession, plain)
		if fail == nil || fail.Status != http.StatusServiceUnavailable || fail.Reason != ReasonEntropy {
			t.Fatalf("seal failure = %v, want 503 entropy", fail)
		}
	})

	t.Run("fixed nonce", func(t *testing.T) {
		s := newTestSealer(a, nil)
		nonce := []byte("0123456789ab")
		s.rand = bytes.NewReader(nonce)
		v, fail := s.seal(cookieSession, plain)
		if fail != nil {
			t.Fatal(fail)
		}
		want := base64.RawURLEncoding.EncodeToString(nonce)
		if !strings.HasPrefix(v, want) {
			t.Fatalf("value %q does not start with the nonce's 16 characters %q", v, want)
		}
	})
}

func TestSessionEncoding(t *testing.T) {
	exp := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	t.Run("session round trip", func(t *testing.T) {
		in := session{Principal: "alice", Realm: "developer", Exp: exp.Add(500 * time.Millisecond)}
		got, ok := decodeSession(in.encode())
		if !ok {
			t.Fatal("did not decode")
		}
		if got.Principal != in.Principal || got.Realm != in.Realm || !got.Exp.Equal(exp) {
			t.Fatalf("decoded %+v, want %+v at second precision", got, in)
		}
	})

	t.Run("trailing bytes", func(t *testing.T) {
		b := session{Principal: "alice", Realm: "developer", Exp: exp}.encode()
		if _, ok := decodeSession(append(b, 0)); ok {
			t.Fatal("decoded a plaintext with a byte left over")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		b := session{Principal: "alice", Realm: "developer", Exp: exp}.encode()
		if _, ok := decodeSession(b[:len(b)-1]); ok {
			t.Fatal("decoded a truncated plaintext")
		}
	})

	t.Run("transaction round trip", func(t *testing.T) {
		in := transaction{State: "s", Nonce: "n", Verifier: "v", Return: "/v1/x?seconds=5", Exp: exp}
		got, ok := decodeTransaction(in.encode())
		if !ok {
			t.Fatal("did not decode")
		}
		if got.State != in.State || got.Nonce != in.Nonce || got.Verifier != in.Verifier || got.Return != in.Return || !got.Exp.Equal(in.Exp) {
			t.Fatalf("decoded %+v, want %+v", got, in)
		}
	})

	t.Run("transaction trailing bytes", func(t *testing.T) {
		b := transaction{State: "s", Nonce: "n", Verifier: "v", Return: "/", Exp: exp}.encode()
		if _, ok := decodeTransaction(append(b, 0)); ok {
			t.Fatal("decoded a plaintext with a byte left over")
		}
	})
}

// parsedCookie returns the single Set-Cookie header w wrote.
func parsedCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	lines := w.Result().Header.Values("Set-Cookie")
	if len(lines) != 1 {
		t.Fatalf("wrote %d Set-Cookie headers, want 1: %q", len(lines), lines)
	}
	c, err := http.ParseSetCookie(lines[0])
	if err != nil {
		t.Fatalf("%q: %v", lines[0], err)
	}

	return c
}

// assertCookieAttributes asserts the four attributes every cookie carries and
// the absence of Domain.
func assertCookieAttributes(t *testing.T, c *http.Cookie) {
	t.Helper()
	if c.Path != "/" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("attributes %+v, want Path=/; HttpOnly; Secure; SameSite=Lax", c)
	}
	if c.Domain != "" {
		t.Fatalf("Domain %q set on a __Host- cookie", c.Domain)
	}
}

func TestSetCookie(t *testing.T) {
	t.Run("set-cookie attributes", func(t *testing.T) {
		w := httptest.NewRecorder()
		if err := setCookie(w, cookieSession, "abc", 8*time.Hour); err != nil {
			t.Fatal(err)
		}
		want := "__Host-profgate_session=abc; Path=/; Max-Age=28800; HttpOnly; Secure; SameSite=Lax"
		if got := w.Result().Header.Get("Set-Cookie"); got != want {
			t.Fatalf("Set-Cookie = %q, want %q", got, want)
		}
		c := parsedCookie(t, w)
		assertCookieAttributes(t, c)
		if c.Name != cookieSession || c.Value != "abc" || c.MaxAge != 28800 {
			t.Fatalf("cookie %+v", c)
		}
	})

	t.Run("delete-cookie attributes", func(t *testing.T) {
		w := httptest.NewRecorder()
		deleteCookie(w, cookieTxn)
		c := parsedCookie(t, w)
		assertCookieAttributes(t, c)
		if c.Name != cookieTxn || c.Value != "" || c.MaxAge != -1 {
			t.Fatalf("cookie %+v, want an empty value and Max-Age=0", c)
		}
	})

	t.Run("delete session cookie", func(t *testing.T) {
		w := httptest.NewRecorder()
		DeleteSessionCookie(w)
		c := parsedCookie(t, w)
		assertCookieAttributes(t, c)
		if c.Name != cookieSession || c.Value != "" || c.MaxAge != -1 {
			t.Fatalf("cookie %+v, want an empty value and Max-Age=0", c)
		}
	})

	t.Run("value cap", func(t *testing.T) {
		w := httptest.NewRecorder()
		err := setCookie(w, cookieSession, strings.Repeat("a", cookieMaxLen+1), time.Hour)
		if err == nil {
			t.Fatal("a value over the cap was accepted")
		}
		if got := w.Result().Header.Values("Set-Cookie"); len(got) != 0 {
			t.Fatalf("wrote %q after refusing the value", got)
		}
	})
}

// replica is one gateway replica: a sealer fed by its own key-file poller.
type replica struct {
	s *sealer
	p *filePoller
}

func newReplica(t *testing.T, reads ...[]byte) *replica {
	t.Helper()
	r := &replica{s: newSealer()}
	r.p = newFilePoller("/etc/profgate/auth/cookie.key", r.s.applyKeyFile, "cookie_key", 0, &reloadRecorder{}, slog.New(slog.DiscardHandler))
	r.p.read = sequenceReader(t, reads...)

	return r
}

func TestKeyRotation(t *testing.T) {
	oldKey, newKey := testKey(1), testKey(2)

	t.Run("staged rotation", func(t *testing.T) {
		// The key file at each step of the procedure: [old], [old new], [new old], [new].
		// The interleaving below models the wait between steps:
		// one replica re-reads the file before the other,
		// and a cookie sealed by either must open on both until both have moved.
		steps := [][]byte{keyFile(oldKey), keyFile(oldKey, newKey), keyFile(newKey, oldKey), keyFile(newKey)}
		a := newReplica(t, steps...)
		b := newReplica(t, steps...)
		check := func(step int) {
			t.Helper()
			for _, sealedBy := range []*replica{a, b} {
				v, fail := sealedBy.s.seal(cookieSession, []byte("session"))
				if fail != nil {
					t.Fatal(fail)
				}
				for _, opener := range []*replica{a, b} {
					if _, ok := opener.s.open(cookieSession, v); !ok {
						t.Fatalf("step %d: a cookie sealed by one replica did not open on the other", step)
					}
				}
			}
		}
		a.p.Poll()
		b.p.Poll()
		check(1)
		for i := 1; i < len(steps); i++ {
			a.p.Poll()
			check(i + 1)
			b.p.Poll()
			check(i + 1)
		}
	})

	t.Run("one-step swap loses a session", func(t *testing.T) {
		// Writing [new] over [old] in one step:
		// replica A has re-read the file and seals with new;
		// replica B has not and holds only old.
		// A browser that logs in on A and is routed to B is sent back to login.
		// The staged procedure exists so that no replica ever seals with a key another replica cannot open.
		a := newReplica(t, keyFile(oldKey), keyFile(newKey))
		b := newReplica(t, keyFile(oldKey))
		a.p.Poll()
		b.p.Poll()
		a.p.Poll()
		v, fail := a.s.seal(cookieSession, []byte("session"))
		if fail != nil {
			t.Fatal(fail)
		}
		if _, ok := b.s.open(cookieSession, v); ok {
			t.Fatal("the replica still on the old key opened a cookie sealed with the new one")
		}
	})

	t.Run("bad file keeps the previous keys", func(t *testing.T) {
		a := newReplica(t, keyFile(oldKey), []byte("not a key\n"), keyFile(oldKey, newKey, oldKey))
		a.p.Poll()
		v, _ := a.s.seal(cookieSession, []byte("session"))
		a.p.Poll()
		a.p.Poll()
		if _, ok := a.s.open(cookieSession, v); !ok {
			t.Fatal("a rejected key file replaced the loaded keys")
		}
	})
}
