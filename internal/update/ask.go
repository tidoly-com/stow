package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Linux update service's files, under its folder: the request this program leaves, and what the service
// last did — running, done or failed — which it writes as it goes.
const (
	askFile   = "ask/update"
	stateFile = "state"
)

// How often and how long the wait for the update service looks at its state.
var (
	askPoll    = 2 * time.Second
	askTimeout = 30 * time.Minute
)

// askable says whether this program may leave a request in dir's ask/: the update service is there, and
// writable by the account this program runs as.
func askable(dir string) bool {
	f, err := os.CreateTemp(filepath.Join(dir, filepath.Dir(askFile)), ".probe-")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// ask leaves the request for the update service, and gives what waits for it to fail. The service does not
// read the request: where it downloads from was set when it was installed, so the account the photo server
// runs as cannot point root at anything else.
func ask(dir string) (func() error, error) {
	start := time.Now().Add(-time.Second) // the state's time may be rounded down
	if err := os.WriteFile(filepath.Join(dir, askFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return func() error {
		for deadline := time.Now().Add(askTimeout); time.Now().Before(deadline); time.Sleep(askPoll) {
			info, err := os.Stat(filepath.Join(dir, stateFile))
			if err != nil || info.ModTime().Before(start) {
				continue // not started yet, or what it says is from an update before
			}
			b, _ := os.ReadFile(filepath.Join(dir, stateFile))
			switch strings.TrimSpace(string(b)) {
			case "failed":
				return errors.New("the update service failed — journalctl -u tidoly-stow-update")
			case "done":
				return errors.New("the update service ended without replacing this program")
			}
		}
		return errors.New("the update service did not end in time")
	}, nil
}
