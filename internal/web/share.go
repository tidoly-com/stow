package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidoly-com/stow/internal/store"
)

// Shared links (#13): an address that shows an album or some photos to someone without an account, while
// the server is reached through tidoly's tunnel. The person who opens it sees the photos by day, each one
// large, and — if the link lets them — takes the originals. The files come from the API's own addresses
// with the link's key (internal/immich, sharedlinks.go).

// Link states, as the server tells them (immich.LinkOpen …).
const (
	linkOpen     = "open"
	linkPassword = "password"
)

// linkDays is how long a link made on the web pages lasts unless the person says otherwise (운영자, 2026-10-17).
const linkDays = 7

const linkLimit = 2000 // photos one link page shows

// LinkFuncs are what the pages need of the server for shared links.
type LinkFuncs struct {
	On      func() bool // the tunnel is on: links work
	Open    func(r *http.Request, key string) (store.SharedLink, string)
	Unlock  func(rw http.ResponseWriter, r *http.Request, l store.SharedLink, password string) error
	New     func(ctx context.Context, owner store.User, albumID string, assetIDs []string, l store.SharedLink, password string) (store.SharedLink, error)
	Address func(l store.SharedLink) string
	Hash    func(password string) string // a link's password, as it is kept
}

func (w *Web) linksOn() bool { return w.Links.On != nil && w.Links.On() }

// shareHeaders: a shared page is for the person given the address — no search engine, no address passed on.
func shareHeaders(rw http.ResponseWriter) {
	rw.Header().Set("X-Robots-Tag", "noindex, nofollow")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Header().Set("Cache-Control", "no-store")
}

// sharePage opens a link: its photos by day, or the box for its password, or word that it is gone.
func (w *Web) sharePage(rw http.ResponseWriter, r *http.Request) {
	shareHeaders(rw)
	if !w.linksOn() {
		w.shareGone(rw)
		return
	}
	key := r.PathValue("key")
	l, state := w.Links.Open(r, key)
	if r.Method == http.MethodPost && state == linkPassword {
		r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
		if err := w.Links.Unlock(rw, r, l, r.PostFormValue("password")); err != nil {
			rw.WriteHeader(http.StatusForbidden)
			w.render(rw, "share-password", map[string]any{"Title": "공유된 사진", "Key": key, "Wrong": true})
			return
		}
		http.Redirect(rw, r, "/share/"+key, http.StatusSeeOther)
		return
	}
	switch state {
	case linkPassword:
		w.render(rw, "share-password", map[string]any{"Title": "공유된 사진", "Key": key})
		return
	case linkOpen:
	default:
		w.shareGone(rw)
		return
	}
	assets, err := w.Store.SharedLinkAssets(r.Context(), l, linkLimit)
	if err != nil {
		w.fail(rw, err)
		return
	}
	owner := ""
	if users, err := w.Store.Users(r.Context()); err == nil {
		for _, u := range users {
			if u.ID == l.OwnerID {
				owner = u.Name
				if owner == "" {
					owner, _, _ = strings.Cut(u.Email, "@")
				}
			}
		}
	}
	list := cells(assets, l.OwnerID, nil)
	for i := range list {
		list[i].Key = l.Key
	}
	title := w.Store.LinkTitle(r.Context(), l)
	w.render(rw, "share", map[string]any{"Title": or(title, "공유된 사진"), "Heading": title, "Owner": owner, "Total": len(assets),
		"Until": untilText(l.ExpiresAt), "Days": days(list, assets, time.Now()), "Key": l.Key, "Download": l.AllowDownload, "Source": SourceURL})
}

// sharePhoto is one photo of a link, large, with the ones before and after it.
func (w *Web) sharePhoto(rw http.ResponseWriter, r *http.Request) {
	shareHeaders(rw)
	key := r.PathValue("key")
	l, state := w.Links.Open(r, key)
	if !w.linksOn() || state != linkOpen {
		if state == linkPassword {
			http.Redirect(rw, r, "/share/"+key, http.StatusSeeOther)
			return
		}
		w.shareGone(rw)
		return
	}
	assets, err := w.Store.SharedLinkAssets(r.Context(), l, linkLimit)
	if err != nil {
		w.fail(rw, err)
		return
	}
	at := -1
	for i, a := range assets {
		if a.ID == r.PathValue("id") {
			at = i
		}
	}
	if at < 0 {
		w.shareGone(rw)
		return
	}
	a := assets[at]
	data := map[string]any{"Title": or(w.Store.LinkTitle(r.Context(), l), "공유된 사진"), "Key": key, "Asset": a, "Video": a.Type == "VIDEO",
		"Download": l.AllowDownload, "Index": at + 1, "Total": len(assets)}
	if at > 0 {
		data["Prev"] = assets[at-1].ID
	}
	if at+1 < len(assets) {
		data["Next"] = assets[at+1].ID
	}
	if l.ShowMetadata {
		data["When"], data["Where"], data["Camera"] = takenText(a), placeText(a), cameraText(a)
	}
	w.render(rw, "share-photo", data)
}

func (w *Web) shareGone(rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusNotFound)
	w.render(rw, "share-gone", map[string]any{"Title": "공유된 사진", "Source": SourceURL})
}

func or(s, otherwise string) string {
	if strings.TrimSpace(s) == "" {
		return otherwise
	}
	return s
}

// untilText says until when a link opens, as a person would: "10월 24일까지"; "" for never.
func untilText(expires string) string {
	t, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return ""
	}
	return t.Local().Format("1월 2일") + "까지"
}

// takenText is when a photo was taken, by the clock where it was taken.
func takenText(a store.Asset) string {
	at := stamp(a)
	if len(at) < 16 {
		return ""
	}
	t, err := time.Parse("2006-01-02T15:04", at[:16])
	if err != nil {
		return ""
	}
	return t.Format("2006년 1월 2일") + " (" + weekdays[t.Weekday()] + ") " + t.Format("15:04")
}

func placeText(a store.Asset) string {
	if m := a.Meta; m != nil {
		var parts []string
		for _, p := range []string{m.Country, m.State, m.City} {
			if p != "" && (len(parts) == 0 || parts[len(parts)-1] != p) {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func cameraText(a store.Asset) string {
	if m := a.Meta; m != nil {
		return strings.TrimSpace(m.Make + " " + m.Model)
	}
	return ""
}

// ── making and ending links, on the web pages ───────────────────────────────────────────────────────────────

// linkMake is POST /web/links: a link of the photos chosen (id) or of an album (album), for linkDays
// unless days says otherwise (0: never). It answers with the address and the link's id.
func (w *Web) linkMake(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !w.linksOn() {
		http.Error(rw, "tidoly 에 연결해야 링크로 공유할 수 있습니다", http.StatusConflict)
		return
	}
	photos := ids(r)
	album := r.PostFormValue("album")
	if album == "" && len(photos) == 0 {
		http.Error(rw, "", http.StatusBadRequest)
		return
	}
	if album != "" {
		photos = nil
	}
	l := store.SharedLink{AllowDownload: true, ShowMetadata: true}
	if d, err := strconv.Atoi(r.PostFormValue("days")); err == nil && d == 0 {
		l.ExpiresAt = ""
	} else {
		if err != nil || d < 0 || d > 3650 {
			d = linkDays
		}
		l.ExpiresAt = store.Timestamp(time.Now().AddDate(0, 0, d))
	}
	l, err := w.Links.New(r.Context(), user, album, photos, l, "")
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(rw, "자신의 사진 · 앨범만 링크로 공유할 수 있습니다", http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("web: link: %v", err)
		http.Error(rw, "", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{"id": l.ID, "url": w.Links.Address(l)})
}

type linkRow struct {
	ID, Address, Title, Until, Created string
	Count                              int
	Album, Password, Gone, Download    bool
}

// linksPage lists the person's links: what each shows, until when, and the way to end it or change it.
func (w *Web) linksPage(rw http.ResponseWriter, r *http.Request, user store.User) {
	links, err := w.Store.SharedLinks(r.Context(), user.ID, "")
	if err != nil {
		w.fail(rw, err)
		return
	}
	now := time.Now()
	rows := make([]linkRow, len(links))
	for i, l := range links {
		n, _ := w.Store.SharedLinkAssets(r.Context(), l, linkLimit)
		rows[i] = linkRow{ID: l.ID, Title: or(w.Store.LinkTitle(r.Context(), l), "사진 묶음"), Until: untilText(l.ExpiresAt), Count: len(n),
			Album: l.AlbumID != "", Password: l.Password != "", Gone: l.Expired(now), Download: l.AllowDownload}
		if w.linksOn() {
			rows[i].Address = w.Links.Address(l)
		}
		if t, err := time.Parse(time.RFC3339Nano, l.CreatedAt); err == nil {
			rows[i].Created = t.Local().Format("2006. 1. 2.")
		}
	}
	data := w.frame(r.Context(), user, "공유 링크", "links")
	data["Links"], data["On"], data["Made"] = rows, w.linksOn(), r.URL.Query().Get("made")
	w.render(rw, "links", data)
}

// linkChange ends a link (/links/{id}/delete) or changes it: its end, whether originals may be taken, its
// password (an empty box keeps the one there; "remove" takes it away).
func (w *Web) linkChange(rw http.ResponseWriter, r *http.Request, user store.User) {
	l, err := w.Store.SharedLink(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		w.fail(rw, err)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 16<<10)
	if strings.HasSuffix(r.URL.Path, "/delete") {
		if err := w.Store.DeleteSharedLink(r.Context(), user.ID, l.ID); err != nil {
			w.fail(rw, err)
			return
		}
		http.Redirect(rw, r, "/links", http.StatusSeeOther)
		return
	}
	switch d, err := strconv.Atoi(r.PostFormValue("days")); {
	case err != nil:
	case d == 0:
		l.ExpiresAt = ""
	case d > 0 && d <= 3650:
		l.ExpiresAt = store.Timestamp(time.Now().AddDate(0, 0, d))
	}
	l.AllowDownload = r.PostFormValue("download") != ""
	if r.PostFormValue("remove") != "" {
		l.Password = ""
	} else if p := r.PostFormValue("password"); p != "" {
		l.Password = w.Links.Hash(p)
	}
	if err := w.Store.UpdateSharedLink(r.Context(), l); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, "/links#"+l.ID, http.StatusSeeOther)
}
