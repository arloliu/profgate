package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateTemp(t *testing.T) {
	dir := t.TempDir()
	f, err := createTemp(dir, "tokens.json", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if filepath.Dir(f.Name()) != dir {
		t.Fatalf("temporary file %q is outside %q", f.Name(), dir)
	}
	if filepath.Base(f.Name()) == "tokens.json" {
		t.Fatal("temporary file already carries the final name")
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary file mode = %v, want 0600 at creation", info.Mode().Perm())
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(dir, "tokens.json", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: a path this test built itself
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %s, want only tokens.json: the rename left a temporary file", strings.Join(names, ", "))
	}

	t.Run("a failed write leaves the previous file", func(t *testing.T) {
		missing := filepath.Join(dir, "absent")
		if err := atomicWrite(missing, "tokens.json", []byte("x"), 0o600); err == nil {
			t.Fatal("atomicWrite into a missing directory = nil error")
		}
		got, err := os.ReadFile(path) //nolint:gosec // G304: a path this test built itself
		if err != nil || string(got) != "new" {
			t.Fatalf("previous file = %q, %v; want new", got, err)
		}
	})
}
