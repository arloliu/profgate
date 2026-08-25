package pgo

import "crypto/rand"

const (
	// idAlphabet is Crockford base32 in lowercase: the digits and the letters
	// that cannot be confused with them, so i, l, o, and u are absent.
	idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

	// idLength is the character count of a Collection identifier,
	// and idBits the entropy it encodes.
	idLength      = 20
	idBitsPerChar = 5
	idBits        = idLength * idBitsPerChar

	// idRandBytes covers idBits; the trailing bits of the last byte are unused.
	idRandBytes = (idBits + 7) / 8
)

// newID returns a Collection identifier: 20 lowercase Crockford base32
// characters over 100 bits from crypto/rand.
// It is opaque and not time-ordered; a record's createdAt carries the time.
func newID() string {
	var buf [idRandBytes]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(buf[:])

	out := make([]byte, idLength)
	for i := range out {
		// Take five bits starting at bit i*5, which span at most two bytes.
		bit := i * idBitsPerChar
		b, offset := bit/8, bit%8

		window := uint16(buf[b]) << 8
		if b+1 < len(buf) {
			window |= uint16(buf[b+1])
		}
		out[i] = idAlphabet[(window>>(11-offset))&0x1f]
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
