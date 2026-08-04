// Package fsutil provides small filesystem helpers shared across Alfred.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path atomically. It writes a temp file in the
// same directory, fsyncs it, then renames it over path. A crash mid-write leaves
// either the previous file or the complete new one, never a truncated file.
//
// After the rename it makes a best-effort fsync of the parent directory so the
// rename is durable across a power loss. That step is deliberately best-effort:
// a filesystem that does not support directory fsync (or Windows, where fsync on
// a directory handle fails) must not turn an already committed write into a
// reported failure. The primary guarantee (never a torn or truncated file) is
// met by the temp fsync plus the atomic rename regardless.
//
// The temp file is hidden (leading dot) and carries a ".tmp" suffix, so an
// orphan left by a failed write is never mistaken for the real file by
// extension-filtering directory scans (e.g. loaders that only read ".yaml").
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".atomic-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		// Close is safe to call again after the explicit Close below (it returns
		// os.ErrClosed, which we ignore); the defer guarantees the fd is released
		// and the temp file removed even on an error or panic path.
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file to %q: %w", path, err)
	}
	success = true

	// Best-effort durability of the rename (see the doc comment): a failure here
	// does not undo the committed write, so it must not be reported as an error.
	_ = syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename within it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for fsync: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %q: %w", dir, err)
	}
	return nil
}
