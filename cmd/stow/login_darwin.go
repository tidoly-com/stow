//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Opening tidoly stow when the user logs in is a launch agent in ~/Library/LaunchAgents that starts this
// program with --login. Not the app-bundled agent macOS offers (SMAppService): for an app not signed by a
// developer, macOS ties that one to the program file as it was, and after an update refuses to start it.

const loginLabel = "com.tidoly.stow.login"

func loginPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", loginLabel+".plist")
}

// loginRegistered says whether there is any agent at all — one the user may have turned off.
func loginRegistered() bool { return exists(loginPlist()) }

// loginOn says whether the app opens at login, from this place.
func loginOn() bool {
	b, err := os.ReadFile(loginPlist())
	return err == nil && strings.Contains(string(b), "<string>"+xmlText(self())+"</string>")
}

// writeLoginAgent makes the app open at login, from where it is now. It takes effect at the next login.
func writeLoginAgent() error {
	if err := os.MkdirAll(filepath.Dir(loginPlist()), 0o755); err != nil {
		return err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>                  <string>` + loginLabel + `</string>
  <key>ProgramArguments</key>       <array><string>` + xmlText(self()) + `</string><string>--login</string></array>
  <key>LimitLoadToSessionType</key> <string>Aqua</string>
  <key>ProcessType</key>            <string>Interactive</string>
  <key>RunAtLoad</key>              <true/>
</dict>
</plist>
`
	return os.WriteFile(loginPlist(), []byte(plist), 0o644)
}

// removeLoginAgent stops the app opening at login — for a PC that only looks at another's photos. The
// mark of the first start goes too, so that keeping photos here again turns it back on.
func removeLoginAgent(dir string) {
	exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), loginLabel)).Run()
	os.Remove(loginPlist())
	os.Remove(filepath.Join(dir, "login-item"))
}

// computerName is this Mac's name as the user sees it ("○○의 MacBook Pro"), shown on the other devices.
func computerName() string {
	out, err := exec.Command("scutil", "--get", "ComputerName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func self() string {
	p, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
