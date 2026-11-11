//go:build windows

package container

import "golang.org/x/sys/windows"

// FreeBytes returns the free space for the current user at path.
func FreeBytes(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(free), nil
}
