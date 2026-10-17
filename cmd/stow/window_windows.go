//go:build windows

package main

import (
	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

var (
	user32                   = windows.NewLazySystemDLL("user32.dll")
	setForegroundWindow      = user32.NewProc("SetForegroundWindow")
	allowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	showWindow32             = user32.NewProc("ShowWindow")
	isIconic                 = user32.NewProc("IsIconic")
)

// raiseWindow brings the open window to the front. Windows lets only the process the user just acted on
// put a window in front; that one — the tray, or a second start — allows the window's process first.
func raiseWindow(pid int) {
	allowSetForegroundWindow.Call(uintptr(pid))
	raise(frontName)
}

// showWindow is the window process: the server's pages in a window of their own, until it is closed. It is
// drawn by WebView2, which Windows 10 and 11 carry.
func showWindow(url string) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus:     true,
		WindowOptions: webview2.WindowOptions{Title: "tidoly stow", Width: 1200, Height: 860, Center: true},
	})
	if w == nil { // no WebView2 here: the browser does
		openBrowserAt(url)
		return
	}
	defer w.Destroy()
	listen(frontName, func() { // the tray or a second start asks: come to the front, restored
		w.Dispatch(func() {
			hwnd := uintptr(w.Window())
			if r, _, _ := isIconic.Call(hwnd); r != 0 {
				showWindow32.Call(hwnd, 9) // SW_RESTORE
			}
			setForegroundWindow.Call(hwnd)
		})
	})
	w.Bind("stowOpen", openOutside) // links that leave the server: the browser, not this window
	if answers(url) {
		w.Navigate(url)
	} else { // the server is still coming up: say so, then go
		w.SetHtml(loading)
		go whenAnswers(url, func() { w.Dispatch(func() { w.Navigate(url) }) })
	}
	w.Run()
}

func openBrowserAt(url string) { execStart("rundll32", "url.dll,FileProtocolHandler", url) }
