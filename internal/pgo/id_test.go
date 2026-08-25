package pgo

import (
	"strings"
	"testing"
)

func TestNewIDShape(t *testing.T) {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

	for range 1000 {
		id := newID()
		if len(id) != idLength {
			t.Fatalf("id %q is %d characters, want %d", id, len(id), idLength)
		}
		for _, r := range id {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("id %q carries %q, outside the Crockford base32 alphabet", id, r)
			}
		}
		if !ValidID(id) {
			t.Fatalf("newID produced %q, which ValidID rejects", id)
		}
	}
}

// TestNewIDDistinct draws 10k identifiers: 100 bits of crypto/rand collide
// with negligible probability, so a repeat means the draw is not random.
func TestNewIDDistinct(t *testing.T) {
	const draws = 10_000

	seen := make(map[string]struct{}, draws)
	for range draws {
		id := newID()
		if _, dup := seen[id]; dup {
			t.Fatalf("id %q drawn twice in %d draws", id, draws)
		}
		seen[id] = struct{}{}
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "the spec's identifier", id: "7h2k9m4p6r8t0v1w3x5y", want: true},
		{name: "all zeroes", id: strings.Repeat("0", 20), want: true},
		{name: "empty", id: "", want: false},
		{name: "one short", id: strings.Repeat("7", 19), want: false},
		{name: "one long", id: strings.Repeat("7", 21), want: false},
		{name: "uppercase", id: "7H2K9M4P6R8T0V1W3X5Y", want: false},
		{name: "letter i", id: "7h2k9m4p6r8t0v1w3x5i", want: false},
		{name: "letter l", id: "7h2k9m4p6r8t0v1w3x5l", want: false},
		{name: "letter o", id: "7h2k9m4p6r8t0v1w3x5o", want: false},
		{name: "letter u", id: "7h2k9m4p6r8t0v1w3x5u", want: false},
		{name: "a path traversal", id: "../../etc/passwd0000", want: false},
		{name: "a key separator", id: "7h2k9m4p6r8t0v1w3x5.", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidID(tc.id); got != tc.want {
				t.Fatalf("ValidID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
