package client

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// idToken builds a three-segment token whose payload is the given JSON.
// Nothing signs it: the client reads exp and verifies nothing.
func idToken(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + "." + enc([]byte("sig"))
}

func TestExpiryOf(t *testing.T) {
	obtained := time.Date(2026, 8, 28, 9, 30, 12, 0, time.UTC)
	exp := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)

	t.Run("access is obtainedAt plus expires_in", func(t *testing.T) {
		got, err := ExpiryOf("access", "opaque", 300, obtained)
		if err != nil {
			t.Fatal(err)
		}
		if want := obtained.Add(300 * time.Second); !got.Equal(want) {
			t.Fatalf("expiresAt = %v, want %v", got, want)
		}
	})

	t.Run("id is the payload's exp and expires_in is ignored", func(t *testing.T) {
		token := idToken(`{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`)
		got, err := ExpiryOf("id", token, 5, obtained)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(exp) {
			t.Fatalf("expiresAt = %v, want the payload's exp %v", got, exp)
		}
	})

	t.Run("another token type is an error", func(t *testing.T) {
		if _, err := ExpiryOf("refresh", "x", 5, obtained); err == nil {
			t.Fatal("ExpiryOf(refresh) = nil error")
		}
	})

	bad := []struct {
		name  string
		token string
	}{
		{"payload that is not base64url", "a.!!!.c"},
		{"payload that is not JSON", idToken(`not json`)},
		{"token above 16 KiB", idToken(`{"exp":1,"pad":"` + strings.Repeat("a", 16<<10) + `"}`)},
		{"exp that is a string", idToken(`{"exp":"1790000000"}`)},
		{"exp that is absent", idToken(`{"sub":"alice"}`)},
		{"fewer than three segments", idToken(`{"exp":1}`)[:strings.LastIndex(idToken(`{"exp":1}`), ".")]},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExpiryOf("id", tc.token, 300, obtained); err == nil {
				t.Fatal("ExpiryOf = nil error; the token would be cached")
			}
		})
	}
}

func TestRefreshExpiryOf(t *testing.T) {
	obtained := time.Date(2026, 8, 28, 9, 30, 12, 0, time.UTC)
	if got, want := RefreshExpiryOf(36000, obtained), obtained.Add(36000*time.Second); !got.Equal(want) {
		t.Fatalf("refreshExpiresAt = %v, want %v", got, want)
	}
	if got := RefreshExpiryOf(0, obtained); !got.IsZero() {
		t.Fatalf("refreshExpiresAt without refresh_expires_in = %v, want the zero time", got)
	}
}
