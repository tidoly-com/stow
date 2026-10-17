package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/tidoly-com/stow/internal/lan"
	"github.com/tidoly-com/stow/internal/store"
)

// Connecting to tidoly from the app's window happens in the browser: tidoly's sign-in belongs there, not in
// a window of its own. The browser is not logged in here, so the window asks for a link good once, for a
// few minutes, that starts the connection in whatever opens it; tidoly's answer is then taken by the
// browser that started — the PKCE cookie proves it — and the window waits until the link is made.

const connectLinkFor = 5 * time.Minute

func (w *Web) newConnectLink() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	w.linksMu.Lock()
	defer w.linksMu.Unlock()
	if w.links == nil {
		w.links = map[string]time.Time{}
	}
	for t, until := range w.links {
		if time.Now().After(until) {
			delete(w.links, t)
		}
	}
	w.links[token] = time.Now().Add(connectLinkFor)
	return token, nil
}

// takeConnectLink spends a link: true once, while it is good.
func (w *Web) takeConnectLink(token string) bool {
	w.linksMu.Lock()
	defer w.linksMu.Unlock()
	until, ok := w.links[token]
	delete(w.links, token)
	return ok && token != "" && time.Now().Before(until)
}

// connectLink gives the app's window a link to start connecting in the browser.
func (w *Web) connectLink(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || !lan.From(r) {
		http.NotFound(rw, r)
		return
	}
	token, err := w.newConnectLink()
	if err != nil {
		w.fail(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{"url": "/admin/tidoly/connect?ot=" + token})
}

// connectState tells the waiting window whether the link to tidoly is made.
func (w *Web) connectState(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]bool{"connected": w.Outside(r.Context()).Host != ""})
}

const byBrowser = "stow_connect_by" // set on a connection a link started, for the way back

// startConnect starts connecting: for the admin logged in here, or for a browser with a link.
func (w *Web) startConnect(rw http.ResponseWriter, r *http.Request) {
	if user, ok := w.User(r); ok && user.IsAdmin {
		w.adminConnect(rw, r, user)
		return
	}
	if !lan.From(r) || !w.takeConnectLink(r.FormValue("ot")) {
		http.Redirect(rw, r, "/login", http.StatusSeeOther)
		return
	}
	http.SetCookie(rw, &http.Cookie{Name: byBrowser, Value: "1", Path: "/admin/tidoly", MaxAge: 900, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Connect(rw, r)
}

// finishConnect takes tidoly's answer: in the admin's browser as before, or in a browser a link started,
// which is told to go back to the app's window.
func (w *Web) finishConnect(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(byBrowser); err == nil && c.Value == "1" {
		http.SetCookie(rw, &http.Cookie{Name: byBrowser, Path: "/admin/tidoly", MaxAge: -1})
		data := map[string]any{"Title": "tidoly 연결"}
		if err := w.ConnectBack(rw, r); err != nil {
			log.Printf("web: connecting to tidoly from the browser: %v", err)
			data["Problem"] = "tidoly 에 연결하지 못했습니다. tidoly stow 창에서 다시 해 주세요."
			rw.WriteHeader(http.StatusBadGateway)
		}
		w.render(rw, "connected-elsewhere", data)
		return
	}
	if user, ok := w.User(r); ok {
		w.adminConnectBack(rw, r, user)
		return
	}
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}
