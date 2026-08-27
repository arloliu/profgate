package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	// wireBytes is the size of state, nonce, and the PKCE verifier before
	// encoding; 32 bytes encode to 43 characters, inside RFC 7636's 43–128.
	wireBytes = 32
	// maxReturnLen bounds the return path a login carries.
	maxReturnLen = 1024
)

// randomValue reads 32 bytes from r and returns them as 43 characters of
// unpadded base64url, or a Failure{503, entropy} when r fails.
func randomValue(r io.Reader) (string, *Failure) {
	var b [wireBytes]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonEntropy}
	}

	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// challenge is the S256 code challenge for verifier:
// base64url(SHA-256(ASCII(verifier))).
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// canonicalReturn reduces a client-supplied return path to a path and query
// on this host, or "/" when it is anything else.
// The path and the query are judged after one percent-decoding, so
// %2F%2F, %5C, and %0A are judged as the slashes, backslash, and newline
// they become; both parts are then re-emitted escaped.
// Browsers turn /\evil.example into //evil.example, which is why the rule
// reads the parsed form and not the raw string.
func canonicalReturn(raw string) string {
	if raw == "" || len(raw) > maxReturnLen {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	if !cleanPath(u.Path) {
		return "/"
	}
	q, err := url.QueryUnescape(u.RawQuery)
	if err != nil || !plainBytes(q) {
		return "/"
	}
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}

	return out
}

// cleanPath reports whether a decoded path begins with one slash, carries no
// backslash or control byte, and has no dot segments: path.Clean must leave
// it unchanged apart from a trailing slash.
func cleanPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || !plainBytes(p) {
		return false
	}
	clean := path.Clean(p)

	return clean == p || clean+"/" == p
}

// plainBytes reports whether s has no backslash and no byte below 0x20.
func plainBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] < 0x20 {
			return false
		}
	}

	return true
}
