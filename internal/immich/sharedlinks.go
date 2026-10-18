package immich

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tidoly-com/stow/internal/store"
)

// Shared links: an address that shows an album, or some photos, to someone without an account — what the
// Immich app makes from an album's or a choice's "share link", and the web pages too. Only while this
// server is reached through tidoly's tunnel: a link is for people outside the house, and over WireGuard
// alone their devices cannot open it (CLAUDE.md, 2026-10-15). The app builds the address from the
// server's externalDomain, which is the tunnel's — "<domain>/share/<key>".

// passwordShown stands for a link's password in what the app is told: the password itself is kept only
// as a hash. The app sends it back unchanged when the password is not edited.
const passwordShown = "********"

// LinksOn says whether shared links work here now.
func (s *Server) LinksOn() bool { return s.tidoly() && s.Link().Host != "" }

// LinkAddress is the address a link opens at, outside.
func (s *Server) LinkAddress(l store.SharedLink) string {
	return "https://" + s.Link().Host + "/share/" + l.Key
}

func (s *Server) linkBody(ctx context.Context, l store.SharedLink, owner store.User) map[string]any {
	kind, assets, album := "INDIVIDUAL", []any{}, any(nil)
	if l.AlbumID != "" {
		kind = "ALBUM"
		if b, err := s.store.Album(ctx, owner.ID, l.AlbumID); err == nil {
			album = albumBody(b, owner)
		}
	} else if list, err := s.store.SharedLinkAssets(ctx, l, 1000); err == nil {
		for _, a := range list {
			assets = append(assets, assetResponse(a))
		}
	}
	var description, password, expires any
	if l.Description != "" {
		description = l.Description
	}
	if l.Password != "" {
		password = passwordShown
	}
	if l.ExpiresAt != "" {
		expires = l.ExpiresAt
	}
	return map[string]any{"id": l.ID, "key": l.Key, "userId": l.OwnerID, "type": kind, "description": description, "password": password,
		"expiresAt": expires, "createdAt": l.CreatedAt, "allowUpload": false, "allowDownload": l.AllowDownload, "showMetadata": l.ShowMetadata,
		"slug": nil, "assets": assets, "album": album}
}

func (s *Server) linksOff(w http.ResponseWriter) bool {
	if s.LinksOn() {
		return false
	}
	writeError(w, 404, "Shared links work while this server is connected to tidoly")
	return true
}

func (s *Server) linkFail(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found")
		return
	}
	log.Printf("shared links: %v", err)
	writeError(w, 500, "Internal error")
}

// expiry reads the end a link was given: "" for never.
func expiry(raw *string) (string, bool) {
	if raw == nil || *raw == "" {
		return "", true
	}
	t, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return "", false
	}
	return store.Timestamp(t), true
}

// NewSharedLink makes a link of an album (albumID) or of photos chosen one by one.
func (s *Server) NewSharedLink(ctx context.Context, owner store.User, albumID string, assetIDs []string, l store.SharedLink, password string) (store.SharedLink, error) {
	l.ID, l.Key, l.OwnerID, l.AlbumID, l.CreatedAt = store.NewID(), newToken(), owner.ID, albumID, store.Timestamp(time.Now())
	if password != "" {
		l.Password = hashPassword(password)
	}
	return l, s.store.AddSharedLink(ctx, l, assetIDs)
}

func (s *Server) linkList(w http.ResponseWriter, r *http.Request, sess session) {
	if s.linksOff(w) {
		return
	}
	links, err := s.store.SharedLinks(r.Context(), sess.user.ID, r.URL.Query().Get("albumId"))
	if err != nil {
		s.linkFail(w, err)
		return
	}
	out := []any{}
	for _, l := range links {
		out = append(out, s.linkBody(r.Context(), l, sess.user))
	}
	writeJSON(w, 200, out)
}

func (s *Server) linkCreate(w http.ResponseWriter, r *http.Request, sess session) {
	if s.linksOff(w) {
		return
	}
	var req struct {
		Type          string   `json:"type"`
		AlbumID       string   `json:"albumId"`
		AssetIDs      []string `json:"assetIds"`
		Description   *string  `json:"description"`
		Password      *string  `json:"password"`
		ExpiresAt     *string  `json:"expiresAt"`
		AllowDownload *bool    `json:"allowDownload"`
		ShowMetadata  *bool    `json:"showMetadata"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	l := store.SharedLink{AllowDownload: req.AllowDownload == nil || *req.AllowDownload, ShowMetadata: req.ShowMetadata == nil || *req.ShowMetadata}
	if req.Description != nil {
		l.Description = strings.TrimSpace(*req.Description)
	}
	var ok bool
	if l.ExpiresAt, ok = expiry(req.ExpiresAt); !ok {
		writeError(w, 400, "expiresAt is not a date")
		return
	}
	password := ""
	if req.Password != nil {
		password = *req.Password
	}
	switch {
	case req.Type == "ALBUM" && req.AlbumID != "":
		req.AssetIDs = nil
	case req.Type == "INDIVIDUAL" && len(req.AssetIDs) > 0:
		req.AlbumID = ""
	default:
		writeError(w, 400, "An album, or some photos, is needed")
		return
	}
	l, err := s.NewSharedLink(r.Context(), sess.user, req.AlbumID, req.AssetIDs, l, password)
	if err != nil {
		s.linkFail(w, err) // an album that is not theirs, or none of the photos is: 404
		return
	}
	log.Printf("shared links: %s made one", sess.user.Email)
	writeJSON(w, 201, s.linkBody(r.Context(), l, sess.user))
}

func (s *Server) linkGet(w http.ResponseWriter, r *http.Request, sess session) {
	if s.linksOff(w) {
		return
	}
	l, err := s.store.SharedLink(r.Context(), sess.user.ID, r.PathValue("id"))
	if err != nil {
		s.linkFail(w, err)
		return
	}
	writeJSON(w, 200, s.linkBody(r.Context(), l, sess.user))
}

// linkUpdate changes what was sent, and only that: a null description, password or end takes it away.
func (s *Server) linkUpdate(w http.ResponseWriter, r *http.Request, sess session) {
	if s.linksOff(w) {
		return
	}
	l, err := s.store.SharedLink(r.Context(), sess.user.ID, r.PathValue("id"))
	if err != nil {
		s.linkFail(w, err)
		return
	}
	var req map[string]json.RawMessage
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	text := func(name string) (*string, bool) {
		raw, sent := req[name]
		if !sent {
			return nil, false
		}
		var v *string
		json.Unmarshal(raw, &v)
		return v, true
	}
	flag := func(name string, into *bool) {
		if raw, sent := req[name]; sent {
			json.Unmarshal(raw, into)
		}
	}
	if v, sent := text("description"); sent {
		l.Description = ""
		if v != nil {
			l.Description = strings.TrimSpace(*v)
		}
	}
	if v, sent := text("password"); sent {
		switch {
		case v == nil || *v == "":
			l.Password = ""
		case *v != passwordShown:
			l.Password = hashPassword(*v)
		}
	}
	if v, sent := text("expiresAt"); sent {
		var ok bool
		if l.ExpiresAt, ok = expiry(v); !ok {
			writeError(w, 400, "expiresAt is not a date")
			return
		}
	}
	flag("allowDownload", &l.AllowDownload)
	flag("showMetadata", &l.ShowMetadata)
	if err := s.store.UpdateSharedLink(r.Context(), l); err != nil {
		s.linkFail(w, err)
		return
	}
	writeJSON(w, 200, s.linkBody(r.Context(), l, sess.user))
}

func (s *Server) linkDelete(w http.ResponseWriter, r *http.Request, sess session) {
	if s.linksOff(w) {
		return
	}
	if err := s.store.DeleteSharedLink(r.Context(), sess.user.ID, r.PathValue("id")); err != nil {
		s.linkFail(w, err)
		return
	}
	w.WriteHeader(204)
}

// linkAssets puts photos into a link of photos chosen one by one (add), or takes them out.
func (s *Server) linkAssets(add bool) func(http.ResponseWriter, *http.Request, session) {
	return func(w http.ResponseWriter, r *http.Request, sess session) {
		if s.linksOff(w) {
			return
		}
		l, err := s.store.SharedLink(r.Context(), sess.user.ID, r.PathValue("id"))
		if err != nil {
			s.linkFail(w, err)
			return
		}
		if l.AlbumID != "" {
			writeError(w, 400, "An album's link shows the album; change the album instead")
			return
		}
		var req struct {
			AssetIDs []string `json:"assetIds"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
			writeError(w, 400, "Bad request")
			return
		}
		why, err := s.store.ChangeSharedLinkAssets(r.Context(), l, req.AssetIDs, add)
		if err != nil {
			s.linkFail(w, err)
			return
		}
		out := []any{}
		for i, id := range req.AssetIDs {
			entry := map[string]any{"assetId": id, "success": why[i] == ""}
			if why[i] != "" {
				entry["error"] = why[i]
			}
			out = append(out, entry)
		}
		writeJSON(w, 200, out)
	}
}

// ── opening a link ──────────────────────────────────────────────────────────────────────────────────────────

// Link states, as the page that opens one shows them.
const (
	LinkOpen     = "open"
	LinkPassword = "password" // it has a password, not yet given in this browser
	LinkGone     = "gone"     // no such link, ended, or not reachable now
)

// A link with a password, once the password is given, is open in that browser: a cookie holds a proof made
// from the key and the password's hash, neither of which leaves the server.
func linkCookie(l store.SharedLink) string { return "stow_link_" + l.ID }

func linkProof(l store.SharedLink) string {
	sum := sha256.Sum256([]byte(l.Key + "\x00" + l.Password))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// OpenLink finds the link an address names and says whether this browser may see it.
func (s *Server) OpenLink(r *http.Request, key string) (store.SharedLink, string) {
	if !s.LinksOn() {
		return store.SharedLink{}, LinkGone
	}
	l, err := s.store.SharedLinkByKey(r.Context(), key)
	if err != nil || l.Expired(time.Now()) {
		return store.SharedLink{}, LinkGone
	}
	if l.Password != "" {
		c, err := r.Cookie(linkCookie(l))
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(linkProof(l))) != 1 {
			return l, LinkPassword
		}
	}
	return l, LinkOpen
}

// ErrWrongPassword: the password given for a link is not its password, or too many wrong ones came lately.
var ErrWrongPassword = errors.New("wrong password")

var linkGuesses = tries{wrong: map[string]int{}, until: map[string]time.Time{}}

// UnlockLink takes the password for a link and, when it is right, opens the link in this browser.
func (s *Server) UnlockLink(w http.ResponseWriter, r *http.Request, l store.SharedLink, password string) error {
	if linkGuesses.blocked(l.ID) { // everyone comes through the tunnel from this machine: count by link, not by address
		return ErrWrongPassword
	}
	ok := passwordMatches(l.Password, password)
	linkGuesses.note(l.ID, ok)
	if !ok {
		return ErrWrongPassword
	}
	http.SetCookie(w, &http.Cookie{Name: linkCookie(l), Value: linkProof(l), Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	return nil
}

// photo answers a request for one photo's file: through the person's session, or — with ?key= — through a
// shared link, which shows only its own photos, and their originals only when it lets one download.
func (s *Server) photo(send func(http.ResponseWriter, *http.Request, store.Asset), original bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			s.authed(func(w http.ResponseWriter, r *http.Request, sess session) {
				asset, err := s.store.AssetFor(r.Context(), sess.user.ID, r.PathValue("id")) // one's own, or one that is shared
				if err != nil {
					s.photoFail(w, err)
					return
				}
				send(w, r, asset)
			})(w, r)
			return
		}
		l, state := s.OpenLink(r, key)
		if state != LinkOpen || original && !l.AllowDownload {
			writeError(w, 404, "Not found")
			return
		}
		asset, err := s.store.SharedLinkAsset(r.Context(), l, r.PathValue("id"))
		if err != nil {
			s.photoFail(w, err)
			return
		}
		w.Header().Set("Referrer-Policy", "no-referrer") // the key is in the address
		if !l.AllowDownload {
			r = r.WithContext(context.WithValue(r.Context(), keepOriginalKey{}, true))
		}
		send(w, r, asset)
	}
}

// keepOriginalKey marks a request through a shared link that keeps originals back: what stands in for a
// small picture not made yet must not be the original then.
type keepOriginalKey struct{}

func keepsOriginal(r *http.Request) bool {
	keep, _ := r.Context().Value(keepOriginalKey{}).(bool)
	return keep
}

func (s *Server) photoFail(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found") // also the answer for someone else's photo
		return
	}
	writeError(w, 500, "Internal error")
}
