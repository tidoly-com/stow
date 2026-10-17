package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tidoly-com/stow/internal/store"
)

// The owner who forgot the password and has no other way in — not connected to tidoly, the only owner — is
// whoever sits at this PC: the app's menu (or, without one, `tidoly-light -reset-owner`) writes a link good
// once, for a few minutes, that only a browser on this PC opens.

// ResetFor is how long a link to set the owner's password again stays good.
const ResetFor = 10 * time.Minute

// MakeOwnerReset writes a new link's secret to file, readable by this user only, and returns it.
func MakeOwnerReset(file string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	body := fmt.Sprintf("%s\n%d\n", token, time.Now().Add(ResetFor).Unix())
	return token, os.WriteFile(file, []byte(body), 0o600)
}

// ownerResetGood says whether the link's secret is the one written, and still good.
func (w *Web) ownerResetGood(token string) bool {
	if w.ResetFile == "" || token == "" {
		return false
	}
	raw, err := os.ReadFile(w.ResetFile)
	if err != nil {
		return false
	}
	want, until, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	end, err := strconv.ParseInt(strings.TrimSpace(until), 10, 64)
	if err != nil || time.Now().Unix() > end {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// onThisPC says whether a request came from a browser on this machine, sent to this machine's own address.
// Both: the tunnel's client also hands requests over from 127.0.0.1, but those were sent to the domain.
func onThisPC(r *http.Request) bool {
	from, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(from).IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

// ownerReset sets the owner's password again, from the link the app's menu opened.
func (w *Web) ownerReset(rw http.ResponseWriter, r *http.Request) {
	token := r.FormValue("t")
	if !onThisPC(r) || !w.ownerResetGood(token) {
		http.NotFound(rw, r)
		return
	}
	owner, ok := w.owner(r)
	if !ok {
		http.NotFound(rw, r)
		return
	}
	data := map[string]any{"Title": "관리자 비밀번호 변경", "Token": token, "Email": owner.Email}
	if r.Method == http.MethodPost {
		password := r.FormValue("password")
		switch {
		case password != r.FormValue("again"):
			data["Problem"] = "비밀번호 두 칸이 서로 다릅니다."
		case w.SetPassword(r.Context(), owner.ID, password) != nil:
			data["Problem"] = "비밀번호는 네 글자 이상이어야 합니다."
		default:
			os.Remove(w.ResetFile) // good once
			if err := w.LocalLogin(rw, r, owner.Email, password); err != nil {
				w.fail(rw, err)
				return
			}
			http.Redirect(rw, r, "/", http.StatusSeeOther)
			return
		}
		rw.WriteHeader(http.StatusBadRequest)
	}
	w.render(rw, "owner-reset", data)
}

// owner is the server's owner: the first account that runs it.
func (w *Web) owner(r *http.Request) (store.User, bool) {
	users, err := w.Store.Users(r.Context())
	if err != nil {
		return store.User{}, false
	}
	for _, u := range users {
		if u.IsAdmin {
			return u, true
		}
	}
	return store.User{}, false
}
