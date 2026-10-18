package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/install"
	"github.com/tidoly-com/stow/internal/nearby"
)

// The setup: the suggested folder is offered, choosing one makes it, nothing to fetch goes straight on,
// and finishing writes the settings and lets the program end.
func TestInstallWritesTheChosenFolder(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "settings", "config.json")
	done := make(chan struct{})
	in := &Install{Config: config, Suggest: filepath.Join(dir, "Pictures", "stow"), Done: func() { close(done) }}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, string) {
		res, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, _ := get("/"); code != 303 {
		t.Errorf("the photos page during the setup: %d, want a redirect to the setup", code)
	}
	if _, page := get("/install"); !strings.Contains(page, `href="/install/folder"`) || !strings.Contains(page, `href="/install/connect"`) {
		t.Fatalf("the setup must first ask how this PC is used: %s", page)
	}
	if _, page := get("/install/folder"); !strings.Contains(page, in.Suggest) || !strings.Contains(page, "저장 위치") {
		t.Fatalf("setup page: %s", page)
	}
	earlier := filepath.Join(dir, "old")
	os.MkdirAll(filepath.Join(earlier, ".stow"), 0o755)
	if _, page := get("/install/folder?at=" + url.QueryEscape(earlier)); !strings.Contains(page, "전에 쓰던 tidoly stow 폴더") {
		t.Errorf("an earlier install is not told apart: %s", page)
	}
	res, err := client.PostForm(srv.URL+"/install/folder", url.Values{"path": {in.Suggest}})
	if err != nil || res.StatusCode != 303 || res.Header.Get("Location") != "/install/prepare" {
		t.Fatalf("choose the folder: %v %v", res.Status, err)
	}
	if _, err := os.Stat(in.Suggest); err != nil {
		t.Fatalf("the chosen folder was not made: %v", err)
	}
	if _, report := get("/install/progress"); !strings.Contains(report, `"finished":true`) {
		time.Sleep(100 * time.Millisecond)
		if _, report = get("/install/progress"); !strings.Contains(report, `"finished":true`) {
			t.Errorf("nothing to fetch, yet not finished: %s", report)
		}
	}
	if res, err := client.PostForm(srv.URL+"/install/finish", nil); err != nil || res.StatusCode != 200 {
		t.Fatalf("finish: %v %v", res.Status, err)
	}
	if c, err := install.Load(config); err != nil || c.Data != in.Suggest {
		t.Fatalf("settings: %+v %v", c, err)
	}
	select { // the page still loads what it needs from this server
	case <-done:
		t.Fatal("the setup ended before its last page said it was loaded")
	case <-time.After(500 * time.Millisecond):
	}
	if res, err := client.Post(srv.URL+"/install/started", "", nil); err != nil || res.StatusCode != 204 {
		t.Fatalf("started: %v %v", res, err)
	}
	client.Post(srv.URL+"/install/started", "", nil) // twice: the setup ends once
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the setup did not say it is done")
	}
}

// The setup has no account to log in with yet and walks this machine's folders: from outside — a request
// sent to the domain, as through the tunnel — it is refused, all but the ping the Mac app waits on.
func TestInstallOnlyAtHome(t *testing.T) {
	dir := t.TempDir()
	in := &Install{Config: filepath.Join(dir, "config.json"), Suggest: filepath.Join(dir, "stow"), Done: func() {}}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	ask := func(host, method, path string, body url.Values) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body.Encode()))
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	away, home := "demo.stow.tidoly.app", strings.TrimPrefix(srv.URL, "http://")
	for _, path := range []string{"/", "/install", "/install/folder?at=" + url.QueryEscape(dir), "/install/prepare", "/install/connect"} {
		if code := ask(away, "GET", path, nil); code != http.StatusForbidden {
			t.Errorf("GET %s from outside: %d", path, code)
		}
	}
	if code := ask(away, "POST", "/install/folder", url.Values{"path": {filepath.Join(dir, "taken")}}); code != http.StatusForbidden {
		t.Errorf("choosing the folder from outside: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "taken")); err == nil {
		t.Error("a folder was made from outside")
	}
	if code := ask(away, "GET", "/api/server/ping", nil); code != 200 {
		t.Errorf("the ping from outside: %d", code)
	}
	if code := ask(home, "GET", "/install", nil); code != 200 {
		t.Errorf("the setup at home: %d", code)
	}
}

// Only looking at another PC's photos: the servers found in the house are listed; the one chosen must answer
// as tidoly stow — an Immich server answers the ping too — and the settings then name it, not a folder.
func TestInstallToLookAtAnotherPC(t *testing.T) {
	stow := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stow/about" {
			rw.Write([]byte(`{"app":"tidoly stow","name":"거실 맥"}`))
			return
		}
		http.NotFound(rw, r)
	}))
	defer stow.Close()
	immich := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(`{"res":"pong"}`))
	}))
	defer immich.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	done := make(chan struct{})
	in := &Install{Config: config, Suggest: filepath.Join(dir, "stow"), Done: func() { close(done) },
		Find: func(context.Context) ([]nearby.Server, error) {
			return []nearby.Server{{Name: "거실 맥", Address: stow.URL}}, nil
		}}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	res, err := client.Get(srv.URL + "/install/connect")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), "거실 맥") || !strings.Contains(string(page), stow.URL) {
		t.Fatalf("the servers found are not listed: %s", page)
	}
	for _, wrong := range []string{immich.URL, "127.0.0.1:1"} {
		res, err := client.PostForm(srv.URL+"/install/connect", url.Values{"address": {wrong}})
		if err != nil || res.StatusCode != 303 || !strings.Contains(res.Header.Get("Location"), "problem=") {
			t.Errorf("%s is not tidoly stow, yet: %v %v", wrong, res.Status, err)
		}
	}
	if _, err := os.Stat(config); err == nil {
		t.Fatal("settings were written for a server that is not tidoly stow")
	}
	res, err = client.PostForm(srv.URL+"/install/connect", url.Values{"address": {strings.TrimPrefix(stow.URL, "http://")}})
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("connect: %v %v", res.Status, err)
	}
	page, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), stow.URL) {
		t.Errorf("the page must go over to the server: %s", page)
	}
	if c, err := install.Load(config); err != nil || c.Server != stow.URL || c.Data != "" {
		t.Fatalf("settings: %+v %v", c, err)
	}
	client.Post(srv.URL+"/install/started", "", nil) // the page, loaded
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the setup did not say it is done")
	}
}

func TestServerAddress(t *testing.T) {
	for typed, want := range map[string]string{
		"192.168.0.10":                 "http://192.168.0.10:30891",
		" 192.168.0.10:30891 ":         "http://192.168.0.10:30891",
		"http://192.168.0.10:30891/":   "http://192.168.0.10:30891",
		"macbook.local":                "http://macbook.local:30891",
		"https://demo.stow.tidoly.app": "https://demo.stow.tidoly.app",
	} {
		if got := serverAddress(typed); got != want {
			t.Errorf("serverAddress(%q) = %q, want %q", typed, got, want)
		}
	}
}

// A new folder can be made inside the one chosen; its name is a plain name, never a way out of it.
func TestInstallMakesANewFolder(t *testing.T) {
	dir := t.TempDir()
	in := &Install{Config: filepath.Join(dir, "config.json"), Suggest: filepath.Join(dir, "stow"), Done: func() {}}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if _, page := func() (int, string) {
		res, err := client.Get(srv.URL + "/install/folder?at=" + url.QueryEscape(dir))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}(); !strings.Contains(page, `name="name"`) {
		t.Fatalf("no field for a new folder: %s", page)
	}
	for _, bad := range []string{"..", "a/b", `a\b`, "c:"} {
		res, err := client.PostForm(srv.URL+"/install/folder", url.Values{"path": {dir}, "name": {bad}})
		if err != nil || res.StatusCode != 303 || !strings.Contains(res.Header.Get("Location"), "problem=") {
			t.Errorf("new folder %q: %v %v", bad, res.Status, err)
		}
	}
	res, err := client.PostForm(srv.URL+"/install/folder", url.Values{"path": {dir}, "name": {"사진 보관"}})
	if err != nil || res.Header.Get("Location") != "/install/prepare" {
		t.Fatalf("new folder: %v %v", res.Status, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "사진 보관")); err != nil || !info.IsDir() {
		t.Fatalf("the new folder was not made: %v", err)
	}
}

// On Linux every disk mounted outside the root's own is offered, wherever it is mounted — a second disk at
// /data was missing (2026-10-18) — once each, and not the system's own mounts.
func TestLinuxDisksAreFoundWhereverMounted(t *testing.T) {
	mounts := `/dev/nvme0n1p2 / ext4 rw,relatime 0 0
proc /proc proc rw 0 0
/dev/nvme0n1p1 /boot/efi vfat rw 0 0
/dev/nvme0n1p2 /var/lib/docker ext4 rw 0 0
/dev/sda1 /data ext4 rw,relatime 0 0
/dev/sda1 /data/stow/bind ext4 rw 0 0
/dev/loop3 /snap/core/1 squashfs ro 0 0
tmpfs /run tmpfs rw 0 0
/dev/sdb1 /run/media/me/My\040Disk exfat rw 0 0
/dev/sdc1 /mnt/backup ntfs3 rw 0 0
//nas/photos /mnt/nas cifs rw 0 0
`
	got := strings.Join(diskMounts(mounts), " | ")
	if want := "/data | /run/media/me/My Disk | /mnt/backup"; got != want {
		t.Errorf("disks: %q, want %q", got, want)
	}
}
