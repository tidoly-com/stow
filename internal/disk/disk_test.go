package disk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSame(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if same, err := Same(a, b); err != nil || !same {
		t.Errorf("two folders in the temporary folder: same %v, %v", same, err)
	}
	if same, err := Same(a, filepath.Join(b, "not", "made", "yet")); err != nil || !same {
		t.Errorf("a folder not made yet counts as on the disk above it: same %v, %v", same, err)
	}
	if _, err := os.Stat("/dev"); err == nil {
		if same, _ := Same(a, "/dev"); same {
			t.Error("/dev is its own file system, not the temporary folder's disk")
		}
	}
}
