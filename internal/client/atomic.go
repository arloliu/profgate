package client

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeFunc creates a temporary file in dir with mode, writes data, and
// renames it over name, so a crash mid-write leaves the previous file.
// The temporary file is created with the final mode rather than chmod-ed.
// It is the seam under the atomic write, so a test observes the sequence.
type writeFunc func(dir, name string, data []byte, mode os.FileMode) error

// atomicWrite is the writeFunc every non-test caller uses.
func atomicWrite(dir, name string, data []byte, mode os.FileMode) (err error) {
	f, err := createTemp(dir, name, mode)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// createTemp opens a new file in dir with exactly mode, named after name
// with a random suffix so two writers never share one temporary file.
// os.CreateTemp always uses 0600, which is why this is not that.
func createTemp(dir, name string, mode os.FileMode) (*os.File, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, fmt.Errorf("temporary file name: %w", err)
		}
		path := filepath.Join(dir, "."+name+"."+hex.EncodeToString(suffix[:])+".tmp")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // the caller chose dir and mode; a temporary file beside the target is the purpose
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create temporary file in %s: %w", dir, err)
		}
	}
	return nil, fmt.Errorf("create temporary file in %s: every name tried exists", dir)
}
