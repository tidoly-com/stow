//go:build !windows

package library

// hide does nothing here: a name that starts with a dot is hidden already.
func hide(string) {}
