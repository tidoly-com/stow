package disk

import (
	"os"
	"path/filepath"
)

// existing is dir, or the nearest folder above it that is there.
func existing(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
