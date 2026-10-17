package main

import (
	"net/url"
	"os"
	"os/exec"
	"sync"
)

// The window is a process of its own — this program with --window — so that the menu bar or the tray
// stays when it is closed, and a window that hangs takes nothing else with it. showWindow, which draws
// it, is the system's own: WebKit on the Mac, WebView2 on Windows.

// window is the one window process the menu bar keeps.
var window struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	quitting bool // the app ends it; the user did not close it
}

// openWindow opens the window, unless it is open already.
func openWindow() { openWindowAt("/", false) }

// openWindowAt opens the window on one page; with again, an open window is opened anew there.
func openWindowAt(path string, again bool) {
	window.mu.Lock()
	defer window.mu.Unlock()
	if window.cmd != nil {
		if !again {
			raiseWindow(window.cmd.Process.Pid) // it may be behind other windows, or minimised
			return
		}
		window.cmd.Process.Kill()
		window.cmd.Wait()
		window.cmd = nil
	}
	self, err := os.Executable()
	if err != nil {
		openBrowser()
		return
	}
	// on the home network address, not localhost: a link copied in the window must work on the family's phones
	cmd := exec.Command(self, "--window", address()+path)
	if err := cmd.Start(); err != nil {
		openBrowser()
		return
	}
	window.cmd = cmd
	go func() {
		cmd.Wait()
		window.mu.Lock()
		byUser := window.cmd == cmd && !window.quitting // not replaced by a new one, not ended with the app
		if window.cmd == cmd {
			window.cmd = nil
		}
		window.mu.Unlock()
		if byUser {
			windowClosed()
		}
	}()
}

// closeWindow closes the window with the app.
func closeWindow() {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.quitting = true
	if window.cmd != nil {
		window.cmd.Process.Kill()
	}
}

// openOutside opens, in the browser, a link the window's pages leave the server by — tidoly's site, the
// source, a store: the window has no address bar nor back button, and opens no second window. Only web
// addresses; anything else a page might pass is dropped.
func openOutside(link string) {
	if u, err := url.Parse(link); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		openURL(u.String())
	}
}
