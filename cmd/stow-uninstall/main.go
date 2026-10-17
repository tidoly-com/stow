//go:build windows

// stow-uninstall removes tidoly stow from this Windows PC (stow issue #18): it lies in the app's folder and
// is what Settings → Apps runs (stow-install lists it there). The photos and the settings stay — installed
// again, tidoly stow goes on with them.
//
// It cannot remove the folder it runs from, so it first copies itself to the temporary folder, runs from
// there, and has that copy removed once it has ended.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func main() {
	self, _ := os.Executable()
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "tidoly-stow")
	if len(os.Args) == 1 { // as Settings → Apps or a double click starts it
		if !ask("tidoly stow 를 이 PC 에서 지울까요?\n\n사진과 설정은 지우지 않습니다 — 다시 설치하면 그대로 이어집니다.") {
			return
		}
		// run from a copy, so that the app's folder can go
		temp := filepath.Join(os.TempDir(), fmt.Sprintf("stow-uninstall-%d.exe", time.Now().UnixNano()))
		if err := copyFile(self, temp); err == nil {
			if exec.Command(temp, "--go").Start() == nil {
				return
			}
		}
	}
	if err := uninstall(dir); err != nil {
		say("tidoly stow 를 다 지우지 못했습니다.\n\n"+err.Error(), windows.MB_ICONERROR)
	} else {
		note := "tidoly stow 를 지웠습니다."
		if data := photos(); data != "" {
			note += "\n\n사진은 " + data + " 에 그대로 있습니다."
		}
		say(note, windows.MB_ICONINFORMATION)
	}
	if strings.HasPrefix(self, os.TempDir()) { // this copy, once it has ended
		// the command line as cmd reads it: Go's quoting of arguments is not what cmd understands
		del := quiet(exec.Command("cmd"))
		del.SysProcAttr.CmdLine = `cmd /c ping -n 3 127.0.0.1 >nul & del /f /q "` + self + `"`
		del.Start()
	}
}

func uninstall(dir string) error {
	exe := filepath.Join(dir, "stow.exe")
	if _, err := os.Stat(exe); err == nil {
		quiet(exec.Command(exe, "--quit")).Run() // as from its menu: the server closes its database first
	}
	// the app lets go of its files once it has quit
	gone := fmt.Sprintf("%s.removing-%d", dir, time.Now().Unix())
	var err error
	if _, statErr := os.Stat(dir); statErr == nil {
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
			if err = os.Rename(dir, gone); err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("떠 있는 tidoly stow 가 꺼지지 않습니다 — 트레이 아이콘에서 끝내기를 누른 뒤 다시 지워 주세요")
		}
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err == nil {
		k.DeleteValue("tidoly stow") // opening at login
		k.Close()
	}
	os.Remove(filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`, "tidoly stow.lnk"))
	registry.DeleteKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\tidoly-stow`)
	os.Remove(filepath.Join(os.Getenv("APPDATA"), "tidoly-stow", "login-item")) // so that a new install opens at login again
	if err := os.RemoveAll(gone); err != nil {
		return fmt.Errorf("프로그램 폴더를 지우지 못했습니다: %s", gone)
	}
	return nil
}

// photos is the folder the photos are kept in, from the settings — to say where they are.
func photos() string {
	b, err := os.ReadFile(filepath.Join(os.Getenv("APPDATA"), "tidoly-stow", "config.json"))
	if err != nil {
		return ""
	}
	var c struct{ Data string }
	json.Unmarshal(b, &c)
	return c.Data
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o755)
}

func quiet(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

func ask(text string) bool {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("tidoly stow 지우기")
	r, _ := windows.MessageBox(0, t, c, windows.MB_YESNO|windows.MB_ICONQUESTION)
	return r == 6 // IDYES
}

func say(text string, icon uint32) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("tidoly stow 지우기")
	windows.MessageBox(0, t, c, windows.MB_OK|icon)
}
