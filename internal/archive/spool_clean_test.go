package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// Spool files live in a per-config subdirectory, so a sweep that only looked at
// the top level would leave every orphan behind after a crash.
func TestCleanSpoolDirRemovesNestedOrphans(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "orders-archive")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		filepath.Join(root, "toplevel.tmp"),
		filepath.Join(nested, "leftover-1.tmp"),
		filepath.Join(nested, "leftover-2.tmp"),
	}
	keep := filepath.Join(nested, "not-a-spool.txt")

	for _, p := range append(paths, keep) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := CleanSpoolDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if removed != len(paths) {
		t.Errorf("removed %d files, want %d", removed, len(paths))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-spool file was removed: %v", err)
	}
}
