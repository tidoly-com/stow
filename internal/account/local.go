package account

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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

// HashPassword is a password as it is kept: an account's, or a shared link's.
func HashPassword(password string) string { return hashPassword(password) }

// PasswordMatches says whether password is the one kept as hash.
func PasswordMatches(hash, password string) bool {
	saltText, keyText, ok := strings.Cut(hash, ".")
	salt, err1 := base64.RawStdEncoding.DecodeString(saltText)
	want, err2 := base64.RawStdEncoding.DecodeString(keyText)
	if !ok || err1 != nil || err2 != nil {
		return false
	}
	got, _ := pbkdf2.Key(sha256.New, password, salt, hashRounds, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Tries counts wrong passwords per name — an address, a link; five in a row make that name wait a minute.
type Tries struct {
	mu    sync.Mutex
	wrong map[string]int
	until map[string]time.Time
}

// NewTries is a count of wrong passwords with none yet.
func NewTries() *Tries { return &Tries{wrong: map[string]int{}, until: map[string]time.Time{}} }

var guesses = NewTries()

// Blocked says whether name is made to wait.
func (t *Tries) Blocked(email string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Now().Before(t.until[email])
}

// Note counts a try with name, right or wrong.
func (t *Tries) Note(email string, ok bool) {
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
func (a *Accounts) NeedsOwner(ctx context.Context) bool {
	n, err := a.store.CountUsers(ctx)
	return err == nil && n == 0
}

// AddLocalUser makes an account. The first one made owns the server.
func (a *Accounts) AddLocalUser(ctx context.Context, name, email, password string) (store.User, error) {
	name, email = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(password) < 4 || len(email) > 200 || len(password) > 200 {
		return store.User{}, ErrWeak
	}
	if _, _, err := a.store.UserByEmail(ctx, email); err == nil {
		return store.User{}, ErrTaken
	}
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	n, err := a.store.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	user, err := a.store.UpsertUser(ctx, "local:"+email, email, name, n == 0)
	if err != nil {
		return store.User{}, err
	}
	return user, a.store.SetPassword(ctx, user.ID, hashPassword(password))
}

// SetLocalPassword gives a person a new password.
func (a *Accounts) SetLocalPassword(ctx context.Context, userID, password string) error {
	if len(password) < 4 || len(password) > 200 {
		return ErrWeak
	}
	return a.store.SetPassword(ctx, userID, hashPassword(password))
}

// ChangeOwnPassword lets a person replace their password, given the one they have now. Wrong guesses
// count as wrong logins do.
func (a *Accounts) ChangeOwnPassword(ctx context.Context, email, current, next string) error {
	if guesses.Blocked(email) {
		return ErrWait
	}
	user, hash, err := a.store.UserByEmail(ctx, email)
	ok := err == nil && hash != "" && PasswordMatches(hash, current)
	guesses.Note(email, ok)
	if !ok {
		return ErrBadLogin
	}
	return a.SetLocalPassword(ctx, user.ID, next)
}

// LocalLogin checks an address and a password and opens a session. The server's own accounts log in only
// at home: from outside, through the tunnel, one logs in with tidoly (internal/lan). A request from outside
// is refused before the password is looked at, and is not counted as a wrong try.
func (a *Accounts) LocalLogin(r *http.Request, email, password string) (store.User, string, error) {
	ctx := r.Context()
	if !lan.From(r) {
		return store.User{}, "", ErrNotHome
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if guesses.Blocked(email) {
		return store.User{}, "", ErrWait
	}
	user, hash, err := a.store.UserByEmail(ctx, email)
	ok := err == nil && hash != "" && PasswordMatches(hash, password)
	guesses.Note(email, ok)
	if !ok {
		return store.User{}, "", ErrBadLogin
	}
	token := NewToken()
	if err := a.store.CreateSession(ctx, token, user.ID, store.ViaHome); err != nil {
		return store.User{}, "", err
	}
	return user, token, nil
}

// WebLocalLogin logs a browser in with an address and a password; on success the session cookie is set.
func (a *Accounts) WebLocalLogin(w http.ResponseWriter, r *http.Request, email, password string) error {
	user, token, err := a.LocalLogin(r, email, password)
	if err != nil {
		return err
	}
	log.Printf("logged in (browser): %s", user.Email)
	http.SetCookie(w, a.webCookie(r, "stow_session", token, 400*24*3600))
	return nil
}
