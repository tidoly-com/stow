// Package immich answers the part of the Immich API that the Immich phone app uses to log in, back up and
// browse (stow issues #2 and #5). Anything else under /api answers 404 and is logged, so the log shows what
// the app asked for that this server does not have yet.
package immich

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidoly-com/stow/internal/coupons"
	"github.com/tidoly-com/stow/internal/disk"
	"github.com/tidoly-com/stow/internal/faces"
	"github.com/tidoly-com/stow/internal/jobs"
	"github.com/tidoly-com/stow/internal/lan"
	"github.com/tidoly-com/stow/internal/library"
	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/thumbs"
)

// The fixtures were extracted from the Immich server's OpenAPI spec (AGPL-3.0); assets/licenses/immich-fixtures.txt.
//
//go:embed fixtures/*.json
var fixtures embed.FS

// fixture returns a fresh copy of a response body shaped after the Immich spec, for the caller to fill in.
func fixture(name string) map[string]any {
	raw, err := fixtures.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		panic(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		panic(err)
	}
	return body
}

type Config struct {
	Name      string                 // what this server is called on other devices: its computer's name
	PublicURL string                 // how the phone reaches this server at home
	HomeURL   string                 // where tidoly-home is, as the phone's browser reaches it
	HomeKey   ed25519.PublicKey      // home's key for tickets; without it nobody logs in with tidoly
	Link      Link                   // a link to tidoly given at start, for trials; otherwise the one the owner made is read
	Linked    func(Link)             // told when the owner connects to tidoly or disconnects: the tunnel follows
	HTTPS     func(host string) bool // whether this server has its own certificate for its address outside; nil: never
	JobKinds  []string               // work queued for every new asset
	Thumbs    *thumbs.Maker

	Faces         *faces.Helper // nil: this server does not look for faces
	FaceThreshold float64       // how alike two faces must be to count as one person

	Coupons *coupons.Helper // reads the words of a coupon, for its last day (macOS); nil: coupons go in without one
}

type Server struct {
	cfg    Config
	store  *store.Store
	lib    *library.Library
	runner *jobs.Runner
	mux    *http.ServeMux
	hub    hub
	warned sync.Map // user id -> told that location tags look off
	scans  sync.Map // location id -> *Scan, the latest look at a read-only folder
	faceMu sync.Mutex

	faceHelper  atomic.Pointer[faces.Helper] // set once the models are there; Config.Faces starts with it
	faceWaiting atomic.Bool                  // the models are being fetched

	link        atomic.Pointer[Link] // this server's link to tidoly
	linkProblem atomic.Value         // string: what went wrong with it lately
}

type session struct {
	user  store.User
	token string
}

type ctxKey struct{}

func New(cfg Config, st *store.Store, lib *library.Library, runner *jobs.Runner) *Server {
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	cfg.HomeURL = strings.TrimRight(cfg.HomeURL, "/")
	s := &Server{cfg: cfg, store: st, lib: lib, runner: runner, mux: http.NewServeMux()}
	if cfg.Faces != nil {
		s.faceHelper.Store(cfg.Faces)
	}
	s.loadLink(context.Background())
	m := s.mux

	m.HandleFunc("GET /.well-known/immich", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"api": map[string]any{"endpoint": "/api"}})
	})
	m.HandleFunc("GET /api/server/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"res": "pong"})
	})
	// An Immich server answers the ping too: this says it is tidoly stow, to a PC set up to show this one.
	m.HandleFunc("GET /stow/about", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"app": "tidoly stow", "name": s.cfg.Name})
	})
	m.HandleFunc("GET /api/server/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"major": 3, "minor": 3, "patch": 0}) // the API version this subset follows
	})
	m.HandleFunc("GET /api/server/features", s.features)
	m.HandleFunc("GET /api/server/config", s.config)

	m.HandleFunc("POST /api/auth/login", s.passwordLogin)
	m.HandleFunc("POST /api/oauth/authorize", s.oauthAuthorize)
	m.HandleFunc("GET /api/oauth/mobile-redirect", s.oauthMobileRedirect)
	m.HandleFunc("POST /api/oauth/callback", s.oauthCallback)

	m.HandleFunc("POST /api/auth/validateToken", s.authed(func(w http.ResponseWriter, r *http.Request, _ session) {
		writeJSON(w, 200, map[string]any{"authStatus": true})
	}))
	m.HandleFunc("POST /api/auth/logout", s.authed(s.logout))
	m.HandleFunc("POST /api/sessions", s.authed(s.lendSession))
	m.HandleFunc("GET /api/users/me", s.authed(s.me))
	m.HandleFunc("GET /api/users/me/preferences", s.authed(func(w http.ResponseWriter, r *http.Request, _ session) {
		writeJSON(w, 200, fixture("preferences"))
	}))
	m.HandleFunc("GET /api/server/storage", s.authed(s.storage))
	m.HandleFunc("POST /api/sync/stream", s.authed(s.syncStream))
	m.HandleFunc("POST /api/sync/ack", s.authed(s.syncAck))
	m.HandleFunc("POST /api/assets", s.authed(s.upload))
	m.HandleFunc("DELETE /api/assets", s.authed(s.deleteAssets))
	m.HandleFunc("PUT /api/assets", s.authed(s.updateAssets))
	m.HandleFunc("POST /api/trash/restore/assets", s.authed(s.trashRestore(false)))
	m.HandleFunc("POST /api/trash/restore", s.authed(s.trashRestore(true)))
	m.HandleFunc("POST /api/trash/empty", s.authed(s.trashEmpty))
	m.HandleFunc("GET /api/assets/{id}", s.authed(s.assetInfo))
	m.HandleFunc("GET /api/assets/{id}/thumbnail", s.authed(s.serveThumbnail))
	m.HandleFunc("GET /api/assets/{id}/original", s.authed(s.serveOriginal))
	m.HandleFunc("GET /api/assets/{id}/video/playback", s.authed(s.serveOriginal))
	m.HandleFunc("GET /api/socket.io/", s.authed(s.socket))
	m.HandleFunc("GET /api/people/{id}/thumbnail", s.authed(s.servePersonPicture))
	m.HandleFunc("PUT /api/people/{id}", s.authed(s.updatePerson))
	m.HandleFunc("GET /api/albums", s.authed(s.albumList))
	m.HandleFunc("POST /api/albums", s.authed(s.albumCreate))
	m.HandleFunc("PUT /api/albums/assets", s.authed(s.albumsAddAssets))
	m.HandleFunc("GET /api/albums/{id}", s.authed(s.albumGet))
	m.HandleFunc("PATCH /api/albums/{id}", s.authed(s.albumUpdate))
	m.HandleFunc("DELETE /api/albums/{id}", s.authed(s.albumDelete))
	m.HandleFunc("PUT /api/albums/{id}/assets", s.authed(s.albumAssets(true)))
	m.HandleFunc("DELETE /api/albums/{id}/assets", s.authed(s.albumAssets(false)))

	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "Not found")
	})
	return s
}

// Pages puts the web pages beside the API: everything that is not /api or /.well-known goes to them.
func (s *Server) Pages(h http.Handler) { s.mux.Handle("/", h) }

// ServeHTTP logs one line per request: method, path, status, time. Never the query — it can carry a ticket.
// Plain http that came through the tunnel is sent to https, once this server has its certificate.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	if host := s.Link().Host; r.TLS == nil && host != "" && strings.EqualFold(r.Host, host) && s.cfg.HTTPS != nil && s.cfg.HTTPS(host) {
		http.Redirect(rec, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	} else {
		s.mux.ServeHTTP(rec, r)
	}
	log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets the websocket upgrade reach the connection underneath.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message, "statusCode": status})
}

// ── sessions ────────────────────────────────────────────────────────────────────────────────────────────────

// The Immich app keeps no token of its own: it sends back the cookies the login answer set (measured
// 2026-10-13). The names are the app's, so they cannot carry the __Host- prefix.
const (
	cookieToken  = "immich_access_token"
	cookieType   = "immich_auth_type"
	cookieIsAuth = "immich_is_authenticated"
)

func (s *Server) requestToken(r *http.Request) string {
	if c, err := r.Cookie(cookieToken); err == nil && c.Value != "" {
		return c.Value
	}
	if c, err := r.Cookie(s.cookieName(r, "stow_session")); err == nil && c.Value != "" { // a browser on the web pages
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

func (s *Server) authed(next func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := s.requestToken(r)
		if token == "" {
			writeError(w, 401, "Authentication required")
			return
		}
		user, err := s.store.SessionUser(r.Context(), token)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 401, "Invalid user token")
			return
		}
		if r.Context().Err() != nil {
			return // the browser went elsewhere before we answered; nobody is listening
		}
		if err != nil {
			log.Printf("auth: %v", err)
			writeError(w, 500, "Internal error")
			return
		}
		next(w, r, session{user: user, token: token})
	}
}

// setAuthCookies sets the cookies the app keeps; kind is how it logged in, "password" or "oauth".
func (s *Server) setAuthCookies(w http.ResponseWriter, r *http.Request, token, kind string) {
	maxAge := 400 * 24 * 3600
	if token == "" {
		maxAge = -1
	}
	secure := s.secure(r)
	for _, c := range []struct {
		name, value string
		httpOnly    bool
	}{{cookieToken, token, true}, {cookieType, kind, true}, {cookieIsAuth, "true", false}} {
		http.SetCookie(w, &http.Cookie{Name: c.name, Value: c.value, Path: "/", MaxAge: maxAge,
			HttpOnly: c.httpOnly, Secure: secure, SameSite: http.SameSiteLaxMode})
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.store.DeleteSession(r.Context(), sess.token); err != nil {
		log.Printf("logout: %v", err)
	}
	s.setAuthCookies(w, r, "", "")
	body := fixture("logout")
	body["successful"], body["redirectUri"] = true, "/auth/login"
	writeJSON(w, 200, body)
}

// lendSession is POST /api/sessions: the app asks for a second, short-lived session to hand to another
// device — a television it casts to, which then fetches the pictures itself with ?sessionKey=.
// It can do no more than its owner, reads only, and ends by itself.
func (s *Server) lendSession(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Duration             int
		DeviceOS, DeviceType string
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
	if req.Duration <= 0 || req.Duration > 24*3600 {
		req.Duration = 24 * 3600
	}
	token, now := newToken(), time.Now()
	until := now.Add(time.Duration(req.Duration) * time.Second)
	if err := s.store.CreateTimedSession(r.Context(), token, sess.user.ID, until); err != nil {
		log.Printf("sessions: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	writeJSON(w, 201, map[string]any{"id": store.NewID(), "token": token, "createdAt": store.Timestamp(now), "updatedAt": store.Timestamp(now),
		"expiresAt": store.Timestamp(until), "current": false, "deviceOS": req.DeviceOS, "deviceType": req.DeviceType,
		"isPendingSyncReset": false, "appVersion": ""})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, sess session) {
	u := sess.user
	body := fixture("user")
	body["id"], body["email"], body["name"], body["isAdmin"] = u.ID, u.Email, u.Name, u.IsAdmin
	body["oauthId"], body["createdAt"], body["updatedAt"], body["profileChangedAt"] = u.Sub, u.CreatedAt, u.CreatedAt, u.CreatedAt
	writeJSON(w, 200, body)
}

// ── server info ─────────────────────────────────────────────────────────────────────────────────────────────

func (s *Server) features(w http.ResponseWriter, r *http.Request) {
	body := fixture("features")                                    // every feature off, then the two this server has
	body["oauth"], body["passwordLogin"] = s.tidoly(), lan.From(r) // own accounts log in only at home
	body["facialRecognition"], body["reverseGeocoding"] = s.faces(r.Context()) != nil, true
	body["trash"] = true // so that the app's "delete" asks for the trash, not for deleting at once
	writeJSON(w, 200, body)
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	body := fixture("config")
	body["isInitialized"], body["isOnboarded"] = true, true
	body["oauthButtonText"], body["externalDomain"] = "Login with tidoly", s.loginBase()
	body["trashDays"] = TrashDays
	writeJSON(w, 200, body)
}

// ── login through a ticket signed by home ───────────────────────────────────────────────────────────────────

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State         string `json:"state"`
		CodeChallenge string `json:"codeChallenge"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil || req.CodeChallenge == "" {
		writeError(w, 400, "state and codeChallenge are required")
		return
	}
	if !s.tidoly() {
		writeError(w, 400, "This server is not connected to tidoly")
		return
	}
	q := url.Values{"slug": {s.Link().Slug}, "redirect": {s.mobileRedirect()}, "state": {req.State}, "cc": {req.CodeChallenge}}
	writeJSON(w, 201, map[string]any{"url": s.cfg.HomeURL + "/stow/login?" + q.Encode()})
}

// oauthMobileRedirect hands the ticket to the app: home can only send a browser to an http(s) address.
func (s *Server) oauthMobileRedirect(w http.ResponseWriter, r *http.Request) {
	if s.finishWebLogin(w, r) {
		return
	}
	w.Header().Set("Location", "app.immich:///oauth-callback?"+r.URL.RawQuery)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL          string `json:"url"`
		CodeVerifier string `json:"codeVerifier"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	back, err := url.Parse(req.URL)
	if err != nil || back.Query().Get("code") == "" {
		writeError(w, 401, "No ticket in the callback address")
		return
	}
	user, token, err := s.ticketLogin(r.Context(), back.Query().Get("code"), req.CodeVerifier)
	switch {
	case errors.Is(err, errNoAccount):
		writeError(w, 403, "This Google account has no account on this server. Ask its owner to add it")
		return
	case errors.Is(err, errBlocked):
		writeError(w, 403, "The owner of this server has shut login with tidoly for this account")
		return
	case err != nil:
		log.Printf("ticket refused: %v", err)
		writeError(w, 401, "Ticket refused: "+err.Error())
		return
	}
	log.Printf("ticket accepted: %s", user.Email)
	s.setAuthCookies(w, r, token, "oauth")
	body := fixture("login")
	body["accessToken"], body["userId"], body["userEmail"], body["name"] = token, user.ID, user.Email, user.Name
	body["isAdmin"], body["isOnboarded"] = user.IsAdmin, true
	writeJSON(w, 201, body)
}

var errAlreadyUsed = errors.New("already used")

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// storage is the disk the person's new photos go to: how large, how much used, how much left — what the
// app shows under its settings.
func (s *Server) storage(w http.ResponseWriter, r *http.Request, sess session) {
	body := fixture("storage")
	location, err := s.store.LocationFor(r.Context(), sess.user.ID)
	if err == nil {
		if total, free, ok := disk.Space(s.lib.Dir(location)); ok && total > 0 {
			used := total - free
			body["diskSize"], body["diskSizeRaw"] = humanBytes(total), total
			body["diskUse"], body["diskUseRaw"] = humanBytes(used), used
			body["diskAvailable"], body["diskAvailableRaw"] = humanBytes(free), free
			body["diskUsagePercentage"] = math.Round(float64(used)*10000/float64(total)) / 100
		}
	}
	writeJSON(w, 200, body)
}

// humanBytes writes a size as Immich's server does: binary units, one decimal ("465.6 GiB").
func humanBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
