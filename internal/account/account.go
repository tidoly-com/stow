// Package account is who comes in: the server's own accounts and their passwords, the sessions a login
// opens, and the link to tidoly — its login by tickets, the people home lists, the key the tunnel and the
// certificate are asked for with. It knows nothing of photos: every build stands on it, the photo server
// (internal/immich) as much as one with no photos at all (#47).
package account

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/tidoly-com/stow/internal/store"
)

type Config struct {
	PublicURL string                 // how a phone or a browser at home reaches this server
	HomeURL   string                 // where tidoly-home is, as a browser reaches it
	HomeKey   ed25519.PublicKey      // home's key for tickets; without it nobody logs in with tidoly
	Link      Link                   // a link to tidoly given at start, for trials; otherwise the one the owner made is read
	Linked    func(Link)             // told when the owner connects to tidoly or disconnects: the tunnel follows
	HTTPS     func(host string) bool // whether this server has its own certificate for its address outside; nil: never
	// Token is a session token a request carries in a way of its own — the Immich app's cookie — tried
	// before the ways every build knows; nil: none
	Token func(r *http.Request) string
}

type Accounts struct {
	cfg   Config
	store *store.Store

	link        atomic.Pointer[Link] // this server's link to tidoly
	linkProblem atomic.Value         // string: what went wrong with it lately
}

func New(cfg Config, st *store.Store) *Accounts {
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	cfg.HomeURL = strings.TrimRight(cfg.HomeURL, "/")
	a := &Accounts{cfg: cfg, store: st}
	a.loadLink()
	return a
}

// Change changes the config after the start: for tests, which start a server before they know where their
// tidoly-home is.
func (a *Accounts) Change(change func(*Config)) {
	change(&a.cfg)
	a.cfg.HomeURL = strings.TrimRight(a.cfg.HomeURL, "/")
}

// HomeURL is where tidoly-home is.
func (a *Accounts) HomeURL() string { return a.cfg.HomeURL }

// RequestToken is the session token a request carries, "" for none.
func (a *Accounts) RequestToken(r *http.Request) string {
	if a.cfg.Token != nil {
		if token := a.cfg.Token(r); token != "" {
			return token
		}
	}
	if c, err := r.Cookie(a.CookieName(r, "stow_session")); err == nil && c.Value != "" { // a browser on the web pages
		return c.Value
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return bearer
	}
	if key := r.URL.Query().Get("sessionKey"); key != "" && r.Method == "GET" { // a device that can only be given an address
		return key
	}
	return r.Header.Get("x-immich-user-token")
}

// ToHTTPS sends plain http that came through the tunnel to https, once this server has its certificate:
// true when it did, and the request is answered.
func (a *Accounts) ToHTTPS(w http.ResponseWriter, r *http.Request) bool {
	host := a.Link().Host
	if r.TLS != nil || host == "" || !strings.EqualFold(r.Host, host) || a.cfg.HTTPS == nil || !a.cfg.HTTPS(host) {
		return false
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	return true
}

var ErrAlreadyUsed = errors.New("already used")

// NewToken is a fresh secret: a session's, a shared link's.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
