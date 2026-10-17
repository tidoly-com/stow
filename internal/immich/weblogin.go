package immich

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

// On https the cookies carry the __Host- prefix: a browser then refuses any cookie of that name set by
// a sibling site under the same registrable domain. The prefix needs Secure, so plain http at home goes
// without it.
func (s *Server) cookieName(r *http.Request, name string) string {
	if s.secure(r) {
		return "__Host-" + name
	}
	return name
}

func (s *Server) webCookie(r *http.Request, name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: s.cookieName(r, name), Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode}
}

// WebLoginStart sends the browser to home to sign in.
func (s *Server) WebLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.TidolyHere(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	state, verifier := newToken(), newToken()
	sum := sha256.Sum256([]byte(verifier))
	http.SetCookie(w, s.webCookie(r, "stow_login", state+"."+verifier, 600))
	q := url.Values{"slug": {s.Link().Slug}, "redirect": {s.mobileRedirect()}, "state": {state},
		"cc": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	http.Redirect(w, r, s.cfg.HomeURL+"/stow/login?"+q.Encode(), http.StatusSeeOther)
}

// finishWebLogin completes a browser login if this request is the end of one; false means it is not.
func (s *Server) finishWebLogin(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(s.cookieName(r, "stow_login"))
	if err != nil {
		return false
	}
	state, verifier, _ := strings.Cut(c.Value, ".")
	if state == "" || state != r.URL.Query().Get("state") {
		return false
	}
	http.SetCookie(w, s.webCookie(r, "stow_login", "", -1))
	user, token, err := s.ticketLogin(r.Context(), r.URL.Query().Get("code"), verifier)
	if err != nil {
		log.Printf("web login refused: %v", err)
		why := "1"
		switch {
		case errors.Is(err, errNoAccount):
			why = "account"
		case errors.Is(err, errBlocked):
			why = "blocked"
		}
		http.Redirect(w, r, "/login?refused="+why, http.StatusSeeOther)
		return true
	}
	log.Printf("ticket accepted (browser): %s", user.Email)
	http.SetCookie(w, s.webCookie(r, "stow_session", token, 400*24*3600))
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return true
}

// WebLogout ends the browser's session.
func (s *Server) WebLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.requestToken(r); token != "" {
		s.store.DeleteSession(r.Context(), token)
	}
	http.SetCookie(w, s.webCookie(r, "stow_session", "", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Changed tells a person's connected devices that something of theirs changed; the app answers with a sync.
func (s *Server) Changed(userID string) { s.hub.emit(userID, "on_asset_update", map[string]any{}) }

// SessionUser tells the web pages who is asking, if anyone.
func (s *Server) SessionUser(r *http.Request) (store.User, bool) {
	token := s.requestToken(r)
	if token == "" {
		return store.User{}, false
	}
	user, err := s.store.SessionUser(r.Context(), token)
	return user, err == nil
}
