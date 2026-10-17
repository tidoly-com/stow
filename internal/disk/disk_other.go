//go:build !darwin && !linux && !windows

package disk

func Space(string) (total, free int64, ok bool) { return 0, 0, false }
