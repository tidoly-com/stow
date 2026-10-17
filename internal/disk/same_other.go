//go:build !darwin && !linux && !windows

package disk

// Same cannot tell here: it says no, which lets a place be added.
func Same(a, b string) (bool, error) { return false, nil }

func Writable(dir string) bool { return true }
