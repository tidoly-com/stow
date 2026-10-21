// Package web is what a browser sees: the photos by date, place and person, and how the server is doing
// (stow issue #11). The pages are drawn here and sent as HTML; a little script loads the months near the
// screen and opens one photo over the list. Everything the pages use is inside the program.
package web

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/tidoly-com/stow/internal/disk"
	"github.com/tidoly-com/stow/internal/install"
	"github.com/tidoly-com/stow/internal/lan"
	"github.com/tidoly-com/stow/internal/library"
	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/update"
	"github.com/tidoly-com/stow/internal/vpn"
)

//go:embed static
var static embed.FS

// The pages say where the app sits as the system of this machine has it: the Mac's menu bar, Windows'
// tray, a Linux service (system).
var pages = template.Must(template.New("pages.html").Funcs(template.FuncMap{"system": func() string { return runtime.GOOS }, "source": func() string { return SourceURL }}).ParseFS(static, "static/pages.html"))

type Web struct {
	Store   *store.Store
	User    func(*http.Request) (store.User, bool) // who is asking
	Address string                                 // what to type into the phone app
	Dir     string                                 // the data folder: the first place photos are kept
	// ResetFile holds the secret of a link that sets the owner's password again, written by the app's menu
	// (MakeOwnerReset); "" when there is no such way
	ResetFile string
	Licenses  string          // the folder of notices beside the program (LICENSES/); "" where there is none
	Version   string          // this build, as the build scripts stamped it
	VPN       *vpn.Manager    // WireGuard from anywhere, off until the owner switches it on; nil: none here
	Update    *update.Checker // newer releases; nil: none looked for

	linksMu sync.Mutex
	links   map[string]time.Time // links that start connecting to tidoly in the browser, until when each is good
	// the storage locations added beside the data folder (#14)
	LocationDir func(id string) string // a location's folder in this run, "" if its disk is not there
	AddLocation func(ctx context.Context, dir string) (string, error)
	// the accounts the server keeps: an address and a password, for use at home
	NeedsOwner  func(ctx context.Context) bool // nobody has an account yet
	LocalLogin  func(rw http.ResponseWriter, r *http.Request, email, password string) error
	AddUser     func(ctx context.Context, name, email, password string) (store.User, error)
	SetPassword func(ctx context.Context, userID, password string) error
	// ChangePassword replaces a person's own password, given the one they have now
	ChangePassword func(ctx context.Context, email, current, next string) error
	// the link to tidoly (#4): logging in with tidoly from outside, through the tunnel
	TidolyHere   func(r *http.Request) bool // a browser at this address can log in with tidoly
	Outside      func(ctx context.Context) Outside
	Connect      http.HandlerFunc                                    // sends the owner to tidoly to connect
	ConnectBack  func(rw http.ResponseWriter, r *http.Request) error // takes tidoly's answer
	Disconnect   func(ctx context.Context) error
	SetAutoAdd   func(ctx context.Context, on bool) error
	Block        func(ctx context.Context, userID string, blocked bool) error
	Arrivals     func(ctx context.Context) ([]store.User, error) // came in with tidoly since the owner last looked
	SeenArrivals func(ctx context.Context) error
	Contact      string // tidoly's page for questions and suggestions; the help opens it
	// folders of photos someone already had, read where they lie (#15)
	AddFolder    func(ctx context.Context, dir, ownerID string) (string, error)
	PauseFolder  func(ctx context.Context, id string, paused bool) error
	ScanFolder   func(id string)
	RemoveFolder func(ctx context.Context, id string) error
	FolderState  func(id string) (state FolderState, ok bool) // the latest look at a folder in this run
	FacesState   func(ctx context.Context) string             // "on", "off", "waiting" (models coming), "none" (not on this PC)
	SetFaces     func(ctx context.Context, on bool) error     // group faces into people from now on, or stop
	CouponDays   bool                                         // started with the coupon helper: coupons have their last day
	SetCoupons   func(ctx context.Context, on bool) error     // look for coupons from now on, or stop
	// screenshots gathered into an album of their own, on unless switched off
	ScreenshotsOn  func(ctx context.Context) bool
	SetScreenshots func(ctx context.Context, on bool) error
	Changed        func(userID string)                         // tells that person's devices to sync
	Events         func(userID string) (<-chan string, func()) // that person's changes, as lines of JSON, for open pages
	Trash          func(ctx context.Context, userID string, ids []string) error
	Restore        func(ctx context.Context, userID string, ids []string) (int, error)
	Empty          func(ctx context.Context, userID string) (int, error)
	Days           int       // how long the trash keeps a photo
	Links          LinkFuncs // shared links (#13), while the tunnel is on
	// what a build brings beside the photos: its name after ours on the top bar, its sets of pages, and
	// the routes those pages answer on
	Edition string
	Spaces  []Space
	Mount   []func(m *http.ServeMux)
	mux     *http.ServeMux
}

const trashLimit = 2000 // photos shown in the trash

type card struct{ Link, Picture, Name, Detail string }

// month is one section of a page of photos by date: "2026-09", how it is shown, and how many photos it has.
type month struct {
	Key, Label string
	Count      int
}

// Outside is how this server is reached from outside the house, for the admin page.
type Outside struct {
	Host    string // its address outside, once connected to tidoly; "" if it is not
	Tunnel  string // how the tunnel is doing, in words; "" while it is up or not wanted
	Problem string // what went wrong with tidoly lately
	AutoAdd bool   // someone tidoly lists for this server gets an account the first time they come
	Cert    string // how getting this server's own certificate goes, in words; "" once it has one
	HTTPS   bool   // it has one: the address outside ends https on this PC
	Busy    bool   // the tunnel or the certificate is on its way: the page looks again in a few seconds
	Last    string // not connected now: the address outside it had before, if it ever was connected
}

// FolderState is how the latest look at a read-only folder went, or is going.
type FolderState struct {
	Running                 bool
	Seen, Added, Same, Gone int64
	Total                   int64 // photos and videos in the folder; still going up until Counted
	Counted                 bool
	Failure                 string
}

// cell is one photo in a list. W and H are its shape as shown, 0 until the file has been read.
type cell struct {
	ID, Length      string
	W, H            int
	Video, Favorite bool
	Shared          bool   // its owner shows it to everyone
	Live            bool   // a live photo: a few seconds of video go with it
	By, ByName      string // whose it is — a letter and a name — when it is not the viewer's own
	Tone            int    // that person's color
	Key             string // on a shared link's page: the link's key, which lets its photos be fetched
}

type person struct {
	ID, Initial, Name string
	Count, Tone       int
}

// tone gives a person one of six colors, the same one every time, so that two people with the same
// first letter can still be told apart.
func tone(userID string) int {
	sum := 0
	for _, b := range []byte(userID) {
		sum = sum*31 + int(b)
	}
	return (sum%6 + 6) % 6
}

func initial(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}

type day struct {
	Label string
	Cells []cell
}

// cells makes the list for one viewer. names says what to call the owners of photos that are not the
// viewer's; nil when the list holds only the viewer's own.
func cells(assets []store.Asset, viewerID string, names map[string]string) []cell {
	out := make([]cell, len(assets))
	for i, a := range assets {
		c := cell{ID: a.ID, Video: a.Type == "VIDEO", Favorite: a.Favorite, Shared: a.Shared, Live: a.Live != ""}
		if a.OwnerID != viewerID {
			c.ByName, c.Favorite, c.Tone = names[a.OwnerID], false, tone(a.OwnerID)
			c.By = initial(c.ByName)
		}
		if m := a.Meta; m != nil && m.Width > 0 && m.Height > 0 {
			c.W, c.H = m.Width, m.Height
			if m.Orientation >= 5 { // stored lying on its side
				c.W, c.H = m.Height, m.Width
			}
		}
		if c.Video && a.DurationMs > 0 {
			sec := (a.DurationMs + 500) / 1000
			c.Length = fmt.Sprintf("%d:%02d", sec/60, sec%60)
		}
		out[i] = c
	}
	return out
}

// stamp is the moment a photo is filed under: the clock on the wall where it was taken, or the phone's
// file time until the file has been read. It is what the store sorts and groups by.
func stamp(a store.Asset) string {
	if a.Meta != nil && a.Meta.LocalTime != "" {
		return a.Meta.LocalTime
	}
	return a.FileCreatedAt
}

var weekdays = [...]string{"일", "월", "화", "수", "목", "금", "토"}

// days cuts a list that is already in order into days, each with the heading a person would say.
func days(list []cell, assets []store.Asset, now time.Time) []day {
	var out []day
	last := ""
	for i, a := range assets {
		key := stamp(a)
		if len(key) > 10 {
			key = key[:10]
		}
		if key != last || len(out) == 0 {
			label := key
			if at, err := time.Parse("2006-01-02", key); err == nil {
				switch key {
				case now.Format("2006-01-02"):
					label = "오늘"
				case now.AddDate(0, 0, -1).Format("2006-01-02"):
					label = "어제"
				default:
					label = at.Format("2006년 1월 2일") + " (" + weekdays[at.Weekday()] + ")"
				}
			}
			out = append(out, day{Label: label})
			last = key
		}
		out[len(out)-1].Cells = append(out[len(out)-1].Cells, list[i])
	}
	return out
}

func bytesText(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	if n < 1<<30 {
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

// countText writes a count with its thousands apart: 14,298.
func countText(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// progressText is how far a count of done things is in all of them: "3,913 / 14,298장 (27%)".
func progressText(done, total int64) string {
	if total <= 0 {
		return countText(done) + "장"
	}
	return fmt.Sprintf("%s / %s장 (%d%%)", countText(done), countText(total), percent(done, total))
}

// percent rounds down, so 100 means all of them, never all but a few.
func percent(done, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return min(done*100/total, 100)
}

// frame is what every page needs around its own content: who is looking and how full the disk is.
func (w *Web) frame(ctx context.Context, user store.User, title, tab string) map[string]any {
	me := user.Name
	if me == "" {
		me = user.Email
	}
	data := map[string]any{"Title": title, "Tab": tab, "User": user, "Me": initial(me), "Version": w.Version, "LinksOn": w.linksOn()}
	data["Edition"], data["Spaces"] = w.Edition, w.Spaces
	if s := w.space(tab); s != nil {
		data["Space"] = s
	}
	dir := w.Dir // the disk shown is the one this person's next photo goes to
	if id, err := w.Store.LocationFor(ctx, user.ID); err == nil && id != "" && w.LocationDir != nil && w.LocationDir(id) != "" {
		dir = w.LocationDir(id)
	}
	if total, free, ok := disk.Space(dir); ok && total > 0 {
		data["Disk"] = map[string]any{"Total": bytesText(total), "Used": bytesText(total - free), "Free": bytesText(free),
			"Max": total >> 20, "Value": (total - free) >> 20}
	}
	return data
}

// ids reads the photos a form names.
func ids(r *http.Request) []string {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if r.ParseForm() != nil {
		return nil
	}
	return r.PostForm["id"]
}

func (w *Web) Handler(loginStart, logout http.HandlerFunc) http.Handler {
	m := http.NewServeMux()
	files, _ := fs.Sub(static, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(files)))
	m.HandleFunc("GET /login", func(rw http.ResponseWriter, r *http.Request) {
		if w.NeedsOwner(r.Context()) && lan.From(r) {
			http.Redirect(rw, r, "/setup", http.StatusSeeOther)
			return
		}
		w.render(rw, "login", map[string]any{"Title": "로그인", "Refused": r.URL.Query().Get("refused"), "Tidoly": w.TidolyHere(r), "Away": !lan.From(r)})
	})
	m.HandleFunc("POST /login", w.localLogin)
	m.HandleFunc("GET /setup", w.setup)
	m.HandleFunc("GET /owner/reset", w.ownerReset)
	m.HandleFunc("GET /licenses", w.page(w.licenses))
	m.Handle("GET /licenses/files/", w.licenseFiles())
	m.HandleFunc("POST /owner/reset", w.ownerReset)
	m.HandleFunc("GET /start/screen", w.page(w.startScreen))
	m.HandleFunc("GET /start/folders", w.page(w.startFolders))
	m.HandleFunc("GET /start/auto", w.page(w.startAuto))
	m.HandleFunc("GET /outside", w.page(w.outside))
	m.HandleFunc("GET /welcome", w.page(w.welcome))
	m.HandleFunc("GET /help", w.page(w.help))
	m.HandleFunc("GET /me/password", w.page(w.ownPassword))
	m.HandleFunc("POST /me/password", w.page(w.ownPassword))
	m.HandleFunc("GET /welcome/qr.png", w.page(w.welcomeQR))
	m.HandleFunc("GET /phone", w.phone)
	m.HandleFunc("POST /setup", w.setup)
	m.HandleFunc("POST /admin/users", w.page(w.adminUsers))
	m.HandleFunc("POST /admin/users/password", w.page(w.adminUsers))
	m.HandleFunc("GET /auth/start", loginStart)
	m.HandleFunc("POST /logout", logout)
	m.HandleFunc("GET /{$}", w.page(w.timeline))
	m.HandleFunc("GET /share/{key}", w.sharePage)
	m.HandleFunc("POST /share/{key}", w.sharePage)
	m.HandleFunc("GET /share/{key}/{id}", w.sharePhoto)
	m.HandleFunc("POST /web/links", w.page(w.linkMake))
	m.HandleFunc("GET /links", w.page(w.linksPage))
	m.HandleFunc("POST /links/{id}", w.page(w.linkChange))
	m.HandleFunc("POST /links/{id}/delete", w.page(w.linkChange))
	m.HandleFunc("GET /web/month/{month}", w.page(w.month))
	m.HandleFunc("GET /web/events", w.page(w.events))
	m.HandleFunc("GET /web/photo/{id}", w.page(w.photo))
	m.HandleFunc("POST /web/photos/delete", w.page(w.remove))
	m.HandleFunc("POST /web/photos/restore", w.page(w.restore))
	m.HandleFunc("POST /web/photos/favorite", w.page(w.favorite))
	m.HandleFunc("POST /web/photos/share", w.page(w.share))
	m.HandleFunc("GET /web/albums", w.page(w.albumPicker))
	m.HandleFunc("POST /web/album", w.page(w.albumChange))
	m.HandleFunc("POST /albums", w.page(w.albumCreate))
	m.HandleFunc("GET /trash", w.page(w.trash))
	m.HandleFunc("POST /trash", w.page(w.trashAct))
	m.HandleFunc("GET /places", w.page(w.places))
	m.HandleFunc("GET /people", w.page(w.people))
	m.HandleFunc("GET /people/{id}", w.page(w.person))
	m.HandleFunc("POST /people/{id}/hide", w.page(w.hide))
	m.HandleFunc("POST /people/{id}/name", w.page(w.name))
	m.HandleFunc("GET /albums", w.page(w.albums))
	m.HandleFunc("GET /albums/{id}", w.page(w.album))
	m.HandleFunc("GET /status", w.page(w.status))
	m.HandleFunc("GET /admin", w.page(w.admin))
	m.HandleFunc("GET /admin/folders", w.page(w.adminFolders))
	m.HandleFunc("POST /admin/locations", w.page(w.adminAddLocation))
	m.HandleFunc("POST /admin/location", w.page(w.adminSetLocation))
	m.HandleFunc("POST /admin/import", w.page(w.adminImport))
	m.HandleFunc("POST /admin/import/scan", w.page(w.adminImportAct))
	m.HandleFunc("POST /admin/import/remove", w.page(w.adminImportAct))
	m.HandleFunc("POST /admin/import/pause", w.page(w.adminImportAct))
	m.HandleFunc("POST /admin/coupons", w.page(w.adminCoupons))
	m.HandleFunc("POST /admin/screenshots", w.page(w.adminScreenshots))
	m.HandleFunc("POST /admin/faces", w.page(w.adminFaces))
	m.HandleFunc("GET /admin/tidoly/connect", w.startConnect)
	m.HandleFunc("GET /admin/tidoly/back", w.finishConnect)
	m.HandleFunc("POST /admin/tidoly/connect-link", w.page(w.connectLink))
	m.HandleFunc("POST /admin/update", w.page(w.adminUpdate))
	m.HandleFunc("GET /admin/update", w.page(w.updating))
	m.HandleFunc("GET /admin/update/state", w.page(w.updateState))
	m.HandleFunc("POST /admin/vpn/switch", w.page(w.adminVPN))
	m.HandleFunc("POST /admin/vpn/endpoint", w.page(w.adminVPN))
	m.HandleFunc("POST /admin/vpn/devices", w.page(w.adminVPN))
	m.HandleFunc("POST /admin/vpn/devices/remove", w.page(w.adminVPN))
	m.HandleFunc("GET /admin/vpn/qr", w.page(w.vpnQR))
	m.HandleFunc("GET /admin/tidoly/state", w.page(w.connectState))
	m.HandleFunc("POST /admin/tidoly/disconnect", w.page(w.adminTidoly))
	m.HandleFunc("POST /admin/tidoly/auto", w.page(w.adminTidoly))
	m.HandleFunc("POST /admin/tidoly/seen", w.page(w.adminTidoly))
	m.HandleFunc("POST /admin/users/block", w.page(w.adminTidoly))
	for _, mount := range w.Mount {
		mount(m)
	}
	w.mux = m
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// nothing here may be framed by another site, and nothing loads from anywhere but this server
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Referrer-Policy", "same-origin")
		m.ServeHTTP(rw, r)
	})
}

// page lets in only someone with a session; a browser without one is sent to the login page.
func (w *Web) page(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		user, ok := w.User(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/web/") {
				http.Error(rw, "", http.StatusUnauthorized)
				return
			}
			http.Redirect(rw, r, "/login", http.StatusSeeOther)
			return
		}
		rw.Header().Set("Cache-Control", "no-store")
		next(rw, r, user)
	}
}

func (w *Web) render(rw http.ResponseWriter, name string, data any) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(rw, name, data); err != nil {
		log.Printf("web: %s: %v", name, err)
	}
}

func (w *Web) fail(rw http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(rw, nil)
		return
	}
	log.Printf("web: %v", err)
	http.Error(rw, "잠시 뒤에 다시 시도해 주세요.", http.StatusInternalServerError)
}

func (w *Web) timeline(rw http.ResponseWriter, r *http.Request, user store.User) {
	show, by, at := r.URL.Query().Get("show"), r.URL.Query().Get("by"), r.URL.Query().Get("at")
	months, err := w.Store.Months(r.Context(), user.ID, show, by, at, store.Narrow{})
	if err != nil {
		w.fail(rw, err)
		return
	}
	var sharers []person
	if show == store.ShowShared {
		sharers, _ = w.sharers(r.Context(), user)
		if len(sharers) < 2 && by == "" {
			sharers = nil // one person, or none: nothing to tell apart
		}
	}
	list, total := make([]month, len(months)), 0
	for i, m := range months {
		list[i] = month{m.Key, strings.Replace(m.Key, "-", "년 ", 1) + "월", m.Count}
		total += m.Count
	}
	data := w.frame(r.Context(), user, "사진", "timeline")
	if show == store.ShowVideos { // a menu of its own: the videos only, by date
		data = w.frame(r.Context(), user, "동영상", "videos")
		data["Total"] = total
	}
	data["Months"], data["Show"], data["By"], data["Sharers"], data["Bar"], data["Address"] = list, show, by, sharers, "photos", w.Address
	data["Upload"] = true                     // the upload button: on the photo and video pages, not on people, places or admin
	if show == "" || show == store.ShowMine { // where my photos lie, when that is more than one place
		data["At"], data["Where"] = at, w.wheres(r.Context(), user)
	}
	w.render(rw, "timeline", data)
}

func (w *Web) month(rw http.ResponseWriter, r *http.Request, user store.User) {
	q := r.URL.Query()
	n := store.Narrow{Place: q.Get("in"), Person: q.Get("person"), Album: q.Get("album")}
	assets, err := w.Store.AssetsWhere(r.Context(), user.ID, r.PathValue("month"), n, q.Get("show"), q.Get("by"), q.Get("at"), 100000)
	if err != nil {
		w.fail(rw, err)
		return
	}
	_, names := w.sharers(r.Context(), user)
	w.render(rw, "days", days(cells(assets, user.ID, names), assets, time.Now()))
}

// wheres lists the storage locations that hold photos of this person, by name, for narrowing the list to
// one of them. With photos in only one place there is nothing to choose and it lists nothing.
func (w *Web) wheres(ctx context.Context, user store.User) []person {
	counts, _ := w.Store.LocationsOf(ctx, user.ID)
	if len(counts) < 2 {
		return nil
	}
	found, _ := w.Store.Locations(ctx)
	names := map[string]string{}
	for _, l := range found {
		names[l.ID] = filepath.Base(l.Path)
	}
	names[""] = filepath.Base(w.Dir)
	out := make([]person, len(counts))
	for i, c := range counts {
		out[i] = person{ID: c.Key, Name: names[c.Key], Count: c.Count}
		if c.Key == "" {
			out[i].ID = store.HomeLocation
		}
	}
	return out
}

// sharers lists who shares photos on this server — the viewer first, as "나" — and what to call each.
func (w *Web) sharers(ctx context.Context, user store.User) ([]person, map[string]string) {
	found, _ := w.Store.Sharers(ctx)
	names := map[string]string{}
	var out []person
	for _, c := range found {
		names[c.Key] = c.Cover
		p := person{ID: c.Key, Initial: initial(c.Cover), Name: c.Cover, Count: c.Count, Tone: tone(c.Key)}
		if c.Key == user.ID {
			p.Name, p.Tone = "나", -1
			out = append([]person{p}, out...)
		} else {
			out = append(out, p)
		}
	}
	return out, names
}

// places lists where one's photos were taken, at three widths — countries, the states in each, the cities
// in each state — most photos first; the page shows one width at a time. With "in" it is the photos of one
// place, by date, as on the photos page.
func (w *Web) places(rw http.ResponseWriter, r *http.Request, user store.User) {
	if in := r.URL.Query().Get("in"); in != "" {
		w.placePhotos(rw, r, user, in)
		return
	}
	found, err := w.Store.Places(r.Context(), user.ID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	countries := []*area{}
	for _, p := range found { // most photos first, so the first city seen of an area is its biggest: its cover
		parts := strings.SplitN(p.Key, "|", 3)
		if len(parts) < 3 {
			continue
		}
		country := within(&countries, parts[0], parts[0], p.Cover)
		state := within(&country.Parts, parts[1], parts[0]+"|"+parts[1], p.Cover)
		city := within(&state.Parts, parts[2], p.Key, p.Cover)
		country.Count += p.Count
		state.Count += p.Count
		city.Count += p.Count
	}
	for _, c := range countries {
		sortAreas(c.Parts)
		for _, s := range c.Parts {
			sortAreas(s.Parts)
		}
	}
	sortAreas(countries)
	data := w.frame(r.Context(), user, "장소", "places")
	data["Countries"], data["Cities"] = countries, len(found)
	states := 0
	for _, c := range countries {
		states += len(c.Parts)
	}
	data["States"] = states
	w.render(rw, "places", data)
}

// area is a country, a state or a city with the photos taken there.
type area struct {
	Name, Key, Cover string
	Count            int
	Parts            []*area
}

func (a *area) Link() string    { return "/places?in=" + url.QueryEscape(a.Key) }
func (a *area) Picture() string { return "/api/assets/" + a.Cover + "/thumbnail" }

func within(list *[]*area, name, key, cover string) *area {
	for _, a := range *list {
		if a.Name == name {
			return a
		}
	}
	a := &area{Name: name, Key: key, Cover: cover}
	*list = append(*list, a)
	return a
}

func sortAreas(list []*area) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Count > list[j].Count })
}

// placePhotos is the photos of one place, with the wider places it lies in above the title.
func (w *Web) placePhotos(rw http.ResponseWriter, r *http.Request, user store.User, in string) {
	parts := strings.SplitN(in, "|", 3)
	type crumb struct{ Name, Link string }
	var up []crumb
	for i := range parts[:len(parts)-1] {
		up = append(up, crumb{parts[i], "/places?in=" + url.QueryEscape(strings.Join(parts[:i+1], "|"))})
	}
	data := w.frame(r.Context(), user, parts[len(parts)-1], "places")
	data["Up"] = up
	w.narrowed(rw, r, data, user, store.Narrow{Place: in}, "in="+url.QueryEscape(in))
}

// narrowed is the photos page kept to one place, person or album: by month, newest first, with the years
// at the side. query is what the months are fetched with.
func (w *Web) narrowed(rw http.ResponseWriter, r *http.Request, data map[string]any, user store.User, n store.Narrow, query string) {
	months, err := w.Store.Months(r.Context(), user.ID, "", "", "", n)
	if err != nil {
		w.fail(rw, err)
		return
	}
	total := 0
	list := make([]month, len(months))
	for i, m := range months {
		list[i] = month{m.Key, strings.Replace(m.Key, "-", "년 ", 1) + "월", m.Count}
		total += m.Count
	}
	if data["Bar"] == nil {
		data["Bar"] = "photos"
	}
	data["Months"], data["Narrowed"], data["Query"], data["Total"] = list, true, "?"+query, total
	w.render(rw, "timeline", data)
}

func (w *Web) people(rw http.ResponseWriter, r *http.Request, user store.User) {
	hidden := r.URL.Query().Get("hidden") != ""
	found, err := w.Store.People(r.Context(), user.ID, hidden)
	if err != nil {
		w.fail(rw, err)
		return
	}
	away, _ := w.Store.People(r.Context(), user.ID, true)
	lone, _ := w.Store.LoneFaces(r.Context(), user.ID)
	cards := make([]card, len(found))
	for i, p := range found {
		name := p.Cover // People puts the person's name here
		if name == "" {
			name = "이름 없음"
		}
		cards[i] = card{"/people/" + p.Key, "/api/people/" + p.Key + "/thumbnail", name, fmt.Sprintf("%d장", p.Count)}
	}
	data := w.frame(r.Context(), user, "인물", "people")
	data["Cards"], data["Faces"], data["Hidden"], data["Away"], data["Lone"] = cards, w.faces(r.Context()), hidden, len(away), lone
	w.render(rw, "people", data)
}

func (w *Web) albums(rw http.ResponseWriter, r *http.Request, user store.User) {
	var cards []card
	err := w.Store.AlbumsSince(r.Context(), user.ID, 0, func(b store.Album) error {
		picture := ""
		if b.CoverID != "" {
			picture = "/api/assets/" + b.CoverID + "/thumbnail"
		}
		cards = append([]card{{"/albums/" + b.ID, picture, b.Name, fmt.Sprintf("%d장", b.Count)}}, cards...) // newest change first
		return nil
	})
	if err != nil {
		w.fail(rw, err)
		return
	}
	if r.URL.Path == "/web/albums" {
		w.render(rw, "picker", cards)
		return
	}
	data := w.frame(r.Context(), user, "앨범", "albums")
	data["Cards"] = cards
	w.render(rw, "albums", data)
}

// albumPicker lists the albums for choosing one to put photos into.
func (w *Web) albumPicker(rw http.ResponseWriter, r *http.Request, user store.User) {
	w.albums(rw, r, user)
}

// albumCreate makes an empty album from the albums page and opens it.
func (w *Web) albumCreate(rw http.ResponseWriter, r *http.Request, user store.User) {
	name := albumName(r.FormValue("name"))
	if name == "" {
		http.Redirect(rw, r, "/albums", http.StatusSeeOther)
		return
	}
	b, err := w.Store.CreateAlbum(r.Context(), user.ID, name, "")
	if err != nil {
		w.fail(rw, err)
		return
	}
	w.changed(user)
	http.Redirect(rw, r, "/albums/"+b.ID, http.StatusSeeOther)
}

// albumChange puts the named photos into an album — an existing one, or a new one of the given name —
// or takes them out of it.
func (w *Web) albumChange(rw http.ResponseWriter, r *http.Request, user store.User) {
	photos, albumID := ids(r), r.PostFormValue("album")
	if albumID == "" {
		name := albumName(r.PostFormValue("name"))
		if name == "" {
			http.Error(rw, "", http.StatusBadRequest)
			return
		}
		b, err := w.Store.CreateAlbum(r.Context(), user.ID, name, "")
		if err != nil {
			w.fail(rw, err)
			return
		}
		albumID = b.ID
	}
	if _, err := w.Store.ChangeAlbumAssets(r.Context(), user.ID, albumID, photos, r.PostFormValue("remove") == ""); err != nil {
		w.fail(rw, err) // also the answer for another member's album
		return
	}
	w.changed(user)
	rw.WriteHeader(http.StatusNoContent)
}

func albumName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

func (w *Web) changed(user store.User) {
	if w.Changed != nil {
		w.Changed(user.ID)
	}
}

func (w *Web) album(rw http.ResponseWriter, r *http.Request, user store.User) {
	b, err := w.Store.Album(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		w.fail(rw, err) // also the answer for another member's album
		return
	}
	data := w.frame(r.Context(), user, b.Name, "albums")
	data["Album"], data["Bar"], data["Upload"] = b.ID, "album", true // photos uploaded here join the album
	w.narrowed(rw, r, data, user, store.Narrow{Album: b.ID}, "album="+url.QueryEscape(b.ID))
}

func (w *Web) person(rw http.ResponseWriter, r *http.Request, user store.User) {
	person, err := w.Store.Person(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		w.fail(rw, err) // also the answer for another member's person
		return
	}
	name := person.Name
	if name == "" {
		name = "이름 없음"
	}
	data := w.frame(r.Context(), user, name, "people")
	data["Person"] = &person
	w.narrowed(rw, r, data, user, store.Narrow{Person: person.ID}, "person="+url.QueryEscape(person.ID))
}

// name gives a person a name. A named person shows in the phone app even with only two photos.
func (w *Web) name(rw http.ResponseWriter, r *http.Request, user store.User) {
	name := strings.TrimSpace(r.FormValue("name"))
	if len(name) > 100 {
		name = name[:100]
	}
	if err := w.Store.NamePerson(r.Context(), user.ID, r.PathValue("id"), name); err != nil {
		w.fail(rw, err)
		return
	}
	if w.Changed != nil {
		w.Changed(user.ID)
	}
	http.Redirect(rw, r, "/people/"+r.PathValue("id"), http.StatusSeeOther)
}

// hide puts a person away, or brings them back when the form says so, and tells the owner's devices.
func (w *Web) hide(rw http.ResponseWriter, r *http.Request, user store.User) {
	hidden := r.FormValue("show") == ""
	if err := w.Store.HidePerson(r.Context(), user.ID, r.PathValue("id"), hidden); err != nil {
		w.fail(rw, err)
		return
	}
	if w.Changed != nil {
		w.Changed(user.ID)
	}
	http.Redirect(rw, r, "/people", http.StatusSeeOther)
}

func (w *Web) photo(rw http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	asset, err := w.Store.AssetFor(ctx, user.ID, r.PathValue("id"))
	if err != nil {
		w.fail(rw, err) // also the answer for a photo of someone else's that is not shared
		return
	}
	mine, by := asset.OwnerID == user.ID, ""
	if !mine {
		_, names := w.sharers(ctx, user)
		by = names[asset.OwnerID]
	}
	when, where, place, camera, shot, shape := stamp(asset), "", "", "", "", ""
	if m := asset.Meta; m != nil {
		if m.City != "" {
			where, place = m.City+", "+m.State, m.Country+"|"+m.State+"|"+m.City
		}
		camera = strings.TrimSpace(m.Make + " " + m.Model)
		var parts []string
		if m.FNumber > 0 {
			parts = append(parts, fmt.Sprintf("f/%.1f", m.FNumber))
		}
		if m.ExposureTime >= 1 {
			parts = append(parts, fmt.Sprintf("%.0f초", m.ExposureTime))
		} else if m.ExposureTime > 0 {
			parts = append(parts, fmt.Sprintf("1/%.0f초", 1/m.ExposureTime))
		}
		if m.FocalLength > 0 {
			parts = append(parts, fmt.Sprintf("%.0fmm", m.FocalLength))
		}
		if m.ISO > 0 {
			parts = append(parts, fmt.Sprintf("ISO %d", m.ISO))
		}
		shot = strings.Join(parts, " · ")
		if c := cells([]store.Asset{asset}, asset.OwnerID, nil)[0]; c.W > 0 {
			shape = fmt.Sprintf("%d × %d", c.W, c.H)
		}
	}
	if at, err := time.Parse(time.RFC3339Nano, when); err == nil {
		if asset.Meta == nil || asset.Meta.LocalTime == "" {
			at = at.Local() // not read yet: the phone's time, shown in this machine's zone
		}
		when = at.Format("2006년 1월 2일") + " (" + weekdays[at.Weekday()] + ") " + at.Format("15:04")
	}
	if !mine {
		place = "" // the places are of one's own photos; there is nothing of this one to go to
	}
	// zoomed in, the page swaps the preview for the file itself — when it is of a kind browsers show — or,
	// for HEIC, for a full-size JPEG the server makes the first time it is asked (stow issue #3)
	full := ""
	if asset.Type != "VIDEO" && asset.DeletedAt == "" {
		switch strings.ToLower(path.Ext(asset.OriginalName)) {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif":
			full = "/api/assets/" + asset.ID + "/original"
		case ".heic", ".heif":
			full = "/api/assets/" + asset.ID + "/full"
		}
	}
	people, _ := w.Store.PeopleIn(ctx, user.ID, asset.ID) // none for someone else's photo
	albums, _ := w.Store.AlbumsWith(ctx, user.ID, asset.ID)
	w.render(rw, "photo", map[string]any{"Asset": asset, "When": when, "Where": where, "Place": place, "Camera": camera,
		"Shot": shot, "Shape": shape, "Size": bytesText(asset.Size), "People": people, "Albums": albums, "Mine": mine, "By": by, "Full": full})
}

// remove moves the named photos to the trash.
func (w *Web) remove(rw http.ResponseWriter, r *http.Request, user store.User) {
	if err := w.Trash(r.Context(), user.ID, ids(r)); err != nil {
		w.fail(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

// restore brings the named photos back from the trash.
func (w *Web) restore(rw http.ResponseWriter, r *http.Request, user store.User) {
	photos := ids(r)
	if len(photos) == 0 { // Restore takes no names to mean everything
		http.Error(rw, "", http.StatusBadRequest)
		return
	}
	if _, err := w.Restore(r.Context(), user.ID, photos); err != nil {
		w.fail(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

// share shows the named photos to everyone on this server, or stops showing them. Only one's own change.
func (w *Web) share(rw http.ResponseWriter, r *http.Request, user store.User) {
	photos := ids(r)
	if _, err := w.Store.SetShared(r.Context(), user.ID, photos, r.PostFormValue("on") != ""); err != nil {
		w.fail(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

// favorite marks the named photos as favorites, or takes the mark off, and tells the owner's devices.
func (w *Web) favorite(rw http.ResponseWriter, r *http.Request, user store.User) {
	photos := ids(r)
	if _, err := w.Store.SetFavorite(r.Context(), user.ID, photos, r.PostFormValue("on") != ""); err != nil {
		w.fail(rw, err)
		return
	}
	w.changed(user)
	rw.WriteHeader(http.StatusNoContent)
}

func (w *Web) trash(rw http.ResponseWriter, r *http.Request, user store.User) {
	assets, err := w.Store.TrashedAssets(r.Context(), user.ID, trashLimit)
	if err != nil {
		w.fail(rw, err)
		return
	}
	// the ones to go first, first; grouped by how many days they have left
	sort.SliceStable(assets, func(i, j int) bool { return assets[i].DeletedAt < assets[j].DeletedAt })
	all := cells(assets, user.ID, nil)
	var groups []day
	for i, a := range assets {
		label := "곧 완전히 지워집니다"
		if at, err := time.Parse(time.RFC3339Nano, a.DeletedAt); err == nil {
			if left := int(time.Until(at.AddDate(0, 0, w.Days)).Hours() / 24); left >= 1 {
				label = fmt.Sprintf("%d일 뒤 완전히 지워집니다", left)
			}
		}
		if n := len(groups); n == 0 || groups[n-1].Label != label {
			groups = append(groups, day{Label: label})
		}
		groups[len(groups)-1].Cells = append(groups[len(groups)-1].Cells, all[i])
	}
	data := w.frame(r.Context(), user, "휴지통", "trash")
	data["Groups"], data["Count"], data["Days"], data["Bar"] = groups, len(assets), w.Days, "trash"
	w.render(rw, "trash", data)
}

// trashAct brings everything back, or — when the form says so — deletes it all for good.
func (w *Web) trashAct(rw http.ResponseWriter, r *http.Request, user store.User) {
	var err error
	if r.FormValue("empty") != "" {
		_, err = w.Empty(r.Context(), user.ID)
	} else {
		_, err = w.Restore(r.Context(), user.ID, nil)
	}
	if err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, "/trash", http.StatusSeeOther)
}

var jobNames = map[string]string{"metadata": "사진 정보 읽기", "thumbnail": "작은 그림 만들기", "place": "장소 이름 붙이기", "face": "얼굴 찾기", "coupon": "쿠폰 찾기"}

// jobsLeft is what is still to be done in the background, and the work going on now: "얼굴 찾기 · 장소 이름 붙이기".
func jobsLeft(reports []store.JobReport) (left int, now string) {
	var doing []string
	for _, kind := range []string{"metadata", "thumbnail", "place", "face", "coupon"} {
		for _, rep := range reports {
			if rep.Kind != kind || rep.State != "queued" && rep.State != "running" {
				continue
			}
			left += rep.Count
			if rep.State == "running" {
				doing = append(doing, jobNames[kind])
			}
		}
	}
	return left, strings.Join(doing, " · ")
}

func (w *Web) status(rw http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	files, bytes, err := w.Store.Totals(ctx, user.ID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	reports, _ := w.Store.JobReports(ctx)
	failures, _ := w.Store.Failures(ctx, user.ID, 100)
	camera, located, _ := w.Store.LocationCounts(ctx, user.ID)
	type row struct {
		Name                          string
		Queued, Running, Done, Failed int
		Progress                      string // "12,481 / 14,760장 (84%)": done or given up, in all of them
		Max, Value                    int
	}
	var jobs []row
	for _, kind := range []string{"metadata", "thumbnail", "place", "face", "coupon"} {
		line := row{Name: jobNames[kind]}
		for _, rep := range reports {
			if rep.Kind != kind {
				continue
			}
			switch rep.State {
			case "queued":
				line.Queued = rep.Count
			case "running":
				line.Running = rep.Count
			case "done":
				line.Done = rep.Count
			case "failed":
				line.Failed = rep.Count
			}
		}
		line.Max, line.Value = line.Queued+line.Running+line.Done+line.Failed, line.Done+line.Failed
		line.Progress = progressText(int64(line.Value), int64(line.Max))
		if kind != "face" || w.faces(r.Context()) == "on" {
			jobs = append(jobs, line)
		}
	}
	for i := range failures {
		failures[i].Kind = jobNames[failures[i].Kind]
	}
	data := w.frame(r.Context(), user, "상태", "status")
	left, now := jobsLeft(reports)
	for k, v := range map[string]any{"Addr": w.addresses(r.Context()), "Invite": w.invite(r.Context(), ""), "Files": files, "Size": bytesText(bytes), "Jobs": jobs, "Failures": failures,
		"Left": left, "LeftText": countText(int64(left)), "Now": now,
		"LocationOff": camera >= 10 && located == 0, "Camera": camera} {
		data[k] = v
	}
	w.render(rw, "status", data)
}

// ── for the owner of the server ─────────────────────────────────────────────────────────────────────────────

type place struct {
	ID, Name, Path, Size, Total, Free string
	Owner, State                      string // of a folder that is only read: whose library it joins, how the latest look went
	Running, Paused, Mine             bool   // Mine: it holds photos of the person looking, so there is something to go and see
	Photos                            int
	Max, Value                        int64
	Current, Missing                  bool
}

// admin is the page of the person who runs the server: where photos are kept, and where each person's
// new ones go (stow issue #14). To everyone else it does not exist.
func (w *Web) admin(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	w.adminPage(rw, r, user, "", http.StatusOK)
}

// placesFor lists where photos are put and the folders read where they lie, as the owner sees them.
func (w *Web) placesFor(ctx context.Context, user store.User) (places, folders []place, err error) {
	found, err := w.Store.Locations(ctx)
	if err != nil {
		return nil, nil, err
	}
	current, _ := w.Store.DefaultLocation(ctx)
	users, _ := w.Store.Users(ctx)
	called := map[string]string{}
	for _, u := range users {
		called[u.ID] = u.Name
		if u.Name == "" {
			called[u.ID] = u.Email
		}
	}
	for _, l := range found {
		p := place{ID: l.ID, Path: l.Path, Photos: l.Photos, Size: bytesText(l.Bytes), Current: l.ID == current}
		dir := w.Dir
		if l.ID != "" {
			dir = w.LocationDir(l.ID)
			p.Missing = dir == ""
		} else {
			p.Path = w.Dir
		}
		if total, free, ok := disk.Space(dir); ok && total > 0 && !p.Missing {
			p.Total, p.Free, p.Max, p.Value = bytesText(total), bytesText(free), total>>20, (total-free)>>20
		}
		p.Name = filepath.Base(p.Path)
		if l.Kind != store.KindFolder {
			p.Mine = l.Photos > 0
			places = append(places, p)
			continue
		}
		p.Owner, p.Paused, p.Mine = called[l.OwnerID], l.Paused, l.OwnerID == user.ID && !l.Paused
		if state, ok := w.FolderState(l.ID); ok && !l.Paused {
			p.Running = state.Running
			switch {
			case state.Running && !state.Counted:
				p.State = fmt.Sprintf("파일 세는 중 — %s개", countText(state.Total))
			case state.Running:
				p.State = "훑는 중 — " + progressText(state.Seen, state.Total)
			case state.Failure != "":
				p.State = "훑지 못했습니다. 디스크가 연결되어 있는지 확인해 주세요"
			default:
				p.State = fmt.Sprintf("%d개 봄 · 새로 %d · 이미 있던 사진 %d · 사라짐 %d", state.Seen, state.Added, state.Same, state.Gone)
			}
		}
		folders = append(folders, p)
	}
	return places, folders, nil
}

func (w *Web) adminPage(rw http.ResponseWriter, r *http.Request, user store.User, problem string, status int) {
	ctx := r.Context()
	places, folders, err := w.placesFor(ctx, user)
	if err != nil {
		w.fail(rw, err)
		return
	}
	users, _ := w.Store.Users(ctx)
	type account struct {
		ID, Initial, Name, Email, Location string
		Invite                             string // how they join their phone, to send them
		Came                               string // when they first came in, and how
		Blocked                            bool   // tidoly's login is shut for them
		Tone                               int
	}
	accounts := make([]account, len(users))
	for i, u := range users {
		a := account{ID: u.ID, Name: u.Name, Email: u.Email, Location: u.Location, Tone: tone(u.ID), Invite: w.invite(ctx, u.Email),
			Came: came(u), Blocked: u.Blocked}
		if a.Name == "" {
			a.Name = u.Email
		}
		a.Initial = initial(a.Name)
		if u.ID == user.ID {
			a.Tone = -1
		}
		accounts[i] = a
	}
	data := w.frame(ctx, user, "관리", "admin")
	data["Places"], data["Folders"], data["Users"], data["Problem"] = places, folders, accounts, problem
	data["Outside"], data["VPN"] = w.Outside(ctx), w.vpnState(ctx)
	if arrivals, err := w.Arrivals(ctx); err == nil {
		var names []string
		for _, u := range arrivals {
			names = append(names, u.Name+" ("+u.Email+")")
		}
		data["Arrivals"] = strings.Join(names, ", ")
	}
	data["Moved"] = r.URL.Query().Get("moved") // a place was just added and made the one new photos go to
	if reports, err := w.Store.JobReports(ctx); err == nil {
		if left, now := jobsLeft(reports); left > 0 {
			data["Left"], data["Now"] = countText(int64(left)), now
		}
	}
	if w.Update != nil {
		data["Newer"], data["Updating"], data["ByButton"] = w.Update.Newer(), w.Update.State(), w.Update.ByButton()
	}
	data["Away"], data["AwayFolders"] = !lan.From(r), awayFolders
	data["CouponDays"], data["CouponsOn"], data["Faces"] = w.CouponDays, w.Store.CouponsOn(r.Context()), w.faces(r.Context())
	data["ScreenshotsOn"] = w.ScreenshotsOn != nil && w.ScreenshotsOn(r.Context())
	usable := 0
	for _, p := range places {
		if !p.Missing {
			usable++
		}
	}
	data["CanChoose"] = usable > 1 // with one place for photos there is nothing to choose per person
	rw.WriteHeader(status)
	w.render(rw, "admin", data)
}

// adminAddLocation makes a folder a place for photos.
func (w *Web) adminAddLocation(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	if !lan.From(r) {
		w.adminPage(rw, r, user, awayFolders, http.StatusForbidden)
		return
	}
	dir := strings.TrimSpace(r.FormValue("path"))
	// a new folder to make inside the chosen one: a plain name, never a way out of it
	if name := strings.TrimSpace(r.FormValue("name")); name != "" && dir != "" {
		if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			w.adminPage(rw, r, user, "새 폴더 이름에는 / 와 \\ 를 쓸 수 없습니다: "+name, http.StatusBadRequest)
			return
		}
		dir = filepath.Join(dir, name)
	}
	id, err := w.AddLocation(r.Context(), dir)
	if err != nil {
		log.Printf("web: storage location %q: %v", dir, err)
		problem := "이 폴더는 쓸 수 없습니다. 전체 경로인지, 디스크가 연결되어 있고 쓸 수 있는지 확인해 주세요: " + dir
		if strings.Contains(err.Error(), "same disk as ") {
			problem = "이미 사진을 두는 위치(" + strings.SplitN(err.Error(), "same disk as ", 2)[1] + ")와 같은 디스크입니다 — 위치를 더해도 용량이 늘지 않습니다. 다른 디스크(외장 디스크 등)의 폴더를 골라 주세요."
		}
		w.adminPage(rw, r, user, problem, http.StatusBadRequest)
		return
	}
	// A place is added because the one in use is filling up: new photos go there from now on — someone
	// who adds one expects that (operator, 2026-10-15). People given a place of their own keep it.
	if err := w.Store.SetDefaultLocation(r.Context(), id); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, "/admin?moved="+url.QueryEscape(dir), http.StatusSeeOther)
}

// adminSetLocation says where new photos go: everyone's, or — when the form names a person — that one's.
func (w *Web) adminSetLocation(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	id, who := r.FormValue("location"), r.FormValue("user")
	var err error
	if who != "" {
		if id == "default" {
			id = "" // follow whatever the default is
		}
		err = w.Store.SetUserLocation(r.Context(), who, id)
	} else {
		err = w.Store.SetDefaultLocation(r.Context(), id)
	}
	if err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

type folder struct{ Name, Path string }

// drive is one disk of this machine, where choosing a folder starts.
type drive struct {
	folder
	free int64 // bytes; -1 when not known
}

// drives are the disks of this machine, each with its free space: the drives on Windows; on the Mac the
// startup disk and the disks in /Volumes (not the startup disk's other volumes — system, recovery — nor
// its alias there); on Linux the root and the disks under /mnt and /media. Choosing a folder starts here,
// then goes down into one (operator, 2026-10-15).
func drives() []drive {
	var out []drive
	add := func(dir string) {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return
		}
		d := drive{folder{Name: dir, Path: dir}, -1}
		if _, free, ok := disk.Space(dir); ok {
			d.free = free
			d.Name += " · " + bytesText(free) + " 남음"
		}
		out = append(out, d)
	}
	switch runtime.GOOS {
	case "windows":
		for c := 'A'; c <= 'Z'; c++ { // from A: a B: drive is often a real disk today
			add(string(c) + `:\`)
		}
	case "darwin":
		add("/")
		entries, _ := os.ReadDir("/Volumes")
		for _, e := range entries {
			dir := filepath.Join("/Volumes", e.Name())
			if same, err := disk.Same(dir, "/"); e.Type()&os.ModeSymlink != 0 || err != nil || same {
				continue
			}
			add(dir)
		}
	default:
		add("/")
		mounts, _ := os.ReadFile("/proc/self/mounts")
		for _, dir := range diskMounts(string(mounts)) {
			add(dir)
		}
	}
	return out
}

// diskMounts are the places Linux has mounted a disk other than the root's: from /proc/self/mounts, each
// disk once (where it is mounted nearest the root), disks only — not the system's own (/boot, snaps,
// containers). A second disk is often mounted where its owner chose, /data say, not under /mnt or /media
// (2026-10-18).
func diskMounts(mounts string) []string {
	kinds := map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
		"ntfs": true, "ntfs3": true, "fuseblk": true, "exfat": true, "vfat": true, "hfsplus": true, "jfs": true, "reiserfs": true}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	root := ""
	at := map[string]string{} // disk → where it is mounted nearest the root
	var order []string
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !kinds[f[2]] || (!strings.HasPrefix(f[0], "/dev/") && f[2] != "zfs") {
			continue
		}
		src, dir := f[0], unescape.Replace(f[1])
		if dir == "/" {
			root = src
			continue
		}
		system := false
		for _, p := range []string{"/boot", "/snap", "/var/lib", "/proc", "/sys", "/dev", "/run", "/efi", "/usr", "/opt", "/var/snap"} {
			if dir == p || strings.HasPrefix(dir, p+"/") {
				system = !strings.HasPrefix(dir, "/run/media/")
			}
		}
		if system {
			continue
		}
		if was, ok := at[src]; !ok {
			at[src] = dir
			order = append(order, src)
		} else if len(dir) < len(was) {
			at[src] = dir
		}
	}
	var out []string
	for _, src := range order {
		if src != root {
			out = append(out, at[src])
		}
	}
	return out
}

// Roomiest is the drive with the most free space that photos can be put on, and how much is free; "" when
// none is known. The setup offers it first when it has more room than the home folder's disk.
func Roomiest() (string, int64) {
	best, most := "", int64(-1)
	for _, d := range drives() {
		if d.free > most && disk.Writable(d.Path) {
			best, most = d.Path, d.free
		}
	}
	return best, most
}

// roomy are the drives a place for photos can be added on: writable, with 1 GB free at least — not the
// system-reserved drive nor a full one.
func roomy(all []drive) []folder {
	var out []folder
	for _, d := range all {
		if d.free >= 1<<30 && disk.Writable(d.Path) {
			out = append(out, d.folder)
		}
	}
	return out
}

func folders(all []drive) []folder {
	out := make([]folder, len(all))
	for i, d := range all {
		out[i] = d.folder
	}
	return out
}

// adminFolders lists the folders inside one folder of this machine, for choosing where photos go. The
// browser's own file dialog cannot do this: it shows the machine the browser is on, which may be a phone
// far away, and never tells a page the path. With no folder named it lists where to start from.
func (w *Web) adminFolders(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	if !lan.From(r) {
		http.Error(rw, awayFolders, http.StatusForbidden) // the page hides the buttons; this is the lock
		return
	}
	at := r.URL.Query().Get("at")
	look, err := lookIn(at)
	if err != nil {
		http.Error(rw, "", http.StatusBadRequest)
		return
	}
	data := map[string]any{"At": look.At, "Up": look.Up, "Dirs": look.Dirs, "Free": look.Free, "Crumbs": look.Crumbs, "Sep": string(filepath.Separator), "Me": user.ID,
		"Next": r.URL.Query().Get("next")}
	if r.URL.Query().Get("for") == "import" { // choosing a folder of photos to read, for one person's library
		data["Import"] = true
		data["Users"], _ = w.Store.Users(r.Context())
		data["System"] = look.At != "" && library.SystemFolder(look.At) // AddFolder refuses it: say so before
	} else { // choosing a place for photos: only the other disks give room (AddLocation refuses the rest)
		places := w.photoPlaces(r.Context())
		used := func(dir string) bool {
			for _, p := range places {
				if same, err := disk.Same(dir, p); err == nil && same {
					return true
				}
			}
			return false
		}
		if look.At == "" {
			var free []folder
			for _, v := range roomy(drives()) {
				if !used(v.Path) {
					free = append(free, v)
				}
			}
			data["Dirs"], data["NoVolume"] = free, len(free) == 0
		} else {
			data["Used"] = used(look.At) // reached by going up: say so rather than take it
		}
	}
	w.render(rw, "folders", data)
}

// photoPlaces are the folders photos are put in: the data folder and the places added to it.
func (w *Web) photoPlaces(ctx context.Context) []string {
	places := []string{w.Dir}
	found, _ := w.Store.Locations(ctx)
	for _, l := range found {
		if l.ID != "" && l.Kind == store.KindStore {
			places = append(places, l.Path)
		}
	}
	return places
}

// look is what one folder of this machine holds, for choosing a folder by walking down to it.
type look struct {
	At, Up, Free string
	Dirs, Crumbs []folder
}

// awayFolders is why the folders of this machine are not shown: they are walked only at home.
const awayFolders = "이 PC 의 폴더를 고르는 것은 집 안에서만 됩니다 — 이 PC 나 같은 와이파이의 기기에서 해 주세요."

// lookIn lists the folders inside one folder of this machine; with none named, where to start from: the
// drives, each with its free space.
func lookIn(at string) (look, error) {
	var dirs []folder
	up := ""
	if at == "" {
		dirs = folders(drives())
	} else {
		at = filepath.Clean(at)
		if !filepath.IsAbs(at) {
			return look{}, errors.New("not a full path: " + at)
		}
		entries, _ := os.ReadDir(at) // a folder that cannot be read simply shows nothing inside
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
				continue
			}
			full := filepath.Join(at, name)
			if info, err := os.Stat(full); err == nil && info.IsDir() { // Stat, so that a link to a folder counts
				dirs = append(dirs, folder{Name: name, Path: full})
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
		if len(dirs) > 500 {
			dirs = dirs[:500]
		}
		if parent := filepath.Dir(at); parent != at {
			up = parent
		}
	}
	room := ""
	if _, free, ok := disk.Space(at); ok && at != "" {
		room = bytesText(free)
	}
	// the way here, one step each: this machine, the drive, the folders down to this one
	var crumbs []folder
	for step := at; step != ""; {
		name := filepath.Base(step)
		parent := filepath.Dir(step)
		if parent == step { // the top of a drive or of the tree
			name = strings.TrimRight(step, `\`)
			if name == "" {
				name = step
			}
			crumbs = append([]folder{{Name: name, Path: step}}, crumbs...)
			break
		}
		crumbs = append([]folder{{Name: name, Path: step}}, crumbs...)
		step = parent
	}
	return look{At: at, Up: up, Free: room, Dirs: dirs, Crumbs: crumbs}, nil
}

func exists(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
}

// adminImport takes a folder of photos someone already has into one person's library, where it lies.
func (w *Web) adminImport(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	if !lan.From(r) {
		w.adminPage(rw, r, user, awayFolders, http.StatusForbidden)
		return
	}
	dir, owner := strings.TrimSpace(r.FormValue("path")), r.FormValue("user")
	if owner == "" {
		owner = user.ID
	}
	if _, err := w.AddFolder(r.Context(), dir, owner); err != nil {
		log.Printf("web: folder %q: %v", dir, err)
		problem := "이 폴더는 등록할 수 없습니다. 폴더가 있는지, 이미 쓰고 있는 위치와 겹치지 않는지 확인해 주세요: " + dir
		if strings.Contains(err.Error(), "a system folder") {
			problem = "시스템 · 프로그램 폴더나 그 위(디스크 맨 위 · 홈 폴더)는 등록할 수 없습니다 — 브라우저 캐시 · 아이콘 같은 것까지 사진으로 들어옵니다. 사진이 든 폴더를 골라 주세요: " + dir
		}
		if back(r) == "/start/folders" {
			w.startFoldersPage(rw, r, user, problem, http.StatusBadRequest)
		} else {
			w.adminPage(rw, r, user, problem, http.StatusBadRequest)
		}
		return
	}
	http.Redirect(rw, r, back(r), http.StatusSeeOther)
}

// adminImportAct looks at a registered folder again, or takes it out of the library — its files stay.
func (w *Web) adminImportAct(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	id := r.FormValue("id")
	var err error
	switch {
	case strings.HasSuffix(r.URL.Path, "/scan"):
		w.ScanFolder(id)
	case strings.HasSuffix(r.URL.Path, "/pause"):
		err = w.PauseFolder(r.Context(), id, r.FormValue("on") != "")
	default:
		err = w.RemoveFolder(r.Context(), id)
	}
	if err != nil {
		log.Printf("web: remove folder %s: %v", id, err)
		w.adminPage(rw, r, user, "지금은 할 수 없습니다. 훑는 중이면 끝난 뒤에 다시 해 주세요.", http.StatusConflict)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

// faces is how face grouping stands; "none" when the server was started without it.
func (w *Web) faces(ctx context.Context) string {
	if w.FacesState == nil {
		return "none"
	}
	return w.FacesState(ctx)
}

// adminFaces switches grouping faces into people on or off.
func (w *Web) adminFaces(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.SetFaces == nil || w.faces(r.Context()) == "none" {
		http.NotFound(rw, r)
		return
	}
	if err := w.SetFaces(r.Context(), r.FormValue("on") != ""); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, back(r), http.StatusSeeOther)
}

// adminCoupons switches gathering coupons into an album of their own on or off.
func (w *Web) adminCoupons(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	if err := w.SetCoupons(r.Context(), r.FormValue("on") != ""); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, back(r), http.StatusSeeOther)
}

// adminScreenshots switches gathering screenshots into an album of their own on or off.
func (w *Web) adminScreenshots(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.SetScreenshots == nil {
		http.NotFound(rw, r)
		return
	}
	if err := w.SetScreenshots(r.Context(), r.FormValue("on") != ""); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, back(r), http.StatusSeeOther)
}

// ── accounts the server keeps itself ────────────────────────────────────────────────────────────────────────

// localLogin takes the address and password from the login page.
func (w *Web) localLogin(rw http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	if err := w.LocalLogin(rw, r, email, r.FormValue("password")); err != nil {
		status := http.StatusUnauthorized
		if !lan.From(r) {
			status = http.StatusForbidden
		}
		rw.WriteHeader(status)
		w.render(rw, "login", map[string]any{"Title": "로그인", "Tidoly": w.TidolyHere(r), "Away": !lan.From(r), "Email": email, "Problem": loginProblem(err)})
		return
	}
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func loginProblem(err error) string {
	if strings.Contains(err.Error(), "only at home") {
		return "주소와 비밀번호로 로그인하는 것은 집 안에서만 됩니다 — 이 PC 와 같은 와이파이에서 해 주세요."
	}
	if strings.Contains(err.Error(), "wait") {
		return "여러 번 틀렸습니다. 1분 뒤에 다시 해 주세요."
	}
	return "주소나 비밀번호가 맞지 않습니다."
}

// help is the manual: where the app is, the phone, the folder, accounts, and what to do when it fails.
func (w *Web) help(rw http.ResponseWriter, r *http.Request, user store.User) {
	data := w.frame(r.Context(), user, "도움말", "")
	data["Addr"], data["Data"], data["Invite"] = w.addresses(r.Context()), w.Dir, w.invite(r.Context(), "")
	data["Admin"], data["Faces"], data["Contact"] = user.IsAdmin, w.faces(r.Context()), w.Contact
	data["System"] = runtime.GOOS
	w.render(rw, "help", data)
}

// ownPassword lets a person change their password — one the owner gave them, say.
func (w *Web) ownPassword(rw http.ResponseWriter, r *http.Request, user store.User) {
	data := w.frame(r.Context(), user, "비밀번호 바꾸기", "")
	if r.Method == http.MethodPost {
		next := r.FormValue("password")
		switch {
		case next != r.FormValue("again"):
			data["Problem"] = "새 비밀번호 두 칸이 서로 다릅니다."
		case len(next) < 4:
			data["Problem"] = "새 비밀번호는 네 글자 이상이어야 합니다."
		default:
			if err := w.ChangePassword(r.Context(), user.Email, r.FormValue("current"), next); err != nil {
				data["Problem"] = "지금 비밀번호가 맞지 않습니다. 여러 번 틀리면 1분 기다려야 합니다."
			} else {
				data["Done"] = true
			}
		}
		if data["Problem"] != nil {
			rw.WriteHeader(http.StatusBadRequest)
		}
	}
	w.render(rw, "password", data)
}

// outside is the setup's step for reaching this server from outside the house: through tidoly's tunnel, or
// a WireGuard link of one's own (not built yet). The admin page has the same choices.
// The first steps after the first account: how the pages look, folders of photos already on this PC, what
// the server sorts by itself — each can be done later in the account menu or the admin page.

func (w *Web) startScreen(rw http.ResponseWriter, r *http.Request, user store.User) {
	w.render(rw, "start-screen", map[string]any{"Title": "화면", "Steps": stepsAt(4)})
}

func (w *Web) startFolders(rw http.ResponseWriter, r *http.Request, user store.User) {
	w.startFoldersPage(rw, r, user, "", http.StatusOK)
}

func (w *Web) startFoldersPage(rw http.ResponseWriter, r *http.Request, user store.User, problem string, status int) {
	if !user.IsAdmin {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	_, folders, err := w.placesFor(r.Context(), user)
	if err != nil {
		w.fail(rw, err)
		return
	}
	rw.WriteHeader(status)
	w.render(rw, "start-folders", map[string]any{"Title": "기존 사진", "Steps": stepsAt(5), "Folders": folders, "Problem": problem,
		"Away": !lan.From(r), "AwayFolders": awayFolders})
}

func (w *Web) startAuto(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	w.render(rw, "start-auto", map[string]any{"Title": "자동 정리", "Steps": stepsAt(6),
		"Faces": w.faces(r.Context()), "CouponsOn": w.Store.CouponsOn(r.Context()), "CouponDays": w.CouponDays,
		"ScreenshotsOn": w.ScreenshotsOn != nil && w.ScreenshotsOn(r.Context())})
}

// back is where a switch or a folder chosen on a first step returns to: that step; from the admin page, the
// admin page. Only the first steps are taken — a form must not send anyone elsewhere.
func back(r *http.Request) string {
	switch next := r.FormValue("next"); next {
	case "/start/folders", "/start/auto", "/outside":
		return next
	}
	return "/admin"
}

func (w *Web) outside(rw http.ResponseWriter, r *http.Request, user store.User) {
	w.render(rw, "outside", map[string]any{"Title": "연결 설정", "Steps": stepsAt(7), "Outside": w.Outside(r.Context()), "VPN": w.vpnState(r.Context()), "First": true, "Away": !lan.From(r), "User": user})
}

// welcome is the last step of the setup: how to join the phone, with the address as a QR code.
func (w *Web) welcome(rw http.ResponseWriter, r *http.Request, user store.User) {
	w.render(rw, "welcome", map[string]any{"Title": "폰 연결", "Steps": stepsAt(8), "Addr": w.addresses(r.Context()), "Data": w.Dir, "Invite": w.invite(r.Context(), "")})
}

// events tells an open page, as server-sent events, that this person's photos changed — a phone's upload
// made ready, something trashed or restored elsewhere — so that it fetches what it shows again.
func (w *Web) events(rw http.ResponseWriter, r *http.Request, user store.User) {
	if w.Events == nil {
		http.NotFound(rw, r)
		return
	}
	events, stop := w.Events(user.ID)
	defer stop()
	flush := http.NewResponseController(rw).Flush
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(rw, ": open\n\n")
	flush()
	alive := time.NewTicker(25 * time.Second) // so that nothing on the way closes a quiet line
	defer alive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			fmt.Fprintf(rw, "data: %s\n\n", e)
		case <-alive.C:
			fmt.Fprint(rw, ": alive\n\n")
		}
		if flush() != nil {
			return
		}
	}
}

// phone is where the QR code leads: a phone's camera opens it in the browser, not in the Immich app, so it
// says what to do there — install the app, copy this address into it. No login: it shows the address only.
// addresses are where a phone reaches this server: at home, and — connected to tidoly — anywhere, through
// the tunnel, where one logs in with tidoly rather than the password.
type addresses struct{ Home, Outside string }

func (w *Web) addresses(ctx context.Context) addresses {
	a := addresses{Home: w.Address}
	if w.Outside != nil {
		if host := w.Outside(ctx).Host; host != "" {
			a.Outside = "https://" + host
		}
	}
	return a
}

// invite is the text to send someone to join their phone: through the tunnel with tidoly's login when
// there is one, else at home with the server's own account.
func (w *Web) invite(ctx context.Context, email string) string {
	if a := w.addresses(ctx); a.Outside != "" {
		return install.Invite(a.Outside, "", true)
	}
	return install.Invite(w.Address, email, false)
}

func (w *Web) phone(rw http.ResponseWriter, r *http.Request) {
	w.render(rw, "phone", map[string]any{"Title": "폰 연결", "Addr": w.addresses(r.Context())})
}

func (w *Web) welcomeQR(rw http.ResponseWriter, r *http.Request, user store.User) {
	a := w.addresses(r.Context())
	base := a.Home
	if a.Outside != "" { // the address that works anywhere, where there is one
		base = a.Outside
	}
	png, err := qrcode.Encode(base+"/phone", qrcode.Medium, 400) // a phone's camera opens it in the browser
	if err != nil {
		w.fail(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "image/png")
	rw.Write(png)
}

// setup is the first visit to a server that keeps its own accounts: whoever comes makes the first
// account and owns the server. Once an account exists the page is gone.
func (w *Web) setup(rw http.ResponseWriter, r *http.Request) {
	if !w.NeedsOwner(r.Context()) || !lan.From(r) { // the first account is made at home
		http.Redirect(rw, r, "/login", http.StatusSeeOther)
		return
	}
	data := map[string]any{"Title": "처음 설정", "Name": r.FormValue("name"), "Email": r.FormValue("email"), "Steps": stepsAt(3)}
	if r.Method == http.MethodPost {
		if r.FormValue("password") != r.FormValue("again") {
			data["Problem"] = "비밀번호 두 칸이 서로 다릅니다."
		} else if _, err := w.AddUser(r.Context(), r.FormValue("name"), r.FormValue("email"), r.FormValue("password")); err != nil {
			data["Problem"] = "로그인 주소는 메일 주소여야 하고, 비밀번호는 네 글자 이상이어야 합니다."
		} else {
			if err := w.LocalLogin(rw, r, r.FormValue("email"), r.FormValue("password")); err != nil {
				w.fail(rw, err)
				return
			}
			http.Redirect(rw, r, "/start/screen", http.StatusSeeOther) // then the rest of the first steps, the phone last
			return
		}
		rw.WriteHeader(http.StatusBadRequest)
	}
	w.render(rw, "setup", data)
}

// adminUsers makes an account for someone, or gives someone a new password.
func (w *Web) adminUsers(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	var err error
	if strings.HasSuffix(r.URL.Path, "/password") {
		err = w.SetPassword(r.Context(), r.FormValue("user"), r.FormValue("password"))
	} else {
		_, err = w.AddUser(r.Context(), r.FormValue("name"), r.FormValue("email"), r.FormValue("password"))
	}
	if err != nil {
		problem := "로그인 주소는 메일 주소여야 하고, 비밀번호는 네 글자 이상이어야 합니다."
		if strings.Contains(err.Error(), "already") {
			problem = "그 주소로는 이미 계정이 있습니다."
		}
		w.adminPage(rw, r, user, problem, http.StatusBadRequest)
		return
	}
	http.Redirect(rw, r, "/admin", http.StatusSeeOther)
}

// ── the link to tidoly ──────────────────────────────────────────────────────────────────────────────────────

// came says when a person first came in, and how.
func came(u store.User) string {
	day := func(t string) string {
		if at, err := time.Parse("2006-01-02T15:04:05.000Z", t); err == nil {
			return at.Local().Format("2006-01-02")
		}
		return t
	}
	var parts []string
	if u.HomeAt != "" {
		parts = append(parts, "집 안 "+day(u.HomeAt))
	}
	if u.TidolyAt != "" {
		parts = append(parts, "tidoly "+day(u.TidolyAt))
	}
	if len(parts) == 0 {
		return "아직 들어온 적 없음"
	}
	return "처음 들어옴: " + strings.Join(parts, " · ")
}

// adminConnect sends the owner to tidoly to connect this server. It is done at home: tidoly sends the answer
// back to the address the owner is at, and takes only an address at home — or the server's own outside.
func (w *Web) adminConnect(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	if !lan.From(r) && w.Outside(r.Context()).Host == "" {
		w.adminPage(rw, r, user, "tidoly 연결은 집 안에서 합니다 — 이 PC 와 같은 와이파이에서 열어 주세요.", http.StatusForbidden)
		return
	}
	if back(r) == "/outside" { // started from the first steps: come back there
		http.SetCookie(rw, &http.Cookie{Name: "stow_connect_next", Value: "/outside", Path: "/admin/tidoly", MaxAge: 900, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	w.Connect(rw, r)
}

func (w *Web) adminConnectBack(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	to := "/admin#outside"
	if c, err := r.Cookie("stow_connect_next"); err == nil && c.Value == "/outside" {
		to = "/outside"
		http.SetCookie(rw, &http.Cookie{Name: "stow_connect_next", Path: "/admin/tidoly", MaxAge: -1})
	}
	if err := w.ConnectBack(rw, r); err != nil {
		log.Printf("web: connecting to tidoly: %v", err)
		w.adminPage(rw, r, user, "tidoly 에 연결하지 못했습니다. 다시 해 주세요.", http.StatusBadGateway)
		return
	}
	http.Redirect(rw, r, to, http.StatusSeeOther)
}

// adminTidoly changes what the owner holds: the link itself, whether tidoly's people get in by themselves,
// one person's tidoly login, and having seen who came.
func (w *Web) adminTidoly(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	ctx := r.Context()
	var err error
	switch r.URL.Path {
	case "/admin/tidoly/disconnect":
		err = w.Disconnect(ctx)
	case "/admin/tidoly/auto":
		err = w.SetAutoAdd(ctx, r.FormValue("on") != "")
	case "/admin/tidoly/seen":
		err = w.SeenArrivals(ctx)
	case "/admin/users/block":
		if id := r.FormValue("user"); id == user.ID {
			err = errors.New("one cannot shut one's own login")
		} else {
			err = w.Block(ctx, id, r.FormValue("on") != "")
		}
	}
	if err != nil {
		w.fail(rw, err)
		return
	}
	to := "/admin#outside"
	switch {
	case r.URL.Path == "/admin/tidoly/auto" || r.URL.Path == "/admin/users/block": // beside the users, and on their rows
		to = "/admin#users"
	case back(r) == "/outside": // the first steps' connection page
		to = "/outside"
	}
	http.Redirect(rw, r, to, http.StatusSeeOther)
}
