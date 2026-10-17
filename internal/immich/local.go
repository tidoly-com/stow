package immich

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidoly-com/stow/internal/lan"
	"github.com/tidoly-com/stow/internal/store"
)

// A server that is used at home only needs no account anywhere else: it keeps its own. A person is an
// address to log in with and a password; a login is a session cookie, the same as with tidoly's login.
// The password is stored hashed, and a run of wrong guesses is made to wait — the server is open to
// everything on the home network, not only to its owner.

var (
	ErrBadLogin = errors.New("wrong address or password")
	ErrWait     = errors.New("too many wrong tries; wait a minute")
	ErrNotHome  = errors.New("this server's own accounts log in only at home")
	ErrTaken    = errors.New("that address already has an account")
	ErrWeak     = errors.New("the address needs an @ and the password four characters or more")
)

const hashRounds = 200_000

func hashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, password, salt, hashRounds, 32)
	return base64.RawStdEncoding.EncodeToString(salt) + "." + base64.RawStdEncoding.EncodeToString(key)
}

func passwordMatches(hash, password string) bool {
	saltText, keyText, ok := strings.Cut(hash, ".")
	salt, err1 := base64.RawStdEncoding.DecodeString(saltText)
	want, err2 := base64.RawStdEncoding.DecodeString(keyText)
	if !ok || err1 != nil || err2 != nil {
		return false
	}
	got, _ := pbkdf2.Key(sha256.New, password, salt, hashRounds, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// tries counts wrong passwords per address; five in a row make that address wait a minute.
type tries struct {
	mu    sync.Mutex
	wrong map[string]int
	until map[string]time.Time
}

var guesses = tries{wrong: map[string]int{}, until: map[string]time.Time{}}

func (t *tries) blocked(email string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Now().Before(t.until[email])
}

func (t *tries) note(email string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ok {
		delete(t.wrong, email)
		return
	}
	if t.wrong[email]++; t.wrong[email] >= 5 {
		t.wrong[email], t.until[email] = 0, time.Now().Add(time.Minute)
	}
}

// NeedsOwner reports that nobody has an account yet: the first person to come sets the server up.
func (s *Server) NeedsOwner(ctx context.Context) bool {
	n, err := s.store.CountUsers(ctx)
	return err == nil && n == 0
}

// AddLocalUser makes an account. The first one made owns the server.
func (s *Server) AddLocalUser(ctx context.Context, name, email, password string) (store.User, error) {
	name, email = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(password) < 4 || len(email) > 200 || len(password) > 200 {
		return store.User{}, ErrWeak
	}
	if _, _, err := s.store.UserByEmail(ctx, email); err == nil {
		return store.User{}, ErrTaken
	}
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	user, err := s.store.UpsertUser(ctx, "local:"+email, email, name, n == 0)
	if err != nil {
		return store.User{}, err
	}
	return user, s.store.SetPassword(ctx, user.ID, hashPassword(password))
}

// SetLocalPassword gives a person a new password.
func (s *Server) SetLocalPassword(ctx context.Context, userID, password string) error {
	if len(password) < 4 || len(password) > 200 {
		return ErrWeak
	}
	return s.store.SetPassword(ctx, userID, hashPassword(password))
}

// ChangeOwnPassword lets a person replace their password, given the one they have now. Wrong guesses
// count as wrong logins do.
func (s *Server) ChangeOwnPassword(ctx context.Context, email, current, next string) error {
	if guesses.blocked(email) {
		return ErrWait
	}
	user, hash, err := s.store.UserByEmail(ctx, email)
	ok := err == nil && hash != "" && passwordMatches(hash, current)
	guesses.note(email, ok)
	if !ok {
		return ErrBadLogin
	}
	return s.SetLocalPassword(ctx, user.ID, next)
}

// localLogin checks an address and a password and opens a session. The server's own accounts log in only
// at home: from outside, through the tunnel, one logs in with tidoly (internal/lan). A request from outside
// is refused before the password is looked at, and is not counted as a wrong try.
func (s *Server) localLogin(r *http.Request, email, password string) (store.User, string, error) {
	ctx := r.Context()
	if !lan.From(r) {
		return store.User{}, "", ErrNotHome
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if guesses.blocked(email) {
		return store.User{}, "", ErrWait
	}
	user, hash, err := s.store.UserByEmail(ctx, email)
	ok := err == nil && hash != "" && passwordMatches(hash, password)
	guesses.note(email, ok)
	if !ok {
		return store.User{}, "", ErrBadLogin
	}
	token := newToken()
	if err := s.store.CreateSession(ctx, token, user.ID, store.ViaHome); err != nil {
		return store.User{}, "", err
	}
	return user, token, nil
}

// WebLocalLogin logs a browser in with an address and a password; on success the session cookie is set.
func (s *Server) WebLocalLogin(w http.ResponseWriter, r *http.Request, email, password string) error {
	user, token, err := s.localLogin(r, email, password)
	if err != nil {
		return err
	}
	log.Printf("logged in (browser): %s", user.Email)
	http.SetCookie(w, s.webCookie(r, "stow_session", token, 400*24*3600))
	return nil
}

// passwordLogin is the app's login with an address and a password: POST /api/auth/login.
func (s *Server) passwordLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	user, token, err := s.localLogin(r, req.Email, req.Password)
	switch {
	case errors.Is(err, ErrNotHome):
		writeError(w, 403, "Log in with an address and a password only at home, on the same Wi-Fi")
		return
	case errors.Is(err, ErrWait):
		writeError(w, 429, "Too many wrong tries; wait a minute")
		return
	case errors.Is(err, ErrBadLogin):
		writeError(w, 401, "Incorrect email or password")
		return
	case err != nil:
		log.Printf("login: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	log.Printf("logged in (app): %s", user.Email)
	s.setAuthCookies(w, r, token, "password")
	body := fixture("login")
	body["accessToken"], body["userId"], body["userEmail"], body["name"] = token, user.ID, user.Email, user.Name
	body["isAdmin"], body["isOnboarded"] = user.IsAdmin, true
	writeJSON(w, 201, body)
}
