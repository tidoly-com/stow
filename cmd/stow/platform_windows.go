//go:build windows

package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// What differs on Windows: where things are, how the server is asked to stop (there is no SIGTERM), how
// a second start and the install script reach the running one, how to tell the user.

func settingsDir() string { return filepath.Join(os.Getenv("APPDATA"), "tidoly-stow") }

func logsDir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "tidoly-stow", "logs") }

// binDir is where the server and its helpers are: bin\ beside this program.
func binDir() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(self), "bin")
	if _, err := os.Stat(filepath.Join(dir, "tidoly-light.exe")); err != nil {
		return "", err
	}
	return dir, nil
}

func program(name string) string { return name + ".exe" }

func facesRunHere() bool { return true }

// quietly readies the server's command — no console window — and returns how to ask it to stop: closing
// its standard input, which the server takes as SIGTERM (STOW_STOP_ON_STDIN).
func quietly(cmd *exec.Cmd) (askToStop func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Env = append(os.Environ(), "STOW_STOP_ON_STDIN=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		return func() { cmd.Process.Kill() }
	}
	return func() { in.Close() }
}

// Names the running program and a second start or the install script share, in this user's session.
const (
	instanceName = `Local\tidoly-stow`
	openName     = `Local\tidoly-stow-open`
	quitName     = `Local\tidoly-stow-quit`
	frontName    = `Local\tidoly-stow-front`
)

var instance windows.Handle // held while this program runs

// firstInstance says whether this is the only tidoly stow running. If not, it asks the running one to
// open its window — what opening a running app does on the Mac.
func firstInstance() bool {
	name, _ := windows.UTF16PtrFromString(instanceName)
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		allowSetForegroundWindow.Call(^uintptr(0)) // ASFW_ANY: the user opened this one, the window may come forward
		raise(openName)
		return false
	}
	instance = h
	return true
}

// listenReopen opens the window when another start asks for it.
func listenReopen() { listen(openName, func() { go openWindow() }) }

// listenQuit passes a request to quit — from the install script, by askToQuit — on as SIGTERM would be.
func listenQuit(quitting chan<- os.Signal) { listen(quitName, func() { quitting <- syscall.SIGTERM }) }

// askToQuit asks a running tidoly stow to quit as from its menu.
func askToQuit() { raise(quitName) }

func listen(name string, do func()) {
	n, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateEvent(nil, 0, 0, n)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		log.Printf("%s: %v", name, err)
		return
	}
	go func() {
		for {
			if ev, _ := windows.WaitForSingleObject(h, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
				do()
			}
		}
	}()
}

func raise(name string) {
	n, _ := windows.UTF16PtrFromString(name)
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, n)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	windows.SetEvent(h)
}

// The tray takes an .ico; this is the app's icon at two sizes, as PNG inside the .ico, which Windows reads
// since Vista.
var (
	//go:embed tray-32.png
	tray32 []byte
	//go:embed tray-64.png
	tray64 []byte
)

func trayIcon() []byte {
	images := [][]byte{tray32, tray64}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(images))})
	offset := 6 + 16*len(images)
	for i, img := range images {
		side := byte(32 << i)
		binary.Write(&b, binary.LittleEndian, struct {
			W, H, Colors, Reserved byte
			Planes, Bits           uint16
			Size, Offset           uint32
		}{side, side, 0, 0, 1, 32, uint32(len(img)), uint32(offset)})
		offset += len(img)
	}
	for _, img := range images {
		b.Write(img)
	}
	return b.Bytes()
}

// openBrowser opens the pages on the home network address, so that what is copied from the address bar
// works on another device too.
func openBrowser() { openBrowserAt(address()) }

func openURL(u string) { openBrowserAt(u) }

func alert(text string) {
	log.Print(text)
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("tidoly stow")
	windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONWARNING)
}

// computerName is this PC's name, shown on the other devices.
func computerName() string {
	name, _ := os.Hostname()
	return name
}

// windowClosed tells, the first three times the window is closed, that tidoly stow goes on in the tray —
// closing the window looks like quitting on Windows, and the tray hides new icons. A Windows notification,
// under tidoly stow's own name (registered for this user), so it is not a box to click away.
func windowClosed() {
	mark := filepath.Join(settingsDir(), "closed-notices")
	n := 0
	if b, err := os.ReadFile(mark); err == nil {
		fmt.Sscan(string(b), &n)
	}
	if n >= 3 {
		return
	}
	os.MkdirAll(settingsDir(), 0o755)
	os.WriteFile(mark, []byte(fmt.Sprint(n+1)), 0o644)
	registerNotifier()
	script := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml('<toast><visual><binding template="ToastGeneric"><text>tidoly stow 는 계속 돌고 있습니다</text><text>트레이에서 폰 사진을 받습니다. 끄려면 트레이 아이콘 → 끝내기.</text></binding></visual></toast>')
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + notifierID + `').Show([Windows.UI.Notifications.ToastNotification]::new($x))`
	toast := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(script))
	toast.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	toast.Start()
}

const notifierID = "tidoly.stow"

// registerNotifier lets notifications come under tidoly stow's name and icon — an app without a package
// is known to Windows by this key.
func registerNotifier() {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+notifierID, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue("DisplayName", "tidoly stow")
	icon := filepath.Join(settingsDir(), "icon.png")
	if os.WriteFile(icon, tray64, 0o644) == nil {
		k.SetStringValue("IconUri", icon)
	}
}

// encodePowerShell is a script as -EncodedCommand takes it: UTF-16LE, base64 — Korean survives it.
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// showInTray takes tidoly stow's icon out of the tray's hidden part once, the first time Windows lists it:
// Windows 11 hides new icons under ^, and an icon in sight says the app is running. Hidden again by the
// user, it stays hidden. The key is not documented; where it is not there, nothing happens.
func showInTray() {
	mark := filepath.Join(settingsDir(), "tray-shown")
	if exists(mark) {
		return
	}
	me := strings.ToLower(self())
	for try := 0; try < 20; try++ {
		time.Sleep(3 * time.Second) // Windows writes the entry once the icon has been shown
		k, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\NotifyIconSettings`, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			return
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, name := range names {
			icon, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\NotifyIconSettings\`+name, registry.QUERY_VALUE|registry.SET_VALUE)
			if err != nil {
				continue
			}
			path, _, _ := icon.GetStringValue("ExecutablePath")
			if strings.ToLower(path) == me {
				icon.SetDWordValue("IsPromoted", 1)
				icon.Close()
				os.MkdirAll(settingsDir(), 0o755)
				os.WriteFile(mark, nil, 0o644)
				return
			}
			icon.Close()
		}
	}
}
