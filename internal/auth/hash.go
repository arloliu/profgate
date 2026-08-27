package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// hashCost is the cost `profgate auth hash` mints at: inside the 10–14 range
// the configuration accepts, and slow enough that a leaked users file is
// expensive to brute-force on today's hardware.
const hashCost = 12

// HashPassword returns the bcrypt hash of password at cost 12,
// for `profgate auth hash`.
func HashPassword(password []byte) (string, error) {
	hash, err := bcrypt.GenerateFromPassword(password, hashCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}

	return string(hash), nil
}
