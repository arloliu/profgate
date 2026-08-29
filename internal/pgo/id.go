package pgo

import "crypto/rand"

const (
	// idAlphabet is Crockford base32 in lowercase: the digits and the letters
	// that cannot be confused with them, so i, l, o, and u are absent.
	idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

	// idLength is the character count of a Collection identifier.
	idLength = 20
)

// newID returns a Collection identifier: 20 lowercase Crockford base32
// characters over 100 bits from crypto/rand.
// The alphabet is exactly 32 characters,
// so the low five bits of a random byte index it uniformly
// and one byte yields one character.
// The identifier is opaque and not time-ordered;
// a record's createdAt carries the time.
func newID() string {
	var buf [idLength]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(buf[:])

	out := make([]byte, idLength)
	for i, b := range buf {
		out[i] = idAlphabet[b&0x1f]
	}

	return string(out)
}

// ValidID reports whether s is the identifier grammar exactly.
// Route matching accepts this and nothing else, so a path that carries a
// separator or a traversal segment is never read as an identifier.
func ValidID(s string) bool {
	if len(s) != idLength {
		return false
	}
	for i := range len(s) {
		if !isIDChar(s[i]) {
			return false
		}
	}

	return true
}

// isIDChar reports whether c is in the Crockford base32 alphabet.
func isIDChar(c byte) bool {
	for i := range len(idAlphabet) {
		if idAlphabet[i] == c {
			return true
		}
	}

	return false
}
