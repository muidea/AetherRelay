//go:build windows

package store

import "golang.org/x/sys/windows"

// diskSpace 的 Windows 实现（GetDiskFreeSpaceEx，返回字节数）。
func diskSpace(path string) (total, free int64, err error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err = windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0, err
	}
	return int64(totalBytes), int64(freeBytesAvailable), nil
}
