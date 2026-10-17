package disk

import (
	"regexp"
	"syscall"
)

// Same says whether two folders are on one disk. On the Mac a disk is split into APFS volumes — the
// system, the data, recovery — each a file system of its own but sharing the disk's room, so they are told
// apart by the disk they come from (/dev/disk3s5 and /dev/disk3s1 are both disk3), not by the volume.
// A folder not made yet counts as on the disk of the nearest folder above it that is there.
func Same(a, b string) (bool, error) {
	da, err := wholeDisk(existing(a))
	if err != nil {
		return false, err
	}
	db, err := wholeDisk(existing(b))
	if err != nil {
		return false, err
	}
	return da == db, nil
}

var slice = regexp.MustCompile(`(s[0-9]+)+$`)

func wholeDisk(dir string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}
	from := make([]byte, 0, len(st.Mntfromname))
	for _, c := range st.Mntfromname {
		if c == 0 {
			break
		}
		from = append(from, byte(c))
	}
	return slice.ReplaceAllString(string(from), ""), nil
}

// Writable says whether this program may write in dir — a read-only volume (recovery) is no place for photos.
func Writable(dir string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(dir, &st) == nil && st.Flags&0x1 == 0 && syscall.Access(dir, 2) == nil // MNT_RDONLY
}
