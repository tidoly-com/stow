//go:build darwin || linux

// Package disk tells how large the disk holding a folder is and how much of it is free.
package disk

import "syscall"

// Space reports the size of the disk that holds dir and how much of it is free.
func Space(dir string) (total, free int64, ok bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, 0, false
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), true
}
