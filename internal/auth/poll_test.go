package auth

import (
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/arloliu/profgate/internal/metrics"
)

// reloadRecorder counts AuthFileReload calls as "file/result" strings.
type reloadRecorder struct {
	metrics.Noop
	mu    sync.Mutex
	calls []string
}

func (r *reloadRecorder) AuthFileReload(file, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, file+"/"+result)
}

func (r *reloadRecorder) reloads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.calls)
}

// sequenceReader serves reads one after another; a nil entry is a read error.
func sequenceReader(t *testing.T, reads ...[]byte) fileReader {
	t.Helper()
	i := 0

	return func(string) ([]byte, error) {
		if i >= len(reads) {
			t.Fatalf("read %d: no read scripted", i+1)
		}
		b := reads[i]
		i++
		if b == nil {
			return nil, errors.New("scripted read error")
		}

		return b, nil
	}
}

func TestFilePoller(t *testing.T) {
	type fixture struct {
		p       *filePoller
		rec     *reloadRecorder
		applied [][]byte
	}
	build := func(applyErr error, reads ...[]byte) *fixture {
		f := &fixture{rec: &reloadRecorder{}}
		f.p = newFilePoller("/x", func(b []byte) error {
			if applyErr != nil {
				return applyErr
			}
			f.applied = append(f.applied, b)

			return nil
		}, "users", 0, f.rec, slog.New(slog.DiscardHandler))
		f.p.read = sequenceReader(t, reads...)

		return f
	}

	t.Run("apply on first and changed reads only", func(t *testing.T) {
		f := build(nil, []byte("a"), []byte("a"), []byte("b"))
		f.p.Poll()
		f.p.Poll()
		f.p.Poll()
		if len(f.applied) != 2 || string(f.applied[0]) != "a" || string(f.applied[1]) != "b" {
			t.Fatalf("applied = %q, want [a b]", f.applied)
		}
		want := []string{"users/ok", "users/ok", "users/ok"}
		if got := f.rec.reloads(); !slices.Equal(got, want) {
			t.Fatalf("reloads = %v, want %v", got, want)
		}
	})

	t.Run("read error keeps the previous state", func(t *testing.T) {
		f := build(nil, []byte("a"), nil, []byte("a"))
		f.p.Poll()
		f.p.Poll()
		f.p.Poll()
		if len(f.applied) != 1 {
			t.Fatalf("applied %d times, want 1: a failed read must not apply, and the unchanged read after it must not either", len(f.applied))
		}
		want := []string{"users/ok", "users/failed", "users/ok"}
		if got := f.rec.reloads(); !slices.Equal(got, want) {
			t.Fatalf("reloads = %v, want %v", got, want)
		}
	})

	t.Run("apply error keeps the previous state", func(t *testing.T) {
		f := build(errors.New("bad file"), []byte("a"), []byte("a"))
		f.p.Poll()
		f.p.Poll()
		if len(f.applied) != 0 {
			t.Fatalf("applied %d times, want 0", len(f.applied))
		}
		// The bytes that failed to apply are not remembered, so the same
		// bytes are tried again rather than silently skipped as unchanged.
		want := []string{"users/failed", "users/failed"}
		if got := f.rec.reloads(); !slices.Equal(got, want) {
			t.Fatalf("reloads = %v, want %v", got, want)
		}
	})
}
