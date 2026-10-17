package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tidoly-com/stow/internal/install"
	"github.com/tidoly-com/stow/internal/lan"
	"github.com/tidoly-com/stow/internal/nearby"
)

// Install is what the server shows before it knows where the photos are kept (stow issue #18): the first
// steps of the setup in the browser, on the server's own port. First the way this PC is used: keeping the
// photos, or only looking at another PC's. Keeping them, it asks where, then fetches what is still missing;
// once the folder is chosen it writes the settings and calls Done; the program then ends and whatever keeps
// it running (the Mac app, systemd) starts it again, now as the photo server, which goes on with the first
// account and the phone. Only looking, it lists the servers found in the house, writes the one chosen and
// calls Done; the Mac app then runs no server and opens that one.
type Install struct {
	Config  string                                         // the settings file
	Suggest string                                         // the folder offered first
	Parts   []install.Part                                 // what is fetched before starting
	Done    func()                                         // the settings are written
	Find    func(context.Context) ([]nearby.Server, error) // the servers in the house; nil: look on the network

	mu       sync.Mutex
	ended    sync.Once
	chosen   string
	progress install.Progress
	fetching bool
}

// installSteps are the steps of the whole setup, as the pages number them.
var installSteps = []string{"저장 위치", "준비", "첫 계정", "화면", "기존 사진", "자동 정리", "연결 설정", "폰 연결"}

type step struct {
	N           int
	Name, State string // State: "done", "on" or ""
}

// stepsAt is the row of steps with the given one (from 1) being taken.
func stepsAt(n int) []step {
	out := make([]step, len(installSteps))
	for i, name := range installSteps {
		out[i] = step{N: i + 1, Name: name}
		switch {
		case i+1 < n:
			out[i].State = "done"
		case i+1 == n:
			out[i].State = "on"
		}
	}
	return out
}

func (in *Install) Handler() http.Handler {
	m := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	m.HandleFunc("GET /api/server/ping", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"res":"pong"}`)) })
	m.HandleFunc("GET /install/state", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte(`{"installing":true}`)) })
	m.HandleFunc("GET /install", in.wayPage)
	m.HandleFunc("GET /install/folder", in.folderPage)
	m.HandleFunc("POST /install/folder", in.choose)
	m.HandleFunc("GET /install/connect", in.connectPage)
	m.HandleFunc("POST /install/connect", in.connect)
	m.HandleFunc("GET /install/prepare", in.preparePage)
	m.HandleFunc("GET /install/progress", in.report)
	m.HandleFunc("POST /install/prepare", in.prepareAgain)
	m.HandleFunc("POST /install/finish", in.finish)
	m.HandleFunc("POST /install/started", in.started)
	m.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) { http.Redirect(rw, r, "/install", http.StatusSeeOther) })
	// No account exists yet to log in with, and the setup walks this machine's folders: at home only.
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !lan.From(r) && r.URL.Path != "/api/server/ping" {
			http.Error(rw, "tidoly stow 의 설치는 이 PC 나 같은 와이파이의 기기에서만 할 수 있습니다.", http.StatusForbidden)
			return
		}
		m.ServeHTTP(rw, r)
	})
}

func (in *Install) render(rw http.ResponseWriter, name string, data map[string]any) {
	data["Title"] = "설치"
	if n := data["Step"].(int); n > 0 { // the way this PC is used comes before the numbered steps
		data["Steps"] = stepsAt(n)
	}
	if err := pages.ExecuteTemplate(rw, name, data); err != nil {
		log.Printf("install: %s: %v", name, err)
	}
}

// wayPage asks how this PC is used: keeping the photos, or looking at another PC's.
func (in *Install) wayPage(rw http.ResponseWriter, r *http.Request) {
	in.render(rw, "install-way", map[string]any{"Step": 0})
}

// connectPage lists the tidoly stow servers in the house, and takes an address for one not found.
func (in *Install) connectPage(rw http.ResponseWriter, r *http.Request) {
	find := in.Find
	if find == nil {
		find = func(ctx context.Context) ([]nearby.Server, error) { return nearby.Find(ctx, 2*time.Second) }
	}
	found, err := find(r.Context())
	if err != nil {
		log.Printf("install: looking for servers: %v", err)
	}
	in.render(rw, "install-connect", map[string]any{"Step": 0, "Found": found, "Address": r.URL.Query().Get("address"), "Problem": r.URL.Query().Get("problem")})
}

// connect takes the server to look at: it must answer as tidoly stow. The settings then say so, and the
// page goes over to that server.
func (in *Install) connect(rw http.ResponseWriter, r *http.Request) {
	address := serverAddress(r.FormValue("address"))
	name, err := aboutServer(r.Context(), address)
	if err != nil {
		log.Printf("install: %s: %v", address, err)
		q := url.Values{"address": {r.FormValue("address")}, "problem": {"그 주소에서 tidoly stow 를 찾지 못했습니다. 그 PC 에서 tidoly stow 가 켜져 있는지, 같은 와이파이인지 확인해 주세요."}}
		http.Redirect(rw, r, "/install/connect?"+q.Encode(), http.StatusSeeOther)
		return
	}
	if err := (install.Config{Server: address}).Save(in.Config); err != nil {
		log.Printf("install: %v", err)
		http.Redirect(rw, r, "/install/connect?problem="+url.QueryEscape("설정을 저장하지 못했습니다: "+err.Error()), http.StatusSeeOther)
		return
	}
	log.Printf("install: this PC looks at %s (%s); no photo server here", address, name)
	in.render(rw, "install-connected", map[string]any{"Step": 0, "Server": address, "Name": name})
	in.leave()
}

// leave ends the setup once the last page is in the browser with all it loads — its stylesheet and
// scripts come from this server too, and a slow machine had not fetched them before the server went
// (2026-10-17). The page says so (started); without it, the setup ends after a while all the same.
func (in *Install) leave() {
	go func() { time.Sleep(15 * time.Second); in.end() }()
}

// started: the last page is loaded.
func (in *Install) started(rw http.ResponseWriter, r *http.Request) {
	rw.WriteHeader(http.StatusNoContent)
	go func() { time.Sleep(100 * time.Millisecond); in.end() }()
}

func (in *Install) end() { in.ended.Do(in.Done) }

// serverAddress makes what was typed into http://<host>:<port>, with stow's port when none is given.
func serverAddress(typed string) string {
	typed = strings.TrimSpace(typed)
	if !strings.Contains(typed, "://") {
		typed = "http://" + typed
	}
	u, err := url.Parse(typed)
	if err != nil || u.Host == "" {
		return typed
	}
	if u.Port() == "" && u.Scheme == "http" {
		u.Host = net.JoinHostPort(u.Hostname(), "30891")
	}
	return u.Scheme + "://" + u.Host
}

// aboutServer asks an address whether tidoly stow is there, and its name.
func aboutServer(ctx context.Context, address string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", address+"/stow/about", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var about struct{ App, Name string }
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&about) != nil || about.App != "tidoly stow" {
		return "", fmt.Errorf("not tidoly stow (%d)", res.StatusCode)
	}
	return about.Name, nil
}

// folderPage offers the suggested folder, and walks down this machine's folders for another. A folder
// that holds .stow/ is an earlier install: choosing it brings everything back.
func (in *Install) folderPage(rw http.ResponseWriter, r *http.Request) {
	at := r.URL.Query().Get("at")
	data := map[string]any{"Step": 1, "Suggest": in.Suggest, "Again": exists(filepath.Join(in.Suggest, ".stow")), "Problem": r.URL.Query().Get("problem")}
	if r.URL.Query().Has("at") {
		look, err := lookIn(at)
		if err != nil {
			http.Redirect(rw, r, "/install/folder", http.StatusSeeOther)
			return
		}
		data["Look"], data["Earlier"] = look, look.At != "" && exists(filepath.Join(look.At, ".stow"))
	}
	in.render(rw, "install", data)
}

// choose takes the folder: it is made if it is not there, and must be writable.
func (in *Install) choose(rw http.ResponseWriter, r *http.Request) {
	dir := filepath.Clean(r.FormValue("path"))
	problem := ""
	// a new folder to make inside the chosen one: a plain name, never a way out of it
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if strings.ContainsAny(name, `/\:`) || name == "." || name == ".." {
			http.Redirect(rw, r, "/install/folder?at="+url.QueryEscape(dir)+"&problem="+url.QueryEscape("새 폴더 이름에는 / \\ : 를 쓸 수 없습니다: "+name), http.StatusSeeOther)
			return
		}
		dir = filepath.Join(dir, name)
	}
	if !filepath.IsAbs(dir) {
		problem = "전체 경로가 아닙니다: " + dir
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		problem = "이 폴더를 만들 수 없습니다: " + dir
	} else if f, err := os.CreateTemp(dir, ".stow-check-"); err != nil {
		problem = "이 폴더에 쓸 수 없습니다: " + dir
	} else {
		f.Close()
		os.Remove(f.Name())
	}
	if problem != "" {
		http.Redirect(rw, r, "/install/folder?problem="+url.QueryEscape(problem), http.StatusSeeOther)
		return
	}
	in.mu.Lock()
	in.chosen = dir
	in.mu.Unlock()
	in.startFetching()
	http.Redirect(rw, r, "/install/prepare", http.StatusSeeOther)
}

func (in *Install) startFetching() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.fetching {
		return
	}
	in.fetching = true
	go func() {
		install.Fetch(context.Background(), in.Parts, &in.progress)
		in.mu.Lock()
		in.fetching = false
		in.mu.Unlock()
	}()
}

func (in *Install) preparePage(rw http.ResponseWriter, r *http.Request) {
	in.mu.Lock()
	chosen := in.chosen
	in.mu.Unlock()
	if chosen == "" {
		http.Redirect(rw, r, "/install", http.StatusSeeOther)
		return
	}
	in.render(rw, "install-prepare", map[string]any{"Step": 2, "Chosen": chosen, "Earlier": exists(filepath.Join(chosen, ".stow")), "Nothing": len(in.Parts) == 0})
}

func (in *Install) report(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(in.progress.Report())
}

func (in *Install) prepareAgain(rw http.ResponseWriter, r *http.Request) {
	in.startFetching()
	http.Redirect(rw, r, "/install/prepare", http.StatusSeeOther)
}

// finish writes the settings and lets the program end; the page waits for the photo server to come up.
func (in *Install) finish(rw http.ResponseWriter, r *http.Request) {
	in.mu.Lock()
	chosen := in.chosen
	in.mu.Unlock()
	if chosen == "" {
		http.Redirect(rw, r, "/install", http.StatusSeeOther)
		return
	}
	if err := (install.Config{Data: chosen}).Save(in.Config); err != nil {
		log.Printf("install: %v", err)
		http.Redirect(rw, r, "/install?problem="+url.QueryEscape("설정을 저장하지 못했습니다: "+err.Error()), http.StatusSeeOther)
		return
	}
	log.Printf("install: photos go to %s; starting again as the photo server", chosen)
	in.render(rw, "install-starting", map[string]any{"Step": 3})
	in.leave()
}
