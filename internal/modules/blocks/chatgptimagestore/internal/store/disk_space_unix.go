//go:build !windows

package store

import "syscall"

// diskSpace 的 Unix 实现（statfs）。Windows 见 disk_space_windows.go。
func diskSpace(path string) (total, free int64, err error) {
	var filesystem syscall.Statfs_t
	if err = syscall.Statfs(path, &filesystem); err != nil {
		return 0, 0, err
	}
	return int64(filesystem.Blocks) * int64(filesystem.Bsize), int64(filesystem.Bavail) * int64(filesystem.Bsize), nil
}
