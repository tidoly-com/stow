//go:build windows

// stow-install is tidoly stow's installer for Windows: stow-install.exe, the app inside it (stow issue #18). Downloaded with a browser and opened, Windows warns once (SmartScreen: the file is not signed —
// operator, 2026-10-15: customers are told to press "추가 정보 → 실행"); what it unpacks carries no mark
// from the internet, so the app opens without a warning after that.
//
// It does what scripts/stow-windows.ps1 does: asks a running tidoly stow to quit, puts the app in
// %LOCALAPPDATA%\Programs\tidoly-stow (no administrator rights), adds it to the Start menu, opens it.
// It also lists the app in Settings → Apps for this user, removed there by stow-uninstall.exe in the
// app's folder (cmd/stow-uninstall). Photos and settings are not touched.
package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// payload is tidoly-stow-windows.zip, put here by scripts/release-windows.sh before this is built.
//
//go:embed payload.zip
var payload []byte

func main() {
	if err := install(); err != nil {
		say("tidoly stow 를 설치하지 못했습니다.\n\n"+err.Error(), windows.MB_ICONERROR)
		os.Exit(1)
	}
}

func install() error {
	programs := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")
	dir := filepath.Join(programs, "tidoly-stow")
	exe := filepath.Join(dir, "stow.exe")
	if err := os.MkdirAll(programs, 0o755); err != nil {
		return err
	}

	// unpacked beside the old one first, so that a failure leaves the old one as it was
	fresh := dir + ".new"
	os.RemoveAll(fresh)
	if err := unzip(payload, programs, fresh); err != nil {
		os.RemoveAll(fresh)
		return fmt.Errorf("풀지 못했습니다: %w", err)
	}

	if _, err := os.Stat(dir); err == nil {
		// a running copy holds its files: ask it to quit as from its menu, then wait for it to let go
		if _, err := os.Stat(exe); err == nil {
			quiet(exec.Command(exe, "--quit")).Run()
		}
		old := fmt.Sprintf("%s.old-%d", dir, time.Now().Unix())
		var err error
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
			if err = os.Rename(dir, old); err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			os.RemoveAll(fresh)
			return errors.New("떠 있는 tidoly stow 가 꺼지지 않습니다 — 작업 표시줄 오른쪽 트레이 아이콘에서 끝내기를 누른 뒤 다시 실행해 주세요")
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(fresh, dir); err != nil {
		return err
	}

	link := filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`, "tidoly stow.lnk")
	script := fmt.Sprintf(`$s=(New-Object -ComObject WScript.Shell).CreateShortcut('%s'); $s.TargetPath='%s'; $s.WorkingDirectory='%s'; $s.Save()`,
		quoteSingle(link), quoteSingle(exe), quoteSingle(dir))
	quiet(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)).Run() // the app works without it
	listInApps(dir, exe)

	// the app opens its window: the setup the first time, the photos after that
	cmd := exec.Command(exe)
	cmd.Dir = dir
	return cmd.Start()
}

// unzip puts the zip's folder tidoly-stow\ into to; nothing may land outside it.
func unzip(data []byte, base, to string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range r.File {
		rel, ok := strings.CutPrefix(filepath.FromSlash(f.Name), "tidoly-stow"+string(filepath.Separator))
		if !ok || rel == "" || strings.Contains(rel, "..") {
			continue
		}
		path := filepath.Join(to, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		dst, err := os.Create(path)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// listInApps puts tidoly stow in Settings → Apps for this user, with stow-uninstall.exe to remove it.
func listInApps(dir, exe string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\tidoly-stow`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue("DisplayName", "tidoly stow")
	k.SetStringValue("Publisher", "tidoly")
	k.SetStringValue("DisplayIcon", exe)
	k.SetStringValue("InstallLocation", dir)
	k.SetStringValue("UninstallString", `"`+filepath.Join(dir, "stow-uninstall.exe")+`"`)
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
}

func quiet(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

func quoteSingle(s string) string { return strings.ReplaceAll(s, "'", "''") }

func say(text string, icon uint32) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("tidoly stow 설치")
	windows.MessageBox(0, t, c, windows.MB_OK|icon)
}
