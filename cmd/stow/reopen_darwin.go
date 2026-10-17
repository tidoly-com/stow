//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
void listenReopen(void);
*/
import "C"

import (
	"os"
	"sync/atomic"
	"syscall"
)

//export goReopen
func goReopen() { go openWindow() }

var quitTo atomic.Pointer[chan<- os.Signal]

//export goQuit
func goQuit() {
	if c := quitTo.Load(); c != nil {
		select {
		case *c <- syscall.SIGTERM: // as SIGTERM: quit as from the menu, the server closing its database first
		default: // already quitting
		}
	}
}

// listenReopen makes opening the app again, while it runs, open its window; and quitting it from outside
// quit it as from its menu.
func listenReopen() { C.listenReopen() }
