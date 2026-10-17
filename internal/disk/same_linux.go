//go:build linux

package disk

import "syscall"

// Same says whether two folders are on one disk — one file system, by its device number. A folder not
// made yet counts as on the disk of the nearest folder above it that is there.
func Same(a, b string) (bool, error) {
	da, err := device(existing(a))
	if err != nil {
		return false, err
	}
	db, err := device(existing(b))
	if err != nil {
		return false, err
	}
	return da == db, nil
}

func device(dir string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// Writable says whether this program may write in dir — a read-only disk is no place for photos.
func Writable(dir string) bool { return syscall.Access(dir, 2) == nil }
