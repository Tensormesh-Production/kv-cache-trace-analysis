//go:build !windows

package container

import "golang.org/x/sys/unix"

// FreeBytes returns the free space for unprivileged users at path.
func FreeBytes(path string) (int64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, err
	}
	return int64(s.Bavail) * int64(s.Bsize), nil
}
