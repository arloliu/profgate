package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// lockTimeout bounds how long Lock waits for another holder.
const lockTimeout = 30 * time.Second

// lockInterval is how long Lock sleeps between attempts.
const lockInterval = 100 * time.Millisecond

// digestName is the ad-hoc entry name: a fixed 32 hex characters after the prefix.
var digestName = regexp.MustCompile(`^adhoc-[0-9a-f]{32}$`)

// Entry is one cache file: a credential for one gateway origin.
type Entry struct {
	Origin           string    `json:"origin"`
	Issuer           string    `json:"issuer"`
	ClientID         string    `json:"clientID"`
	TokenType        string    `json:"tokenType"`
	Token            string    `json:"token"`
	ExpiresAt        time.Time `json:"expiresAt"`
	RefreshToken     string    `json:"refreshToken,omitempty"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt,omitzero"`
	ObtainedAt       time.Time `json:"obtainedAt"`
}

// Store is the tokens directory.
// Every method checks modes and never repairs them:
// a permission something else widened is a fact the client cannot tell from an attack,
// and narrowing it silently would hide both.
type Store struct {
	dir   string
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	write writeFunc
}

// StoreOptions is everything a Store is built from; a nil clock or sleeper is the real one.
type StoreOptions struct {
	Dir   string
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// NewStore builds the Store over the tokens directory, which need not exist yet.
func NewStore(o StoreOptions) *Store {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	return &Store{dir: o.Dir, now: now, sleep: sleep, write: atomicWrite}
}

// realSleep waits d or until ctx ends.
func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Read returns the entry for name, or false when there is none.
func (s *Store) Read(name string) (Entry, bool, error) {
	if err := s.check(name); err != nil {
		return Entry{}, false, err
	}
	data, err := os.ReadFile(s.path(name, ".json")) //nolint:gosec // the name passed its grammar and the modes were checked
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("read token cache: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false, fmt.Errorf("%s: %w", s.path(name, ".json"), err)
	}
	return e, true, nil
}

// Write replaces the entry atomically, 0600 in a 0700 directory.
func (s *Store) Write(name string, e Entry) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", s.dir, err)
	}
	if err := s.check(name); err != nil {
		return err
	}
	data, err := json.Marshal(e) //nolint:gosec // G117: the refresh token is what the 0600 cache file holds
	if err != nil {
		return fmt.Errorf("encode token cache: %w", err)
	}
	if err := s.write(s.dir, name+".json", data, 0o600); err != nil {
		return fmt.Errorf("write token cache: %w", err)
	}
	return nil
}

// Delete removes the entry; a missing entry is not an error.
func (s *Store) Delete(name string) error {
	if err := s.check(name); err != nil {
		return err
	}
	err := os.Remove(s.path(name, ".json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete token cache: %w", err)
	}
	return nil
}

// Lock takes the exclusive lock on <name>.lock and returns the release.
// The lock is a file created exclusively,
// which every platform the binary builds for supports;
// it gives up after 30 seconds rather than breaking a
// lock it did not take, because breaking one would defeat the serialization the lock exists for.
func (s *Store) Lock(ctx context.Context, name string) (func() error, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", s.dir, err)
	}
	lock := s.path(name, ".lock")
	deadline := s.now().Add(lockTimeout)
	for {
		if err := s.check(name); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the name passed its grammar; creating the lock file is the purpose
		if err == nil {
			_ = f.Close()
			return func() error {
				if err := os.Remove(lock); err != nil {
					return fmt.Errorf("release %s: %w", lock, err)
				}
				return nil
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create %s: %w", lock, err)
		}
		if !s.now().Before(deadline) {
			return nil, fmt.Errorf("%s is held by another profgate; waited %s. Remove the file if no other profgate is running", lock, lockTimeout)
		}
		if err := s.sleep(ctx, lockInterval); err != nil {
			return nil, err
		}
	}
}

// Usable reports whether e may be sent for this command:
// its origin equals the resolved gateway's byte for byte, and its issuer, client, and token type still match the context.
// With no snapshot to compare against — --server alone, or a context no
// login has recorded — the entry carries its own values and only the
// origin is checked.
func (e Entry) Usable(s Settings) error {
	if e.Origin != s.Origin {
		return fmt.Errorf("the cached token for %s was obtained for %s, and the command resolves to %s", s.describe(), e.Origin, s.Origin)
	}
	a := s.Context.Auth
	if a.Mode == "" {
		return nil
	}
	switch {
	case e.Issuer != a.Issuer:
		return fmt.Errorf("the cached token was issued by %s, and the context names %s", e.Issuer, a.Issuer)
	case e.ClientID != a.ClientID:
		return fmt.Errorf("the cached token was issued to client %q, and the context names %q", e.ClientID, a.ClientID)
	case e.TokenType != a.TokenType:
		return fmt.Errorf("the cached token is an %s token, and the context expects %s", e.TokenType, a.TokenType)
	}
	return nil
}

// path joins a checked name and suffix under the directory.
func (s *Store) path(name, suffix string) string {
	return filepath.Join(s.dir, name+suffix)
}

// checkName admits a context label or a digest name and nothing else, so no
// name reaches a path outside the directory.
func checkName(name string) error {
	if isDNSLabel(name) || digestName.MatchString(name) {
		return nil
	}
	return fmt.Errorf("%w: token cache name %q is neither a context name nor a digest name", ErrUsage, name)
}

// check requires the name to pass its grammar and the directory, the entry,
// and the lock file to grant nothing to group or other; a missing path is fine.
func (s *Store) check(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkMode(s.dir, 0o700); err != nil {
		return err
	}
	if err := checkMode(s.path(name, ".json"), 0o600); err != nil {
		return err
	}
	return checkMode(s.path(name, ".lock"), 0o600)
}

// checkMode refuses a path that grants any group or other bit, naming the
// path and the mode expected; it never chmods.
func checkMode(path string, want os.FileMode) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s is mode %04o and grants group or other; expected %04o", ErrUsage, path, perm, want)
	}
	return nil
}
