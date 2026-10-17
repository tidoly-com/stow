// Package update tells whether a newer tidoly stow is out, and installs it where it can.
//
// Every release carries latest.json — {"version": "v0.2.0"} — beside the app, at the address the install
// scripts download from. The server looks once a day; a newer version is shown on the admin page, and
// "업데이트" runs the same install the customer ran: download, checksum, quit the app, replace it, open it.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Checker knows the newest version out.
type Checker struct {
	Current string // this build's version, as git describe --tags gives it: v0.1.0, v0.1.0-3-gabc1234
	Base    string // where releases are downloaded from; latest.json is there

	mu     sync.Mutex
	latest string
	state  string // Running or Failed once an update was started here
}

// What an update started here is doing.
const (
	Running = "running"
	Failed  = "failed"
)

// Run looks now and once a day, until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	for {
		if err := c.Check(ctx); err != nil && ctx.Err() == nil {
			log.Printf("update: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// Check asks for the newest version out.
func (c *Checker) Check(ctx context.Context) error {
	if c.Base == "" || version(c.Current) == nil {
		return nil // a build without a version (from the source, not a release) does not look
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.Base, "/")+"/latest.json", nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New(res.Status)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out); err != nil {
		return err
	}
	if version(out.Version) == nil {
		return errors.New("latest.json has no version")
	}
	c.mu.Lock()
	c.latest = out.Version
	c.mu.Unlock()
	return nil
}

// Newer is the newest version out when it is newer than this build; "" otherwise.
func (c *Checker) Newer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if Newer(c.latest, c.Current) {
		return c.latest
	}
	return ""
}

// State is Running or Failed for an update started here; "" when none was started.
func (c *Checker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Checker) setState(s string) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

// ErrByHand: on this system the update is not run from the app.
var ErrByHand = errors.New("this system updates by hand")

// Install runs the update, on the systems that can; it returns once the update is under way.
func (c *Checker) Install(ctx context.Context) error {
	if c.Newer() == "" {
		return errors.New("no newer version")
	}
	cmd, err := install(ctx, strings.TrimRight(c.Base, "/"))
	if err != nil {
		return err
	}
	c.setState(Running)
	go func() {
		// The install quits this program before it ends; when it ends with this program still here, it did
		// not take.
		err := cmd.Wait()
		log.Printf("update: the install ended without replacing this program: %v", err)
		c.setState(Failed)
	}()
	return nil
}

// Newer says whether one version is newer than another; versions it cannot read are never newer, and a
// build some commits after a release (v0.1.0-3-gabc) counts as that release.
func Newer(latest, current string) bool {
	l, c := version(latest), version(current)
	if l == nil || c == nil {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// version reads vMAJOR.MINOR.PATCH, ignoring what git describe puts after it.
func version(v string) []int {
	v, ok := strings.CutPrefix(v, "v")
	if !ok {
		return nil
	}
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}
