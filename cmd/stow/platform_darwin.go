//go:build darwin

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// What differs on the Mac: where things are, how the server is asked to stop, how to tell the user.

func settingsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "tidoly-stow")
}

func logsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "tidoly-stow")
}

// binDir is where the server and its helpers are: inside the app, Contents/Resources/bin.
func binDir() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(self), "..", "Resources", "bin")
	if _, err := os.Stat(filepath.Join(dir, "tidoly-light")); err != nil {
		return "", err
	}
	return dir, nil
}

func program(name string) string { return name }

// facesRunHere: the face helper and its ONNX Runtime are built for Apple Silicon only — none is published
// for Intel Macs. This program is universal; on an Intel Mac it runs as x86-64.
func facesRunHere() bool { return runtime.GOARCH == "arm64" }

// quietly readies the server's command and returns how to ask it to stop: SIGTERM. Apps started from
// Finder have a short PATH; the server needs only the system's tools.
func quietly(cmd *exec.Cmd) (askToStop func()) {
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	return func() { cmd.Process.Signal(syscall.SIGTERM) }
}

// firstInstance: macOS itself hands a running app a reopen when it is opened again (reopen_darwin.m).
func firstInstance() bool { return true }

// listenQuit: on the Mac the install script sends SIGTERM, which main already listens for; a quit from
// outside the app — Activity Monitor, AppleScript, logging out — comes as an Apple event (reopen_darwin.m).
func listenQuit(quitting chan<- os.Signal) { quitTo.Store(&quitting) }

// askToQuit asks a running tidoly stow to quit as from its menu.
func askToQuit() {
	exec.Command("pkill", "-TERM", "-f", "tidoly-stow.app/Contents/MacOS/stow$").Run()
}

func trayIcon() []byte { return menubarIcon } // the menu bar takes the one-colour icon only

// openBrowser opens the pages on the home network address, so that what is copied from the address bar
// works on another device too.
func openBrowser() { openURL(address()) }

func openURL(u string) { exec.Command("open", u).Run() }

func alert(text string) {
	log.Print(text)
	exec.Command("osascript", "-e", `display alert "tidoly stow" message `+quote(text)+` as critical`).Run()
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// windowClosed: on the Mac the menu bar icon is in sight; nothing to say.
func windowClosed() {}

// showInTray: the menu bar shows every icon.
func showInTray() {}
