package container

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestUserArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no user ids on Windows")
	}
	user := fmt.Sprintf("--user %d:%d", os.Getuid(), os.Getgid())
	if got := strings.Join((&Runtime{}).userArgs(), " "); got != user {
		t.Errorf("docker: %q", got)
	}
	if got := strings.Join((&Runtime{Rootless: true}).userArgs(), " "); got != "--userns=keep-id "+user {
		t.Errorf("rootless podman: %q", got)
	}
}
