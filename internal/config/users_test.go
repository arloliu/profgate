package config_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/config"
)

// Hashes shaped like bcrypt output.
// internal/config cannot import x/crypto, so it checks the grammar and the
// cost and never a password; a hand-built string exercises every rule.
const (
	hashCost12  = "$2a$12$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ."
	hashCost12B = "$2y$12$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ/"
	hashCost10  = "$2b$10$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ."
	hashCost09  = "$2a$09$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ."
	hashCost15  = "$2a$15$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ."
	hashShort   = "$2a$12$abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	hashMD5     = "$1$abcdefgh$0123456789012345678901"
)

// oneRealm is the realm set every user in this file names.
func oneRealm() map[string]config.Realm {
	return map[string]config.Realm{"developer": {
		Namespaces: []string{"*"}, Services: []string{"*"}, Profiles: []string{"*"},
	}}
}

// user builds a BasicUser with the fields a case cares about.
func user(name, hash, realm string) config.BasicUser {
	return config.BasicUser{Name: name, PasswordHash: hash, Realm: realm}
}

// wantValidateErr runs ValidateBasicUsers and fails unless the error mentions every want.
func wantValidateErr(t *testing.T, inline, file []config.BasicUser, wants ...string) {
	t.Helper()
	_, err := config.ValidateBasicUsers(inline, file, oneRealm())
	if err == nil {
		t.Fatalf("ValidateBasicUsers() = nil error, want one containing %q", wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ValidateBasicUsers() error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestParseUsers(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		users, err := config.ParseUsers([]byte("users:\n  - name: bob\n    passwordHash: \"" + hashCost12B + "\"\n    realm: developer\n"))
		if err != nil {
			t.Fatalf("ParseUsers() error = %v", err)
		}
		want := []config.BasicUser{user("bob", hashCost12B, "developer")}
		if !slices.Equal(users, want) {
			t.Fatalf("ParseUsers() = %+v, want %+v", users, want)
		}
	})

	// An empty file is a file with no users, not a parse failure:
	// the user set is the inline list plus the file, and either half may be empty.
	t.Run("empty", func(t *testing.T) {
		users, err := config.ParseUsers(nil)
		if err != nil || users != nil {
			t.Fatalf("ParseUsers(nil) = %+v, %v, want nil, nil", users, err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		_, err := config.ParseUsers([]byte("users:\n  - name: bob\n    password: hunter2\n"))
		if err == nil || !strings.Contains(err.Error(), "field password not found in type config.BasicUser") {
			t.Fatalf("ParseUsers() error = %v, want it to name the unknown key", err)
		}
	})

	// The file holds one document. Users written after a `---` separator
	// would otherwise be dropped without a word, and an operator would
	// believe they were active.
	t.Run("second document", func(t *testing.T) {
		_, err := config.ParseUsers([]byte("users: []\n---\nusers:\n  - name: bob\n    passwordHash: \"" + hashCost12B + "\"\n    realm: developer\n"))
		if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
			t.Fatalf("ParseUsers() error = %v, want it to refuse the second document", err)
		}
	})

	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := config.ParseUsers([]byte("groups: []\n"))
		if err == nil || !strings.Contains(err.Error(), "field groups not found") {
			t.Fatalf("ParseUsers() error = %v, want it to name the unknown key", err)
		}
	})
}

// A users file reaches the same result whether it arrives as a path or as bytes,
// because the poller in internal/auth reads the bytes itself and calls ParseUsers.
func TestLoadUsersFile(t *testing.T) {
	t.Run("same as ParseUsers", func(t *testing.T) {
		for _, name := range []string{"auth-users.yaml", "auth-users-unknown.yaml"} {
			t.Run(name, func(t *testing.T) {
				path := fixture(name)
				b, err := os.ReadFile(path) //nolint:gosec // the path is a testdata fixture this test names
				if err != nil {
					t.Fatalf("ReadFile(%q) error = %v", path, err)
				}
				fromBytes, bytesErr := config.ParseUsers(b)
				fromPath, pathErr := config.LoadUsersFile(path)
				if !slices.Equal(fromBytes, fromPath) {
					t.Fatalf("LoadUsersFile() = %+v, ParseUsers() = %+v", fromPath, fromBytes)
				}
				if (bytesErr == nil) != (pathErr == nil) {
					t.Fatalf("LoadUsersFile() error = %v, ParseUsers() error = %v", pathErr, bytesErr)
				}
			})
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		if _, err := config.LoadUsersFile(fixture("nonexistent.yaml")); err == nil {
			t.Fatal("LoadUsersFile() = nil error, want one")
		}
	})
}

func TestValidateBasicUsers(t *testing.T) {
	// One cost across the whole set is what lets an unknown name be compared
	// against a dummy hash that costs exactly what a real one costs.
	t.Run("shared cost", func(t *testing.T) {
		cost, err := config.ValidateBasicUsers(
			[]config.BasicUser{user("alice", hashCost12, "developer")},
			[]config.BasicUser{user("bob", hashCost12B, "developer")},
			oneRealm(),
		)
		if err != nil || cost != 12 {
			t.Fatalf("ValidateBasicUsers() = %d, %v, want 12, nil", cost, err)
		}
	})

	t.Run("no users", func(t *testing.T) {
		wantValidateErr(t, nil, nil, "auth.basic", "at least one user")
	})

	t.Run("name rules", func(t *testing.T) {
		for _, tc := range []struct{ name, value string }{
			{"empty", ""},
			{"too long", strings.Repeat("a", 257)},
			{"colon", "a:b"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				wantValidateErr(t, []config.BasicUser{user(tc.value, hashCost12, "developer")},
					nil, "auth.basic.users[0].name")
			})
		}
	})

	t.Run("hash grammar", func(t *testing.T) {
		for _, tc := range []struct{ name, hash string }{
			{"plaintext", "hunter2"},
			{"short", hashShort},
			{"md5", hashMD5},
		} {
			t.Run(tc.name, func(t *testing.T) {
				wantValidateErr(t, []config.BasicUser{user("alice", tc.hash, "developer")},
					nil, "auth.basic.users[0].passwordHash")
			})
		}
	})

	// The cost range bounds the CPU an operator can spend on one comparison.
	t.Run("cost range", func(t *testing.T) {
		for _, tc := range []struct{ name, hash, cost string }{
			{"too cheap", hashCost09, "9"},
			{"too expensive", hashCost15, "15"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				wantValidateErr(t, []config.BasicUser{user("alice", tc.hash, "developer")},
					nil, "auth.basic.users[0].passwordHash", `"alice"`, tc.cost)
			})
		}
	})

	t.Run("mixed costs", func(t *testing.T) {
		wantValidateErr(t, []config.BasicUser{
			user("alice", hashCost12, "developer"),
			user("bob", hashCost10, "developer"),
		}, nil, "auth.basic.users[1].passwordHash", `"alice"`, `"bob"`, "12", "10")
	})

	// A file that disagrees with the inline users is named as the file,
	// because that is the half an operator rotates without touching the ConfigMap.
	t.Run("mixed costs across file", func(t *testing.T) {
		wantValidateErr(t,
			[]config.BasicUser{user("alice", hashCost12, "developer")},
			[]config.BasicUser{user("bob", hashCost10, "developer")},
			"auth.basic.usersFile", `"alice"`, `"bob"`, "12", "10")
	})

	t.Run("realm", func(t *testing.T) {
		wantValidateErr(t, []config.BasicUser{user("alice", hashCost12, "nobody")},
			nil, "auth.basic.users[0].realm")
	})

	t.Run("realm required", func(t *testing.T) {
		wantValidateErr(t, []config.BasicUser{user("alice", hashCost12, "")},
			nil, "auth.basic.users[0].realm")
	})

	t.Run("duplicate names", func(t *testing.T) {
		wantValidateErr(t,
			[]config.BasicUser{user("alice", hashCost12, "developer")},
			[]config.BasicUser{user("alice", hashCost12B, "developer")},
			"auth.basic.usersFile", `"alice"`)
	})
}
