package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/arloliu/profgate/internal/metrics"
)

const (
	// cookieSession carries the minted session; cookieTxn carries the login in flight.
	// The __Host- prefix makes the browser refuse either without Secure, Path=/, and no Domain.
	cookieSession = "__Host-profgate_session"
	cookieTxn     = "__Host-profgate_txn"
	// cookieMaxLen is the largest value the gateway will set, below the
	// 4096-byte cookie limit browsers share.
	cookieMaxLen = 4000
	// cookieKeySize is AES-256's key size; nonceSize is GCM's standard nonce.
	cookieKeySize = 32
	nonceSize     = 12
)

// cookieKeys is one snapshot of the key file: current seals, both open.
type cookieKeys struct {
	current  [cookieKeySize]byte
	previous *[cookieKeySize]byte
}

// parseCookieKeys reads one or two base64 lines of 32 bytes each, first
// line current, and tolerates one trailing newline.
// Lines are standard base64 because that is what `openssl rand -base64 32`
// writes.
func parseCookieKeys(b []byte) (cookieKeys, error) {
	b = bytes.TrimSuffix(b, []byte("\n"))
	if len(b) == 0 {
		return cookieKeys{}, errors.New("cookie key file is empty")
	}
	lines := bytes.Split(b, []byte("\n"))
	if len(lines) > 2 {
		return cookieKeys{}, fmt.Errorf("cookie key file has %d lines, want one or two", len(lines))
	}
	var keys cookieKeys
	for i, line := range lines {
		raw, err := base64.StdEncoding.DecodeString(string(line))
		if err != nil {
			return cookieKeys{}, fmt.Errorf("cookie key file line %d: not base64", i+1)
		}
		if len(raw) != cookieKeySize {
			return cookieKeys{}, fmt.Errorf("cookie key file line %d: %d bytes, want %d", i+1, len(raw), cookieKeySize)
		}
		if i == 0 {
			copy(keys.current[:], raw)
		} else {
			keys.previous = new([cookieKeySize]byte)
			copy(keys.previous[:], raw)
		}
	}

	return keys, nil
}

// fingerprint is the first 8 hex digits of SHA-256(key), the value the
// key-info gauge reports so an operator can watch a rotation without reading
// key material.
func fingerprint(key [cookieKeySize]byte) string {
	sum := sha256.Sum256(key[:])

	return hex.EncodeToString(sum[:])[:8]
}

// sealer seals and opens cookie values under the current key file snapshot.
// keys is nil until the first key file is applied.
type sealer struct {
	keys atomic.Pointer[cookieKeys]
	rand io.Reader
}

// newSealer builds a sealer with no keys, reading nonces from crypto/rand.
func newSealer() *sealer {
	return &sealer{rand: rand.Reader}
}

// applyKeyFile is the key-file poller's apply: it parses the bytes and swaps
// the snapshot whole, so a request that loaded one set opens under it.
func (s *sealer) applyKeyFile(raw []byte) error {
	keys, err := parseCookieKeys(raw)
	if err != nil {
		return err
	}
	s.keys.Store(&keys)

	return nil
}

// keyInfo lists the loaded keys as the info gauge reports them.
func (s *sealer) keyInfo() []metrics.CookieKey {
	keys := s.keys.Load()
	if keys == nil {
		return nil
	}
	info := []metrics.CookieKey{{Fingerprint: fingerprint(keys.current), Role: "current"}}
	if keys.previous != nil {
		info = append(info, metrics.CookieKey{Fingerprint: fingerprint(*keys.previous), Role: "previous"})
	}

	return info
}

// seal returns base64url(nonce || AES-256-GCM(plaintext)) under the current
// key with name as associated data, or a Failure{503, entropy} when the
// random source fails.
func (s *sealer) seal(name string, plaintext []byte) (string, *Failure) {
	keys := s.keys.Load()
	if keys == nil {
		return "", &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonEntropy}
	}
	gcm, err := newGCM(keys.current)
	if err != nil {
		return "", &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonEntropy}
	}
	buf := make([]byte, nonceSize, nonceSize+len(plaintext)+gcm.Overhead())
	if _, err := io.ReadFull(s.rand, buf); err != nil {
		return "", &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonEntropy}
	}
	buf = gcm.Seal(buf, buf[:nonceSize], plaintext, []byte(name))

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// open tries the current key then the previous one under the same associated
// data; false when neither opens, when the value is not base64url, or when
// it is shorter than a nonce plus a tag.
func (s *sealer) open(name, value string) ([]byte, bool) {
	keys := s.keys.Load()
	if keys == nil {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < nonceSize+16 {
		return nil, false
	}
	nonce, sealed := raw[:nonceSize], raw[nonceSize:]
	candidates := []*[cookieKeySize]byte{&keys.current}
	if keys.previous != nil {
		candidates = append(candidates, keys.previous)
	}
	for _, key := range candidates {
		gcm, err := newGCM(*key)
		if err != nil {
			continue
		}
		if plain, err := gcm.Open(nil, nonce, sealed, []byte(name)); err == nil {
			return plain, true
		}
	}

	return nil, false
}

// newGCM builds AES-256-GCM over key.
func newGCM(key [cookieKeySize]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}

	return cipher.NewGCM(block)
}

// session is the plaintext of the session cookie.
type session struct {
	Principal, Realm string
	Exp              time.Time
}

// transaction is the plaintext of the login in flight.
type transaction struct {
	State, Nonce, Verifier, Return string
	Exp                            time.Time
}

// sessionWire is the sealed form of a session:
// a JSON object whose exp is Unix seconds,
// so an expiry costs a number rather than a timestamp string
// and round-trips at the precision the session is judged on.
type sessionWire struct {
	Principal string `json:"principal"`
	Realm     string `json:"realm"`
	Exp       int64  `json:"exp"`
}

// transactionWire is the sealed form of a login in flight,
// with the same expiry encoding as sessionWire.
type transactionWire struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
	Exp      int64  `json:"exp"`
}

// encode marshals the session as its wire object.
// The fields are bounded well under the cookie limit —
// the verifier bounds the principal at 256 bytes and the realm is a DNS label —
// so the result always fits a cookie.
func (s session) encode() []byte {
	b, err := json.Marshal(sessionWire{Principal: s.Principal, Realm: s.Realm, Exp: s.Exp.Unix()})
	if err != nil {
		// A struct of strings and an int64 has no unmarshalable value.
		panic("auth: marshaling a session: " + err.Error())
	}

	return b
}

// decodeSession is the inverse of encode;
// false on any plaintext that is not one well-formed session object,
// including bytes left over after it.
func decodeSession(b []byte) (session, bool) {
	var w sessionWire
	if err := json.Unmarshal(b, &w); err != nil {
		return session{}, false
	}

	return session{Principal: w.Principal, Realm: w.Realm, Exp: time.Unix(w.Exp, 0)}, true
}

// encode marshals the transaction as session.encode does.
// The three wire values are 43 bytes each and the return path is at most 1024,
// so the result always fits a cookie.
func (t transaction) encode() []byte {
	b, err := json.Marshal(transactionWire{
		State: t.State, Nonce: t.Nonce, Verifier: t.Verifier, Return: t.Return, Exp: t.Exp.Unix(),
	})
	if err != nil {
		// A struct of strings and an int64 has no unmarshalable value.
		panic("auth: marshaling a transaction: " + err.Error())
	}

	return b
}

// decodeTransaction is the inverse of encode;
// false on any plaintext that is not one well-formed transaction object.
func decodeTransaction(b []byte) (transaction, bool) {
	var w transactionWire
	if err := json.Unmarshal(b, &w); err != nil {
		return transaction{}, false
	}

	return transaction{State: w.State, Nonce: w.Nonce, Verifier: w.Verifier, Return: w.Return, Exp: time.Unix(w.Exp, 0)}, true
}

// setCookie writes name=value with the attributes every gateway cookie
// carries — Path=/, Max-Age, HttpOnly, Secure, SameSite=Lax, no Domain — and
// refuses a value over cookieMaxLen without writing anything.
func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) error {
	if len(value) > cookieMaxLen {
		return fmt.Errorf("auth: cookie %s value is %d bytes, over the %d-byte limit", name, len(value), cookieMaxLen)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	return nil
}

// deleteCookie writes the same attributes with an empty value and Max-Age=0,
// which is the only shape a browser matches to a __Host- cookie it holds.
func deleteCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1, // net/http writes Max-Age=0 for a negative value and omits it for zero
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// DeleteSessionCookie deletes the session cookie; internal/httpapi calls it
// when a Failure asks for ClearSession.
func DeleteSessionCookie(w http.ResponseWriter) {
	deleteCookie(w, cookieSession)
}
