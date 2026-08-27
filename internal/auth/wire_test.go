package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestRandomValue(t *testing.T) {
	t.Run("fixed vectors", func(t *testing.T) {
		seed := make([]byte, 32)
		for i := range seed {
			seed[i] = byte(i)
		}
		state, fail := randomValue(bytes.NewReader(seed))
		if fail != nil {
			t.Fatal(fail)
		}
		if want := base64.RawURLEncoding.EncodeToString(seed); state != want || len(state) != 43 {
			t.Fatalf("state = %q (%d chars), want %q", state, len(state), want)
		}
		sum := sha256.Sum256([]byte(state))
		if got, want := challenge(state), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
			t.Fatalf("challenge = %q, want %q", got, want)
		}
	})

	t.Run("alphabet", func(t *testing.T) {
		alphabet := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
		s := newSealer()
		for range 100 {
			v, fail := randomValue(s.rand)
			if fail != nil {
				t.Fatal(fail)
			}
			if !alphabet.MatchString(v) {
				t.Fatalf("%q is not 43 characters of unpadded base64url", v)
			}
		}
	})

	t.Run("entropy", func(t *testing.T) {
		_, fail := randomValue(errReader{})
		if fail == nil || fail.Status != http.StatusServiceUnavailable || fail.Reason != ReasonEntropy {
			t.Fatalf("failure = %v, want 503 entropy", fail)
		}
	})
}

func TestCanonicalReturn(t *testing.T) {
	refused := map[string]string{
		"backslash host":      `/\evil.example`,
		"protocol relative":   "//evil.example",
		"absolute":            "https://evil.example/",
		"encoded slashes":     "%2F%2Fevil",
		"encoded backslash":   "/a%5Cb",
		"newline":             "/a\nb",
		"2 KiB path":          "/" + strings.Repeat("a", 2048),
		"javascript scheme":   "javascript:alert(1)",
		"empty":               "",
		"query newline":       "/v1/x?q=%0A",
		"query backslash":     "/v1/x?q=%5C",
		"query raw backslash": `/v1/x?q=a\b`,
		"query control byte":  "/v1/x?q=a\x01",
		"query bad escape":    "/v1/x?q=%ZZ",
		"parent segment":      "/v1/../ops",
		"dot segment":         "/v1/./x",
	}
	for name, raw := range refused {
		t.Run("refused "+name, func(t *testing.T) {
			if got := canonicalReturn(raw); got != "/" {
				t.Fatalf("canonicalReturn(%q) = %q, want /", raw, got)
			}
		})
	}

	kept := map[string][2]string{
		"return kept":     {"/v1/x?seconds=5#frag", "/v1/x?seconds=5"},
		"query untouched": {"/v1/x?name=a%2Fb", "/v1/x?name=a%2Fb"},
		"path re-escaped": {"/v1/a%20b", "/v1/a%20b"},
		"root":            {"/", "/"},
		"trailing slash":  {"/v1/", "/v1/"},
	}
	for name, tc := range kept {
		t.Run(name, func(t *testing.T) {
			if got := canonicalReturn(tc[0]); got != tc[1] {
				t.Fatalf("canonicalReturn(%q) = %q, want %q", tc[0], got, tc[1])
			}
		})
	}
}
