package disk

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// Space reports the size of the disk that holds dir and how much of it is free.
func Space(dir string) (total, free int64, ok bool) {
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, false
	}
	var avail, size, all uint64
	r, _, _ := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&all)))
	if r == 0 {
		return 0, 0, false
	}
	return int64(size), int64(avail), true
}
