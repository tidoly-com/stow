package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The Linux update: this program may ask only where the install made ask/ for it; it leaves the request and
// learns from the state the root service writes that the update failed — not from a state left by an
// update before.
func TestAskTheUpdateService(t *testing.T) {
	askPoll, askTimeout = 10*time.Millisecond, 2*time.Second
	dir := t.TempDir()
	if askable(dir) {
		t.Fatal("asked where no update service is")
	}
	os.Mkdir(filepath.Join(dir, "ask"), 0o755)
	if !askable(dir) {
		t.Fatal("could not ask where the update service is")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "ask")); len(left) != 0 {
		t.Fatalf("the look left %d files behind", len(left))
	}

	state := filepath.Join(dir, stateFile)
	os.WriteFile(state, []byte("failed\n"), 0o644) // from an update before
	old := time.Now().Add(-time.Hour)
	os.Chtimes(state, old, old)
	wait, err := ask(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, askFile)); err != nil {
		t.Fatalf("no request: %v", err)
	}
	ended := make(chan error, 1)
	go func() { ended <- wait() }()
	select {
	case err := <-ended:
		t.Fatalf("an old state ended the wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	os.WriteFile(state, []byte("running\n"), 0o644)
	select {
	case err := <-ended:
		t.Fatalf("running ended the wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	os.WriteFile(state, []byte("failed\n"), 0o644)
	select {
	case err := <-ended:
		if err == nil {
			t.Error("a failed update was not told")
		}
	case <-time.After(time.Second):
		t.Fatal("a failed update was not seen")
	}
}
