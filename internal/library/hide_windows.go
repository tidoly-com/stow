package library

import "syscall"

// hide marks a folder as hidden; a leading dot alone does not hide anything on Windows.
func hide(dir string) {
	if path, err := syscall.UTF16PtrFromString(dir); err == nil {
		syscall.SetFileAttributes(path, syscall.FILE_ATTRIBUTE_HIDDEN)
	}
}
