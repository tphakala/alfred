package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/alfred/internal/fsutil"
)

func TestWriteFileAtomic_CreatesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Create with 0644.
	if err := fsutil.WriteFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after create: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("content = %q, want %q", got, "first")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o644 {
		t.Errorf("mode after create = %v, want 0644", info.Mode().Perm())
	}

	// Overwrite with a DIFFERENT perm: the rename must apply the requested mode,
	// not inherit the pre-existing file's.
	if err := fsutil.WriteFileAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after overwrite: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode after overwrite = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteFileAtomic_RenameFailureRemovesOrphanTemp(t *testing.T) {
	// Force a failure AFTER the temp file is created: CreateTemp succeeds, but
	// renaming onto an existing directory fails (EISDIR). The deferred cleanup
	// must then remove the temp so no .atomic-*.tmp orphan is left behind.
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	if err := fsutil.WriteFileAtomic(target, []byte("data"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic() error = nil, want a rename-onto-directory failure")
	}

	// Only the pre-created target directory must remain; the temp is cleaned up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Errorf("temp orphan left behind after rename failure; dir has %d entries", len(entries))
	}
}

func TestWriteFileAtomic_NoTempLitter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for _, content := range []string{"a", "b", "c"} {
		if err := fsutil.WriteFileAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir = %v, want exactly [config.yaml] with no temp litter", names)
	}
}

func TestWriteFileAtomic_FailureLeavesTargetAndNoOrphan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := fsutil.WriteFileAtomic(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read-only dir: CreateTemp cannot create the temp file, so the write fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := fsutil.WriteFileAtomic(path, []byte("updated"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic() error = nil, want a create-temp failure")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("existing file must be untouched on failure, got %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Errorf("dir must contain only config.yaml with no temp orphan, got %d entries", len(entries))
	}
}
