package auth

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/arloliu/profgate/internal/config"
)

// usersYAML renders a users file with one user at the given hash.
func usersYAML(name, hash, realm string) []byte {
	return []byte("users:\n  - name: " + name + "\n    passwordHash: \"" + hash + "\"\n    realm: " + realm + "\n")
}

// fileFixture builds Basic over a real users file holding dave, then points
// the poller's reader at whatever each test scripts.
type fileFixture struct {
	b   *Basic
	cfg *config.Config
	cmp *countingComparer
	rec *reloadRecorder
}

func newFileFixture(t *testing.T) *fileFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, usersYAML("dave", hashHunter10, "ops"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := basicConfig(16)
	cfg.Auth.Basic.UsersFile = path
	rec := &reloadRecorder{}
	b, err := NewBasic(cfg, BasicOptions{Logger: slog.New(slog.DiscardHandler), Recorder: rec})
	if err != nil {
		t.Fatalf("NewBasic error = %v", err)
	}
	cmp := &countingComparer{}
	b.compare = cmp

	return &fileFixture{b: b, cfg: cfg, cmp: cmp, rec: rec}
}

func (f *fileFixture) auth(name, password string) (Principal, error) {
	return f.b.Authenticate(context.Background(), request(basicHeader(name, password)), f.cfg)
}

func (f *fileFixture) serve(t *testing.T, b []byte) {
	t.Helper()
	f.b.poller.read = func(string) ([]byte, error) { return b, nil }
}

func TestBasicUsersFile(t *testing.T) {
	t.Run("file user", func(t *testing.T) {
		f := newFileFixture(t)
		p, err := f.auth("dave", "hunter2")
		wantPrincipal(t, p, err, "dave", "ops")
		_, err = f.auth("bob", "secret")
		wantFailure(t, err, 401, ReasonBadCredential)

		f.serve(t, usersYAML("bob", hashSecret10, "developer"))
		f.b.poller.Poll()
		p, err = f.auth("bob", "secret")
		wantPrincipal(t, p, err, "bob", "developer")
		if got := f.rec.reloads(); !slices.Equal(got, []string{"users/ok"}) {
			t.Fatalf("reloads = %v, want [users/ok]", got)
		}
	})

	t.Run("startup bytes are not applied again", func(t *testing.T) {
		// The file the constructor loaded is what the first poll compares
		// against: an unchanged file must not swap the set or log a reload
		// every 30 seconds.
		f := newFileFixture(t)
		before := f.b.set.Load()
		f.serve(t, usersYAML("dave", hashHunter10, "ops"))
		f.b.poller.Poll()
		if f.b.set.Load() != before {
			t.Fatal("an unchanged users file swapped the set")
		}
		if got := f.rec.reloads(); !slices.Equal(got, []string{"users/ok"}) {
			t.Fatalf("reloads = %v, want [users/ok]", got)
		}
	})

	t.Run("file replaced", func(t *testing.T) {
		f := newFileFixture(t)
		f.serve(t, usersYAML("bob", hashSecret10, "developer"))
		f.b.poller.Poll()
		f.serve(t, usersYAML("carol", hashSecret10, "developer"))
		f.b.poller.Poll()
		p, err := f.auth("carol", "secret")
		wantPrincipal(t, p, err, "carol", "developer")
		f.cmp.calls.Store(0)
		_, err = f.auth("bob", "secret")
		wantFailure(t, err, 401, ReasonBadCredential)
		if got := f.cmp.calls.Load(); got != 1 {
			t.Fatalf("comparisons = %d, want 1", got)
		}
	})

	rejected := []struct {
		name string
		file []byte
	}{
		{"file unparseable", []byte("users: [")},
		{"file cost differs", usersYAML("bob", hashSecret12, "developer")},
		{"file realm unknown", usersYAML("bob", hashSecret10, "nobody")},
		{"file duplicates inline", usersYAML("alice", hashSecret10, "developer")},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			f := newFileFixture(t)
			f.serve(t, tc.file)
			f.b.poller.Poll()
			p, err := f.auth("dave", "hunter2")
			wantPrincipal(t, p, err, "dave", "ops")
			if got := f.rec.reloads(); !slices.Equal(got, []string{"users/failed"}) {
				t.Fatalf("reloads = %v, want [users/failed]", got)
			}
		})
	}

	t.Run("snapshot", func(t *testing.T) {
		f := newFileFixture(t)
		f.cmp.block, f.cmp.started = make(chan struct{}), make(chan struct{}, 1)
		done := make(chan struct{})
		var p Principal
		var err error
		go func() {
			p, err = f.auth("dave", "hunter2")
			close(done)
		}()
		<-f.cmp.started
		f.serve(t, usersYAML("carol", hashSecret10, "developer"))
		f.b.poller.Poll()
		close(f.cmp.block)
		<-done
		wantPrincipal(t, p, err, "dave", "ops")
	})
}
