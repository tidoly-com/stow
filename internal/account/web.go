package account

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidoly-com/stow/internal/store"
)

// A browser logs in with the same ticket as the phone app (stow issue #3), with this server playing the
// part the app plays there: it makes the PKCE pair, keeps the verifier in a short-lived cookie, and
// finishes the login when home sends the browser back. Home sends every ticket for this slug to one
// address, /api/oauth/mobile-redirect; a browser that carries the login cookie is finished there, and
// anything else is handed on to the phone app.

// CookieName is a cookie's name as this request is to see it. On https the cookies carry the __Host-
// prefix: a browser then refuses any cookie of that name set by a sibling site under the same registrable
// domain. The prefix needs Secure, so plain http at home goes without it.
func (a *Accounts) CookieName(r *http.Request, name string) string {
	if a.Secure(r) {
		return "__Host-" + name
	}
	return name
}

func (a *Accounts) webCookie(r *http.Request, name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: a.CookieName(r, name), Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: a.Secure(r), SameSite: http.SameSiteLaxMode}
}

// WebLoginStart sends the browser to home to sign in.
func (a *Accounts) WebLoginStart(w http.ResponseWriter, r *http.Request) {
	if !a.TidolyHere(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	state, verifier := NewToken(), NewToken()
	sum := sha256.Sum256([]byte(verifier))
	http.SetCookie(w, a.webCookie(r, "stow_login", state+"."+verifier, 600))
	q := url.Values{"slug": {a.Link().Slug}, "redirect": {a.MobileRedirect()}, "state": {state},
		"cc": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	http.Redirect(w, r, a.cfg.HomeURL+"/stow/login?"+q.Encode(), http.StatusSeeOther)
}

// FinishWebLogin completes a browser login if this request is the end of one; false means it is not.
func (a *Accounts) FinishWebLogin(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(a.CookieName(r, "stow_login"))
	if err != nil {
		return false
	}
	state, verifier, _ := strings.Cut(c.Value, ".")
	if state == "" || state != r.URL.Query().Get("state") {
		return false
	}
	http.SetCookie(w, a.webCookie(r, "stow_login", "", -1))
	user, token, err := a.TicketLogin(r.Context(), r.URL.Query().Get("code"), verifier)
	if err != nil {
		log.Printf("web login refused: %v", err)
		why := "1"
		switch {
		case errors.Is(err, ErrNoAccount):
			why = "account"
		case errors.Is(err, ErrBlocked):
			why = "blocked"
		}
		http.Redirect(w, r, "/login?refused="+why, http.StatusSeeOther)
		return true
	}
	log.Printf("ticket accepted (browser): %s", user.Email)
	http.SetCookie(w, a.webCookie(r, "stow_session", token, 400*24*3600))
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return true
}

// WebLogout ends the browser's session.
func (a *Accounts) WebLogout(w http.ResponseWriter, r *http.Request) {
	if token := a.RequestToken(r); token != "" {
		a.store.DeleteSession(r.Context(), token)
	}
	http.SetCookie(w, a.webCookie(r, "stow_session", "", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// SessionUser tells the web pages who is asking, if anyone.
func (a *Accounts) SessionUser(r *http.Request) (store.User, bool) {
	token := a.RequestToken(r)
	if token == "" {
		return store.User{}, false
	}
	user, err := a.store.SessionUser(r.Context(), token)
	return user, err == nil
}
