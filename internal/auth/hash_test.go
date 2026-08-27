package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword([]byte("correct horse"))
	if err != nil {
		t.Fatalf("HashPassword error = %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("bcrypt.Cost error = %v", err)
	}
	if cost != 12 {
		t.Fatalf("cost = %d, want 12", cost)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse")); err != nil {
		t.Fatalf("the hash does not verify its own password: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("wrong")); err == nil {
		t.Fatal("the hash verifies a different password")
	}
}
