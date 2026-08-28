package client

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock is a clock a sleeper advances; nothing in these tests waits.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func testStore(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tokens")
	clock := &fakeClock{now: time.Date(2026, 8, 28, 9, 30, 12, 0, time.UTC)}
	return NewStore(StoreOptions{Dir: dir, Now: clock.Now, Sleep: clock.Sleep}), clock, dir
}

func testEntry() Entry {
	obtained := time.Date(2026, 8, 28, 9, 30, 12, 0, time.UTC)
	return Entry{
		Origin:       "https://profgate.example:443",
		Issuer:       "https://issuer.example/realms/eng",
		ClientID:     "profgate",
		TokenType:    "id",
		Token:        "tok",
		ExpiresAt:    obtained.Add(5 * time.Minute),
		RefreshToken: "rt",
		ObtainedAt:   obtained,
	}
}

func testSettings(t *testing.T, server string) Settings {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	e := testEntry()
	return Settings{
		ContextName: "prod",
		Context: Context{Server: server, Auth: AuthSnap{
			Mode: "oidc", Issuer: e.Issuer, ClientID: e.ClientID, TokenType: e.TokenType,
		}},
		Server:    u,
		Origin:    CanonicalOrigin(u),
		CacheName: "prod",
	}
}

func TestStoreWriteModes(t *testing.T) {
	s, _, dir := testStore(t)
	if err := s.Write("prod", testEntry()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, want 0700", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(dir, "prod.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}
	got, ok, err := s.Read("prod")
	if err != nil || !ok {
		t.Fatalf("Read = %v, %v; want the entry", ok, err)
	}
	if got != testEntry() {
		t.Fatalf("Read = %+v, want %+v", got, testEntry())
	}
}

func TestStoreWriteSeam(t *testing.T) {
	s, _, dir := testStore(t)
	if err := s.Write("prod", testEntry()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "prod.json")
	old, err := os.ReadFile(path) //nolint:gosec // G304: a path this test built itself
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	s.write = func(wdir, name string, data []byte, mode os.FileMode) error {
		calls++
		if wdir != dir || name != "prod.json" || mode != 0o600 {
			t.Fatalf("write(%q, %q, mode %v), want (%q, prod.json, 0600)", wdir, name, mode, dir)
		}
		// Before the rename the previous file is intact.
		before, err := os.ReadFile(path) //nolint:gosec // G304: a path this test built itself
		if err != nil || string(before) != string(old) {
			t.Fatalf("file before the rename = %q, %v; want the old contents", before, err)
		}
		f, err := createTemp(wdir, name, mode)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(f.Name()) != dir {
			t.Fatalf("temporary file %q is outside %q", f.Name(), dir)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return os.Rename(f.Name(), filepath.Join(wdir, name))
	}
	e := testEntry()
	e.Token = "rotated"
	if err := s.Write("prod", e); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("write seam called %d times, want 1", calls)
	}
	got, _, err := s.Read("prod")
	if err != nil || got.Token != "rotated" {
		t.Fatalf("after the rename the entry is %+v, %v; want the new contents", got, err)
	}
}

func TestStoreModeRefusals(t *testing.T) {
	cases := []struct {
		name  string
		widen func(dir string) string // returns the path that is too wide
		want  string
	}{
		{"the tokens directory", func(dir string) string {
			return dir
		}, "0700"},
		{"a cache file", func(dir string) string {
			return filepath.Join(dir, "prod.json")
		}, "0600"},
		{"a lock file", func(dir string) string {
			return filepath.Join(dir, "prod.lock")
		}, "0600"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" granting a group or other bit", func(t *testing.T) {
			s, _, dir := testStore(t)
			if err := s.Write("prod", testEntry()); err != nil {
				t.Fatal(err)
			}
			path := tc.widen(dir)
			if filepath.Base(path) == "prod.lock" {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, info.Mode().Perm()|0o040); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			ops := map[string]func() error{
				"Read":   func() error { _, _, err := s.Read("prod"); return err },
				"Write":  func() error { return s.Write("prod", testEntry()) },
				"Delete": func() error { return s.Delete("prod") },
				"Lock": func() error {
					release, err := s.Lock(ctx, "prod")
					if err == nil {
						_ = release()
					}
					return err
				},
			}
			for op, fn := range ops {
				err := fn()
				if err == nil {
					t.Fatalf("%s with %s too wide = nil error", op, path)
				}
				if !errors.Is(err, ErrUsage) {
					t.Fatalf("%s error %v is not ErrUsage", op, err)
				}
				if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s error %q names neither %s nor mode %s", op, err, path, tc.want)
				}
			}
			// The mode was reported, never repaired.
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode().Perm() != info.Mode().Perm()|0o040 {
				t.Fatalf("mode of %s became %v; the store repaired it", path, after.Mode().Perm())
			}
		})
	}
}

func TestEntryUsable(t *testing.T) {
	t.Run("an origin that differs from the resolved gateway", func(t *testing.T) {
		s := testSettings(t, "https://someone-elses.example")
		err := testEntry().Usable(s)
		if err == nil {
			t.Fatal("Usable = nil for a different origin")
		}
		for _, want := range []string{"https://profgate.example:443", "https://someone-elses.example:443"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %s", err, want)
			}
		}
	})

	t.Run("https://host and https://host:443 are the same entry", func(t *testing.T) {
		if err := testEntry().Usable(testSettings(t, "https://profgate.example")); err != nil {
			t.Fatal(err)
		}
		if err := testEntry().Usable(testSettings(t, "https://PROFGATE.example:443")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("http://host and https://host are different entries", func(t *testing.T) {
		if err := testEntry().Usable(testSettings(t, "http://profgate.example")); err == nil {
			t.Fatal("Usable = nil for http:// against a cached https://")
		}
	})

	mismatches := []struct {
		name string
		edit func(*AuthSnap)
	}{
		{"issuer", func(a *AuthSnap) { a.Issuer = "https://other.example" }},
		{"client identifier", func(a *AuthSnap) { a.ClientID = "other" }},
		{"token type", func(a *AuthSnap) { a.TokenType = "access" }},
	}
	for _, tc := range mismatches {
		t.Run("a "+tc.name+" that differs from the context", func(t *testing.T) {
			s := testSettings(t, "https://profgate.example")
			tc.edit(&s.Context.Auth)
			if err := testEntry().Usable(s); err == nil {
				t.Fatalf("Usable = nil when the %s differs", tc.name)
			}
		})
	}
}

func TestStoreLock(t *testing.T) {
	t.Run("held by another holder is acquired after the release", func(t *testing.T) {
		s, clock, dir := testStore(t)
		if err := s.Write("prod", testEntry()); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(dir, "prod.lock")
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		sleeps := 0
		start := clock.now
		s.sleep = func(ctx context.Context, d time.Duration) error {
			sleeps++
			if sleeps == 3 {
				if err := os.Remove(lock); err != nil {
					t.Fatal(err)
				}
			}
			return clock.Sleep(ctx, d)
		}
		release, err := s.Lock(context.Background(), "prod")
		if err != nil {
			t.Fatal(err)
		}
		if sleeps != 3 {
			t.Fatalf("slept %d times, want 3: acquired only after the holder released", sleeps)
		}
		if clock.now.Sub(start) > 30*time.Second {
			t.Fatalf("clock advanced %v, past the bound", clock.now.Sub(start))
		}
		if _, err := os.Stat(lock); err != nil {
			t.Fatalf("lock file after Lock: %v; want it held", err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock file after release: %v; want it removed", err)
		}
	})

	t.Run("held past 30 seconds names the lock file", func(t *testing.T) {
		s, clock, dir := testStore(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(dir, "prod.lock")
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		start := clock.now
		_, err := s.Lock(context.Background(), "prod")
		if err == nil {
			t.Fatal("Lock on a held lock = nil error after the bound")
		}
		if !strings.Contains(err.Error(), lock) {
			t.Fatalf("error %q does not name %s", err, lock)
		}
		if elapsed := clock.now.Sub(start); elapsed < 30*time.Second {
			t.Fatalf("gave up after %v, before the 30 second bound", elapsed)
		}
		if _, err := os.Stat(lock); err != nil {
			t.Fatalf("lock file after giving up: %v; the store broke a lock it did not take", err)
		}
	})

	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		s, _, dir := testStore(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "prod.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		if _, err := s.Lock(ctx, "prod"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Lock under a cancelled context = %v, want context.Canceled", err)
		}
	})
}

func TestStoreNames(t *testing.T) {
	bad := []string{"", "Prod", "../prod", "prod.lock", "adhoc-", "adhoc-XYZ", "a/b"}
	for _, name := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			s, _, dir := testStore(t)
			if _, _, err := s.Read(name); !errors.Is(err, ErrUsage) {
				t.Fatalf("Read(%q) = %v, want ErrUsage", name, err)
			}
			if err := s.Write(name, testEntry()); !errors.Is(err, ErrUsage) {
				t.Fatalf("Write(%q) = %v, want ErrUsage", name, err)
			}
			if err := s.Delete(name); !errors.Is(err, ErrUsage) {
				t.Fatalf("Delete(%q) = %v, want ErrUsage", name, err)
			}
			if _, err := s.Lock(context.Background(), name); !errors.Is(err, ErrUsage) {
				t.Fatalf("Lock(%q) = %v, want ErrUsage", name, err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the directory exists after a rejected name: %v", err)
			}
		})
	}
	good := []string{"prod", "a", "adhoc-" + strings.Repeat("0f", 16)}
	for _, name := range good {
		t.Run("accepts "+name, func(t *testing.T) {
			s, _, _ := testStore(t)
			if err := s.Write(name, testEntry()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoreDelete(t *testing.T) {
	s, _, dir := testStore(t)
	if err := s.Delete("prod"); err != nil {
		t.Fatalf("Delete of a missing entry in a missing directory = %v", err)
	}
	if err := s.Write("prod", testEntry()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("prod"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Read("prod"); err != nil || ok {
		t.Fatalf("Read after Delete = %v, %v; want no entry", ok, err)
	}
	if err := s.Delete("prod"); err != nil {
		t.Fatalf("Delete of a missing entry = %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the directory after Delete: %v", err)
	}
}

func TestStoreWriteOmitsZeroRefreshExpiry(t *testing.T) {
	s, _, dir := testStore(t)
	e := testEntry()
	if err := s.Write("prod", e); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "prod.json")) //nolint:gosec // G304: a path this test built itself
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "refreshExpiresAt") {
		t.Fatalf("file %s carries refreshExpiresAt for a zero time", data)
	}
	e.RefreshExpiresAt = RefreshExpiryOf(36000, e.ObtainedAt)
	if err := s.Write("prod", e); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Read("prod")
	if err != nil {
		t.Fatal(err)
	}
	if !got.RefreshExpiresAt.Equal(e.ObtainedAt.Add(36000 * time.Second)) {
		t.Fatalf("refreshExpiresAt = %v, want obtainedAt plus 36000 seconds", got.RefreshExpiresAt)
	}
}

func TestStoreReadMissing(t *testing.T) {
	s, _, _ := testStore(t)
	if _, ok, err := s.Read("prod"); err != nil || ok {
		t.Fatalf("Read with no directory = %v, %v; want no entry", ok, err)
	}
}
