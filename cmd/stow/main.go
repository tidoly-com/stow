// stow is what runs when tidoly stow is opened — the Mac app (stow issue #18) or the Windows program: it
// starts the photo server next to it, keeps it running, opens its pages in a window, and sits in the menu
// bar or the tray — open, open in the browser, quit. The pages are the server's — the setup too: on the
// first start the server shows it, asks how this PC is used and where the photos are kept, and writes the
// settings. A PC that only looks at another's photos runs no server; the window goes to that one.
//
// The Mac app's layout:
//
//	tidoly-stow.app/Contents/MacOS/stow                   this program
//	tidoly-stow.app/Contents/Resources/bin/tidoly-light   the server
//	tidoly-stow.app/Contents/Resources/bin/stow-coupons   the coupon helper
//	tidoly-stow.app/Contents/Resources/bin/ffmpeg         LGPL build, for small pictures of HEIC photos and videos
//	tidoly-stow.app/Contents/Resources/LICENSES/          ffmpeg's license notice and where its source is
//
// On Windows: %LOCALAPPDATA%\Programs\tidoly-stow\stow.exe with bin\tidoly-light.exe beside it.
//
// The settings and the logs are where each system keeps them (settingsDir, logsDir); opening at login is a
// launch agent on the Mac and a Run entry in the registry on Windows.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/tidoly-com/stow/internal/install"
)

// port is stow's port everywhere (decided 2026-10-14).
const port = 30891

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--window" {
		showWindow(os.Args[2])
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--quit" { // the install script, to replace the program
		askToQuit()
		return
	}
	if !firstInstance() { // already running: it was asked to open its window
		return
	}
	home, _ := os.UserHomeDir()
	os.Chdir(home) // Finder starts apps in /
	logs := logsDir()
	os.MkdirAll(logs, 0o755)
	if f, err := os.OpenFile(filepath.Join(logs, "stow.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(f)
	}

	atLogin := len(os.Args) == 2 && os.Args[1] == "--login" // opened by macOS: stay in the menu bar, no window

	cfgPath := filepath.Join(settingsDir(), "config.json")
	bin, err := binDir()
	if err != nil {
		alert("stow 앱이 온전하지 않습니다: " + err.Error())
		return
	}
	srv := &server{bin: bin, config: cfgPath, log: filepath.Join(logs, "server.log")}
	ctx, stop := context.WithCancel(context.Background())
	if cfg, _ := install.Load(cfgPath); cfg.Server != "" {
		// this PC only looks at another PC's photos: no server here
		becomeViewer(cfg.Server, cfgPath)
		if !atLogin {
			go openWindow()
		}
	} else {
		if ours() { // already running — macOS hands the running app a reopen instead, but a server left over is possible
			if !atLogin { // macOS also starts the login agent the moment it is turned on: nothing to show then
				openBrowser()
			}
			return
		}
		if !free() {
			alert(fmt.Sprintf("다른 프로그램이 포트 %d 을 쓰고 있어 tidoly stow 를 띄울 수 없습니다.\n\n그 프로그램을 끈 뒤 다시 열어 주세요.", port))
			return
		}
		serve(ctx, srv, atLogin)
	}
	// Logging out, shutting down and the install script (to replace the app) ask to quit — with SIGTERM on
	// the Mac, by askToQuit on Windows: quit as from the menu, so that the server closes its database first.
	quitting := make(chan os.Signal, 1)
	signal.Notify(quitting, syscall.SIGTERM, syscall.SIGINT)
	listenQuit(quitting)
	go func() { <-quitting; closeWindow(); stop(); srv.wait(); systray.Quit() }() // as from the menu: the system ends the app at Quit
	systray.Run(func() { menu(ctx, srv, stop) }, func() { closeWindow(); stop(); srv.wait() })
}

// serve runs the photo server here — or, before the setup is done, the setup — and opens its window.
func serve(ctx context.Context, srv *server, quiet bool) {
	go srv.keep(ctx)
	if !quiet {
		go openWindow() // at once: it says the server is coming up until it is
	}
	go func() {
		if waitUp(2 * time.Minute) { // a slow machine's first start takes a while
			if viewing() {
				return // the setup made this PC one that only looks
			}
			firstLogin(filepath.Dir(srv.config))
		} else {
			alert("tidoly stow 서버가 뜨지 않았습니다. 기록: " + srv.log)
		}
	}()
}

// viewed is the other PC's tidoly stow this one only looks at; empty while this PC keeps the photos.
var viewed atomic.Value

func viewing() bool { s, _ := viewed.Load().(string); return s != "" }

// becomeViewer makes this PC one that only looks at the photos at address: the window and the browser go
// there, and nothing here needs to open at login.
func becomeViewer(address, settings string) {
	viewed.Store(address)
	removeLoginAgent(filepath.Dir(settings))
	if switchItem != nil {
		switchItem.Show()
	}
	log.Printf("this PC looks at %s; no server here", address)
}

// ── the server ──────────────────────────────────────────────────────────────────────────────────────────

type server struct {
	bin, config, log string

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// keep runs the server and starts it again when it ends by itself — as it does once the setup has its
// folder — waiting longer each time it ends soon after starting. When the setup made this PC one that only
// looks at another's photos, it does not start it again.
func (s *server) keep(ctx context.Context) {
	pause := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		if err := s.run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("server ended: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		if cfg, _ := install.Load(s.config); cfg.Server != "" {
			becomeViewer(cfg.Server, s.config)
			return
		}
		if time.Since(started) > time.Minute {
			pause = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, time.Minute)
	}
}

// ownerReset asks the server program for a link that sets the owner's password again (-reset-owner).
func (s *server) ownerReset() (string, error) {
	out, err := exec.Command(filepath.Join(s.bin, program("tidoly-light")), "-config", s.config,
		"-listen", fmt.Sprintf("0.0.0.0:%d", port), "-reset-owner").Output()
	if err != nil {
		return "", err
	}
	link := strings.TrimSpace(string(out))
	if !strings.HasPrefix(link, "http://127.0.0.1:") {
		return "", fmt.Errorf("not a link: %q", link)
	}
	return link, nil
}

func (s *server) run(ctx context.Context) error {
	out, err := os.OpenFile(s.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	args := []string{"-config", s.config, "-listen", fmt.Sprintf("0.0.0.0:%d", port), "-public-url", address(), "-name", computerName()}
	if helper := filepath.Join(s.bin, program("stow-coupons")); exists(helper) {
		args = append(args, "-coupon-helper", helper)
	}
	if helper := filepath.Join(s.bin, program("stow-faces")); exists(helper) && facesRunHere() {
		args = append(args, "-face-helper", helper)
	}
	cmd := exec.Command(filepath.Join(s.bin, program("tidoly-light")), args...)
	cmd.Stdout, cmd.Stderr = out, out
	askToStop := quietly(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.cmd, s.done = cmd, done
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		askToStop() // the server closes its database and stops its jobs
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
		}
	}()
	err = cmd.Wait()
	close(done)
	return err
}

// wait returns once the server has ended, after quitting.
func (s *server) wait() {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(12 * time.Second):
		}
	}
}

// ── the port ────────────────────────────────────────────────────────────────────────────────────────────

func local() string { return fmt.Sprintf("http://localhost:%d", port) }

// ours says whether a stow server answers on the port already.
func ours() bool {
	c := http.Client{Timeout: time.Second}
	res, err := c.Get(local() + "/api/server/ping")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

func free() bool {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func waitUp(limit time.Duration) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
		if ours() {
			return true
		}
	}
	return false
}

// address is how a phone in the house reaches this Mac: its address on the home network.
func address() string {
	if s, _ := viewed.Load().(string); s != "" {
		return s
	}
	conn, err := net.Dial("udp", "192.0.2.1:9") // nothing is sent: this only asks which address would be used
	if err != nil {
		return local()
	}
	defer conn.Close()
	return fmt.Sprintf("http://%s:%d", conn.LocalAddr().(*net.UDPAddr).IP, port)
}

// ── the menu bar ────────────────────────────────────────────────────────────────────────────────────────

// switchItem is the menu's way back to the setup, shown while this PC only looks at another's photos.
var switchItem *systray.MenuItem

func menu(ctx context.Context, srv *server, stop func()) {
	listenReopen()
	go showInTray()
	systray.SetTemplateIcon(menubarIcon, trayIcon())
	systray.SetTooltip("tidoly stow")
	open := systray.AddMenuItem("열기", "tidoly stow 창을 엽니다")
	browser := systray.AddMenuItem("브라우저에서 열기", "기본 브라우저에서 엽니다")
	switchItem = systray.AddMenuItem("연결할 PC 바꾸기", "사진을 볼 PC 를 다시 고르거나, 이 PC 에 사진을 보관하도록 바꿉니다")
	if !viewing() {
		switchItem.Hide()
	}
	reset := systray.AddMenuItem("관리자 비밀번호 변경", "관리자 비밀번호를 잊었을 때: 이 PC 의 브라우저에서만 10분 안에 한 번 열리는 링크를 엽니다")
	if viewing() {
		reset.Hide() // the photos, and their owner, are on the other PC
	}
	systray.AddSeparator()
	quit := systray.AddMenuItem("끝내기", "tidoly stow 를 끕니다 — 폰의 백업도 멈춥니다")
	go func() {
		for {
			select {
			case <-open.ClickedCh:
				openWindow()
			case <-browser.ClickedCh:
				openBrowser()
			case <-switchItem.ClickedCh:
				if !free() {
					alert(fmt.Sprintf("다른 프로그램이 포트 %d 을 쓰고 있어 설치 화면을 열 수 없습니다.", port))
					continue
				}
				if err := (install.Config{}).Save(srv.config); err != nil {
					log.Printf("settings: %v", err)
					continue
				}
				viewed.Store("")
				switchItem.Hide()
				closeWindow()
				serve(ctx, srv, false) // the setup, from its first question
			case <-reset.ClickedCh:
				link, err := srv.ownerReset()
				if err != nil {
					log.Printf("owner reset: %v", err)
					alert("링크를 만들지 못했습니다. 사진 서버가 이 PC 에서 돌고 있는지 확인해 주세요.")
					continue
				}
				openURL(link)
			case <-quit.ClickedCh:
				closeWindow()
				stop()
				systray.Quit()
				return
			}
		}
	}()
}

// firstLogin makes the app open at login from its first start on; who does not want that turns it off in
// System Settings → General → Login Items, and it is left so. When the app was moved, the agent is pointed
// at where it is now.
func firstLogin(dir string) {
	mark := filepath.Join(dir, "login-item")
	if exists(mark) && (loginOn() || !loginRegistered()) {
		return
	}
	if err := writeLoginAgent(); err != nil {
		log.Printf("login item: %v", err)
		return
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(mark, []byte("opening at login was turned on at the first start\n"), 0o644)
}

// menubarIcon is the app's S-and-box in one colour, which macOS tints for the menu bar (18 points at 2x).
// It is made from assets/icon/menubar.svg by scripts/build-icons.sh.
//
//go:embed menubar.png
var menubarIcon []byte

// ── small things ────────────────────────────────────────────────────────────────────────────────────────

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
