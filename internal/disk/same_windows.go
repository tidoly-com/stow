package disk

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var getVolumePathName = syscall.NewLazyDLL("kernel32.dll").NewProc("GetVolumePathNameW")

// Same says whether two folders are on one disk (volume). A folder not made yet counts as on the disk of
// the nearest folder above it that is there.
func Same(a, b string) (bool, error) {
	va, err := volume(existing(a))
	if err != nil {
		return false, err
	}
	vb, err := volume(existing(b))
	if err != nil {
		return false, err
	}
	return strings.EqualFold(va, vb), nil
}

// volume is the root of the volume a folder is on — C:\, or a folder a disk is mounted at.
func volume(dir string) (string, error) {
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 1024)
	if r, _, err := getVolumePathName.Call(uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return "", err
	}
	return syscall.UTF16ToString(buf), nil
}

// Writable says whether a file can be written in dir — tried, then removed: Windows has drives no photos
// can go on (the system-reserved one, a full one) that say nothing of it otherwise.
func Writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".stow-check-")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
