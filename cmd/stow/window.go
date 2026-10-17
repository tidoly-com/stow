package main

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"time"
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

// loading is what the window shows until the server answers — on a slow machine that takes a while, and
// a window that does not come makes people start the app again (2026-10-17).
const loading = `<!doctype html><html lang="ko"><head><meta charset="utf-8"><style>
html,body{height:100%;margin:0}
body{display:flex;flex-direction:column;align-items:center;justify-content:center;gap:18px;background:#fff;color:#64748b;
font:15px -apple-system,"Segoe UI","Malgun Gothic",sans-serif}
b{color:#0F766E;font-size:28px;letter-spacing:-.5px}b span{color:#64748b;font-weight:500;font-size:18px;margin-right:6px}
i{width:28px;height:28px;border:3px solid #E6F7F4;border-top-color:#0F766E;border-radius:50%;animation:r .9s linear infinite}
@keyframes r{to{transform:rotate(360deg)}}
@media (prefers-color-scheme:dark){body{background:#0b1220;color:#94a3b8}b{color:#2DD4BF}i{border-color:#123F3B;border-top-color:#2DD4BF}}
</style></head><body><b><span>tidoly</span>stow</b><i></i><p>사진 서버를 여는 중입니다…</p></body></html>`

// answers says whether the tidoly stow at the page's address answers.
func answers(page string) bool {
	u, err := url.Parse(page)
	if err != nil {
		return false
	}
	c := http.Client{Timeout: time.Second}
	res, err := c.Get(u.Scheme + "://" + u.Host + "/api/server/ping")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

// whenAnswers calls open once the server at the page's address answers.
func whenAnswers(page string, open func()) {
	for !answers(page) {
		time.Sleep(300 * time.Millisecond)
	}
	open()
}
