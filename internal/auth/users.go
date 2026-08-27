package auth

import (
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"github.com/arloliu/profgate/internal/config"
)

// userSet is the file half of the user set and the dummy hash every
// comparison against an unknown name runs at.
// It is swapped whole, so a request that loaded it judges the credential
// against one consistent set.
type userSet struct {
	users map[string]config.BasicUser
	dummy []byte
}

// newUserSet validates the file users against the inline users and realms the
// gateway started with, then mints the dummy hash at the cost the whole set
// shares.
// Every file poll is judged against the same startup policy: the inline users
// only change when a configuration reload replaces them, and none exists.
func newUserSet(inline, file []config.BasicUser, realms map[string]config.Realm) (*userSet, error) {
	cost, err := config.ValidateBasicUsers(inline, file, realms)
	if err != nil {
		return nil, err
	}
	dummy, err := dummyHash(cost)
	if err != nil {
		return nil, err
	}
	set := &userSet{users: make(map[string]config.BasicUser, len(file)), dummy: dummy}
	for _, u := range file {
		set.users[u.Name] = u
	}

	return set, nil
}

// dummyHash is what an unknown name is compared against: a bcrypt hash of
// random bytes at the shared cost, so the unknown-user path does the same
// work as the wrong-password path and belongs to nobody.
func dummyHash(cost int) ([]byte, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, fmt.Errorf("auth: random source: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword(secret[:], cost)
	if err != nil {
		return nil, fmt.Errorf("auth: dummy hash: %w", err)
	}

	return hash, nil
}

// applyUsersFile is the poller's apply: it parses the bytes and swaps the set
// only when the whole merged set validates.
func (b *Basic) applyUsersFile(raw []byte) error {
	file, err := config.ParseUsers(raw)
	if err != nil {
		return fmt.Errorf("parse users file: %w", err)
	}
	set, err := newUserSet(b.inline, file, b.realms)
	if err != nil {
		return err
	}
	b.set.Store(set)

	return nil
}
