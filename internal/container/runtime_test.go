package container

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistingDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		filepath.Join(root, "a"):                root + string(filepath.Separator) + "a",
		filepath.Join(root, "a", "b", "traces"): filepath.Join(root, "a"),
		filepath.Join(root, "missing"):          root,
	} {
		if got := existingDir(in); got != want {
			t.Errorf("existingDir(%q) = %q, want %q", in, got, want)
		}
	}
	if free, err := FreeBytes(existingDir(filepath.Join(root, "x", "y"))); err != nil || free <= 0 {
		t.Errorf("FreeBytes: %d, %v", free, err)
	}
}
