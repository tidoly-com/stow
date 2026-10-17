//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
void windowMenu(void);
void fileDialogs(void *window);
void raiseApp(int pid);
*/
import "C"

import webview "github.com/webview/webview_go"

// raiseWindow brings the open window to the front.
func raiseWindow(pid int) { C.raiseApp(C.int(pid)) }

// showWindow is the window process: the server's pages in a window of their own, until it is closed.
func showWindow(url string) {
	C.windowMenu()
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle("tidoly stow")
	w.SetSize(1200, 860, webview.HintNone)
	C.fileDialogs(w.Window())
	w.Bind("stowOpen", openOutside) // links that leave the server: the browser, not this window
	w.Navigate(url)
	w.Run()
}
