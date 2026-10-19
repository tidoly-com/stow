package immich

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/ticket"
)

// The link to tidoly (stow issue #4). Every server keeps its own accounts and logs them in at home with a
// password. Once the owner connects it to tidoly — from the admin page, at home — it has a name there (the
// slug), an address outside (<slug>.stow.tidoly.app, through the tunnel) and a key of its own. From then on
// a person can also log in with tidoly: home signs a ticket with their Google address, and this server
// finds the account with that address. The owner is whoever has the owner's address here. Someone home lists
// for this server and who has no account yet gets one, as an ordinary member, the first time they come —
// unless the owner switched that off; and the owner can shut tidoly's login for any one person.

// Link is what this server knows of its place with tidoly.
type Link struct {
	Slug string `json:"slug"`           // its name with tidoly
	Host string `json:"host"`           // its address outside: <slug>.stow.tidoly.app. "" for a trial that gives its address itself
	Key  string `json:"key"`            // its key with tidoly — for the tunnel, the member list, the certificate. Secret
	Frps string `json:"frps,omitempty"` // the tunnel server tidoly named for it, host:port; "" until tidoly has one
}

const linkSetting = "tidoly"

var (
	errNoAccount = errors.New("no account here with that address")
	errBlocked   = errors.New("tidoly's login is shut for this person")
)

// loadLink reads the link the owner made, unless the program was started with one (a trial).
func (s *Server) loadLink(ctx context.Context) {
	if s.cfg.Link.Slug != "" {
		l := s.cfg.Link
		s.link.Store(&l)
		return
	}
	var l Link
	if raw := s.store.Setting(ctx, linkSetting); raw != "" {
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			log.Printf("tidoly: the link kept in the settings cannot be read: %v", err)
		}
	}
	s.link.Store(&l)
}

// Link is this server's link to tidoly; an empty Slug means it has none.
func (s *Server) Link() Link {
	if l := s.link.Load(); l != nil {
		return *l
	}
	return Link{}
}

func (s *Server) setLink(ctx context.Context, l Link) error {
	raw := ""
	if l.Slug != "" {
		b, _ := json.Marshal(l)
		raw = string(b)
	}
	if err := s.store.SetSetting(ctx, linkSetting, raw); err != nil {
		return err
	}
	s.link.Store(&l)
	s.linkProblem.Store("")
	if s.cfg.Linked != nil {
		s.cfg.Linked(l)
	}
	return nil
}

// tidoly says whether tickets from tidoly-home are taken: the server has a slug and home's key to check them.
func (s *Server) tidoly() bool {
	return s.Link().Slug != "" && len(s.cfg.HomeKey) == ed25519.PublicKeySize
}

func (s *Server) verifier() ticket.Verifier {
	return ticket.Verifier{Key: s.cfg.HomeKey, Issuer: s.cfg.HomeURL, Slug: s.Link().Slug}
}

// loginBase is the address a login through tidoly comes back to: the address outside.
func (s *Server) loginBase() string {
	if host := s.Link().Host; host != "" {
		return "https://" + host
	}
	return s.cfg.PublicURL
}

func (s *Server) mobileRedirect() string { return s.loginBase() + "/api/oauth/mobile-redirect" }

// TidolyHere says whether a browser at this address can log in with tidoly: home sends it back to the
// address outside, and the login cookie must be there when it comes.
func (s *Server) TidolyHere(r *http.Request) bool {
	if !s.tidoly() {
		return false
	}
	base, err := url.Parse(s.loginBase())
	return err == nil && strings.EqualFold(base.Host, r.Host)
}

// secure says whether the request came over https: at the address outside it did — the tunnel's entrance
// is https — even where the last step, from frpc on this machine, is not.
func (s *Server) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if l := s.Link(); l.Host != "" {
		return strings.EqualFold(host, l.Host)
	}
	return strings.HasPrefix(s.cfg.PublicURL, "https://")
}

// ── who a ticket lets in ────────────────────────────────────────────────────────────────────────────────────

const autoAddSetting = "tidoly-auto-add"

// AutoAdd says whether someone home lists for this server gets an account the first time they come. On
// unless the owner switched it off.
func (s *Server) AutoAdd(ctx context.Context) bool {
	return s.store.Setting(ctx, autoAddSetting) != "off"
}

func (s *Server) SetAutoAdd(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = ""
	}
	return s.store.SetSetting(ctx, autoAddSetting, value)
}

// ticketUser is the account a ticket opens: the one with its Google address. Without one, a person home
// lists gets one now as an ordinary member — never as the owner: home cannot make anyone the owner here.
// Only the very first account of a server nobody has come into yet is made the owner's, when home says so.
func (s *Server) ticketUser(ctx context.Context, c ticket.Claims) (store.User, error) {
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if !strings.Contains(email, "@") {
		return store.User{}, errors.New("the ticket has no address")
	}
	user, _, err := s.store.UserByEmail(ctx, email)
	if err == nil {
		if user.Blocked {
			return store.User{}, errBlocked
		}
		return user, nil
	}
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	if n > 0 && !s.AutoAdd(ctx) {
		return store.User{}, errNoAccount
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	user, err = s.store.UpsertUser(ctx, "tidoly:"+c.Subject, email, name, n == 0 && c.Role == "owner")
	if err == nil {
		log.Printf("tidoly: %s came for the first time and was given an account", email)
	}
	return user, err
}

// ticketLogin checks a ticket, spends it and opens a session for the person it names.
func (s *Server) ticketLogin(ctx context.Context, code, verifier string) (store.User, string, error) {
	if !s.tidoly() {
		return store.User{}, "", errors.New("this server is not connected to tidoly")
	}
	claims, err := s.verifier().Verify(code, verifier, time.Now())
	if err != nil {
		return store.User{}, "", err
	}
	if fresh, err := s.store.SpendTicket(ctx, claims.ID, claims.Expires); err != nil {
		return store.User{}, "", err
	} else if !fresh {
		return store.User{}, "", errAlreadyUsed
	}
	user, err := s.ticketUser(ctx, claims)
	if err != nil {
		return store.User{}, "", err
	}
	token := newToken()
	if err := s.store.CreateSession(ctx, token, user.ID, store.ViaTidoly); err != nil {
		return store.User{}, "", err
	}
	return user, token, nil
}

// Block shuts tidoly's login for one person — the sessions they opened with it end now — or opens it again.
func (s *Server) Block(ctx context.Context, userID string, blocked bool) error {
	return s.store.Block(ctx, userID, blocked)
}

// ── what the owner is told ──────────────────────────────────────────────────────────────────────────────────

const arrivalsSetting = "tidoly-arrivals-seen"

// Arrivals are the people tidoly made an account for, who came in for the first time since the owner last
// looked: the way to notice someone home let in that the owner did not expect.
func (s *Server) Arrivals(ctx context.Context) ([]store.User, error) {
	seen := s.store.Setting(ctx, arrivalsSetting)
	users, err := s.store.Users(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.User
	for _, u := range users {
		// only the accounts tidoly made: someone with an account here coming in by tidoly is no news
		if strings.HasPrefix(u.Sub, "tidoly:") && u.TidolyAt != "" && u.TidolyAt > seen {
			out = append(out, u)
		}
	}
	return out, nil
}

// SeenArrivals records that the owner has seen who came: the latest of them, not the time of looking — someone
// coming in the same moment is still shown.
func (s *Server) SeenArrivals(ctx context.Context) error {
	arrivals, err := s.Arrivals(ctx)
	if err != nil || len(arrivals) == 0 {
		return err
	}
	latest := ""
	for _, u := range arrivals {
		latest = max(latest, u.TidolyAt)
	}
	return s.store.SetSetting(ctx, arrivalsSetting, latest)
}

// LinkProblem is what went wrong with tidoly lately, for the admin page: "" if nothing did.
func (s *Server) LinkProblem() string {
	p, _ := s.linkProblem.Load().(string)
	return p
}

// ── connecting, from the admin page ─────────────────────────────────────────────────────────────────────────

// ConnectStart sends the owner's browser to tidoly to sign in, agree, and choose the server's name. Home
// sends it back here with a code only good together with the verifier kept in the cookie; the key itself
// never passes through the browser.
func (s *Server) ConnectStart(w http.ResponseWriter, r *http.Request) {
	state, verifier := newToken(), newToken()
	sum := sha256.Sum256([]byte(verifier))
	http.SetCookie(w, s.webCookie(r, "stow_connect", state+"."+verifier, 900))
	scheme := "http"
	if s.secure(r) {
		scheme = "https"
	}
	q := url.Values{"return": {scheme + "://" + r.Host + "/admin/tidoly/back"}, "state": {state},
		"cc": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	http.Redirect(w, r, s.cfg.HomeURL+"/stow/connect?"+q.Encode(), http.StatusSeeOther)
}

// ConnectBack takes the code home sent the browser back with and trades it for this server's key.
func (s *Server) ConnectBack(w http.ResponseWriter, r *http.Request) error {
	c, err := r.Cookie(s.cookieName(r, "stow_connect"))
	if err != nil {
		return errors.New("the connection was not started from this browser, or took too long")
	}
	http.SetCookie(w, s.webCookie(r, "stow_connect", "", -1))
	state, verifier, _ := strings.Cut(c.Value, ".")
	code := r.URL.Query().Get("code")
	if state == "" || state != r.URL.Query().Get("state") || code == "" {
		return errors.New("the answer from tidoly does not belong to this connection")
	}
	l, err := s.serverKey(r.Context(), code, verifier)
	if err != nil {
		return err
	}
	log.Printf("tidoly: connected as %s", l.Host)
	return s.setLink(r.Context(), l)
}

func (s *Server) serverKey(ctx context.Context, code, verifier string) (Link, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	req, err := http.NewRequestWithContext(ctx, "POST", s.cfg.HomeURL+"/stow/server-key", bytes.NewReader(body))
	if err != nil {
		return Link{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.homeClient().Do(req)
	if err != nil {
		return Link{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return Link{}, fmt.Errorf("tidoly answered %s", res.Status)
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	var answer struct {
		Slug, Host, Key string
		Frps            frpsAddress
	}
	switch err := json.Unmarshal(raw, &answer); {
	case err != nil:
		var fields map[string]any
		json.Unmarshal(raw, &fields)
		names := make([]string, 0, len(fields))
		for name, v := range fields {
			names = append(names, fmt.Sprintf("%s:%T", name, v))
		}
		return Link{}, fmt.Errorf("tidoly's answer is not a server key: %v (fields %v)", err, names) // never the values: the key is one
	case answer.Slug == "" || answer.Key == "":
		return Link{}, errors.New("tidoly's answer has no slug or no key")
	case !strings.HasPrefix(answer.Host, answer.Slug+"."):
		return Link{}, fmt.Errorf("tidoly's answer: the address %q is not under %q", answer.Host, answer.Slug)
	}
	l := Link{Slug: answer.Slug, Host: answer.Host, Key: answer.Key, Frps: string(answer.Frps)}
	l.Host = strings.ToLower(l.Host)
	return l, nil
}

// Disconnect forgets the link: the tunnel closes and tidoly's login stops. Home still has the key until
// the owner ends it there, or connects again.
func (s *Server) Disconnect(ctx context.Context) error {
	log.Printf("tidoly: disconnected")
	if host := s.Link().Host; host != "" { // shown when connecting again: the address it had
		if err := s.store.SetSetting(ctx, lastHostSetting, host); err != nil {
			return err
		}
	}
	return s.setLink(ctx, Link{})
}

const lastHostSetting = "tidoly-last-host"

// LastHost is the address outside this server had the last time it was connected; "" if it never was.
func (s *Server) LastHost(ctx context.Context) string { return s.store.Setting(ctx, lastHostSetting) }

func (s *Server) homeClient() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

// CallHome is a request to home made as this server, with its key: the answer's status and body. For the
// certificate (internal/certs); a 401 is told to the owner as everywhere else.
func (s *Server) CallHome(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if s.Link().Key == "" {
		return 0, nil, ErrNotLinked
	}
	req, err := s.homeRequest(ctx, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.homeClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized {
		s.linkProblem.Store(keyRefused)
	}
	return res.StatusCode, answer, err
}

// homeRequest is a request to home made as this server: with its key.
func (s *Server) homeRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.HomeURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if key := s.Link().Key; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// ── the people home lists ───────────────────────────────────────────────────────────────────────────────────

// WatchMembers closes the tidoly sessions of people home no longer lists for this server. A session outlives
// the ticket that opened it, so removal has to arrive this way. If home cannot be reached, nothing changes.
func (s *Server) WatchMembers(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	var askedTunnel time.Time
	for {
		if l := s.Link(); l.Key != "" && l.Frps == "" && time.Since(askedTunnel) >= time.Minute {
			askedTunnel = time.Now()
			if err := s.fetchTunnel(ctx); err != nil && !errors.Is(err, errKeyRefused) {
				log.Printf("tunnel server: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !s.tidoly() {
			continue
		}
		emails, err := s.fetchMembers(ctx)
		if err != nil {
			continue
		}
		if n, err := s.store.DropTidolySessionsExcept(ctx, emails); err != nil {
			log.Printf("members: %v", err)
		} else if n > 0 {
			log.Printf("members: closed %d session(s) of people no longer listed", n)
		}
	}
}

// frpsAddress is the tunnel server as home writes it — {"serverAddr": "...", "serverPort": 7001}, the names
// of frpc's own settings — or as "host:port"; kept as host:port.
type frpsAddress string

func (a *frpsAddress) UnmarshalJSON(b []byte) error {
	var text string
	if json.Unmarshal(b, &text) == nil {
		*a = frpsAddress(text)
		return nil
	}
	var parts struct {
		ServerAddr string `json:"serverAddr"`
		ServerPort int    `json:"serverPort"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return fmt.Errorf("frps: %v", err)
	}
	switch {
	case parts.ServerAddr == "":
		*a = ""
	case parts.ServerPort == 0:
		*a = frpsAddress(parts.ServerAddr)
	default:
		*a = frpsAddress(net.JoinHostPort(parts.ServerAddr, strconv.Itoa(parts.ServerPort)))
	}
	return nil
}

var errKeyRefused = errors.New("tidoly does not take this server's key")

// keyRefused is what the owner is told when home answers the key with 401: another PC connected with the
// same address, which ends the old key, or tidoly stopped the address.
const keyRefused = "tidoly 가 이 서버의 열쇠를 받지 않습니다 — 다른 PC 가 같은 주소로 연결했거나, tidoly 가 이 주소를 멈췄습니다. 이 PC 로 다시 연결하거나 tidoly 에 문의해 주세요."

// fetchTunnel asks home which tunnel server this server is to use, when the key came without one; once
// there is one, the tunnel opens.
func (s *Server) fetchTunnel(ctx context.Context) error {
	req, err := s.homeRequest(ctx, "GET", "/stow/tunnel", nil)
	if err != nil {
		return err
	}
	res, err := s.homeClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		s.linkProblem.Store(keyRefused)
		return errKeyRefused
	}
	if res.StatusCode != 200 {
		return errors.New(res.Status)
	}
	var body struct {
		Frps frpsAddress `json:"frps"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return err
	}
	if body.Frps == "" {
		return nil // not yet
	}
	l := s.Link()
	l.Frps = string(body.Frps)
	log.Printf("tidoly: tunnel server %s", l.Frps)
	return s.setLink(ctx, l)
}

// TunnelWaiting asks home where this server is in the line for a tunnel, which it joins when the week's
// new tunnels are taken: its place, and when the next place opens. ok is false when it is not in the line
// (home leaves the field out), or home cannot be asked.
func (s *Server) TunnelWaiting(ctx context.Context) (position int, opensAt time.Time, ok bool) {
	if s.Link().Key == "" {
		return 0, time.Time{}, false
	}
	req, err := s.homeRequest(ctx, "GET", "/stow/tunnel", nil)
	if err != nil {
		return 0, time.Time{}, false
	}
	res, err := s.homeClient().Do(req)
	if err != nil {
		log.Printf("tunnel line: %v", err)
		return 0, time.Time{}, false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Printf("tunnel line: %s", res.Status)
		return 0, time.Time{}, false
	}
	var body struct {
		Waiting *struct {
			Position int       `json:"position"`
			OpensAt  time.Time `json:"opensAt"`
		} `json:"waiting"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.Waiting == nil || body.Waiting.Position < 1 {
		return 0, time.Time{}, false
	}
	return body.Waiting.Position, body.Waiting.OpensAt, true
}

func (s *Server) fetchMembers(ctx context.Context) ([]string, error) {
	req, err := s.homeRequest(ctx, "GET", "/stow/members?slug="+url.QueryEscape(s.Link().Slug), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.homeClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		s.linkProblem.Store(keyRefused)
		return nil, errKeyRefused
	}
	if res.StatusCode != 200 {
		return nil, errors.New(res.Status)
	}
	s.linkProblem.Store("")
	var body struct {
		Members *[]struct {
			Email string `json:"email"`
		} `json:"members"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.Members == nil {
		return nil, errors.New("not a member list") // an answer without the list must not read as "nobody"
	}
	emails := make([]string, 0, len(*body.Members))
	for _, m := range *body.Members {
		emails = append(emails, strings.ToLower(m.Email))
	}
	return emails, nil
}

// ErrNotLinked: a call to home from a server that is not connected to tidoly.
var ErrNotLinked = errors.New("this server is not connected to tidoly")
