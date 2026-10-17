//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// Opening tidoly stow when the user logs in is an entry in this user's Run key, which starts this program
// with --login. Turning it off in Settings → Apps → Startup marks it elsewhere and leaves the entry, so a
// present entry is not taken as "on" by itself — see firstLogin.

const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "tidoly stow"
)

func loginCommand() string { return `"` + self() + `" --login` }

func readRun() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return v, err == nil
}

// loginRegistered says whether there is any entry at all.
func loginRegistered() bool { _, ok := readRun(); return ok }

// loginOn says whether the app opens at login, from this place.
func loginOn() bool { v, ok := readRun(); return ok && v == loginCommand() }

// writeLoginAgent makes the app open at login, from where it is now.
func writeLoginAgent() error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(runValue, loginCommand())
}

// removeLoginAgent stops the app opening at login — for a PC that only looks at another's photos. The
// mark of the first start goes too, so that keeping photos here again turns it back on.
func removeLoginAgent(dir string) {
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE); err == nil {
		k.DeleteValue(runValue)
		k.Close()
	}
	os.Remove(filepath.Join(dir, "login-item"))
}

func self() string {
	p, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func execStart(name string, args ...string) { exec.Command(name, args...).Start() }
