package immich

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/ticket"
)

// A ticket opens the account with its Google address: the one made at home with a password is the same
// person, and stays the owner whatever home says of them.
func TestATicketFindsTheAccountByItsAddress(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	owner, err := e.api.AddLocalUser(ctx, "엄마", "Mom@Example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	dad, err := e.api.AddLocalUser(ctx, "아빠", "dad@example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	res := e.callback(claimsFor("mom@example.com", "member", "v1"), "v1")
	if body := decode(t, res); res.StatusCode != 201 || body["userId"] != owner.ID || body["isAdmin"] != true {
		t.Errorf("the owner coming with tidoly: %d %v", res.StatusCode, body)
	}
	res = e.callback(claimsFor("dad@example.com", "owner", "v2"), "v2")
	if body := decode(t, res); res.StatusCode != 201 || body["userId"] != dad.ID || body["isAdmin"] != false {
		t.Errorf("home calling someone the owner does not make them the owner here: %d %v", res.StatusCode, body)
	}
}

// Someone home lists who has no account here gets one, as a member — even if home says owner. Switched
// off, they get nothing until the owner adds them here too.
func TestPeopleTidolyAddsGetAnAccountUnlessSwitchedOff(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	if _, err := e.api.AddLocalUser(ctx, "엄마", "mom@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	res := e.callback(claimsFor("stranger@example.com", "owner", "v1"), "v1")
	if body := decode(t, res); res.StatusCode != 201 || body["isAdmin"] != false {
		t.Errorf("a new person from tidoly: %d %v", res.StatusCode, body)
	}
	if !e.api.AutoAdd(ctx) {
		t.Fatal("taking tidoly's people in must be on at first")
	}
	if err := e.api.SetAutoAdd(ctx, false); err != nil {
		t.Fatal(err)
	}
	res = e.callback(claimsFor("kid@example.com", "member", "v2"), "v2")
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Errorf("a new person with the switch off: %d, want 403", res.StatusCode)
	}
	if _, _, err := e.store.UserByEmail(ctx, "kid@example.com"); err == nil {
		t.Error("an account was made with the switch off")
	}
	if _, err := e.api.AddLocalUser(ctx, "아이", "kid@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	if res := e.callback(claimsFor("kid@example.com", "member", "v3"), "v3"); res.StatusCode != 201 {
		t.Errorf("added here as well, they get in: %d", res.StatusCode)
	}
}

// The owner sees who came with tidoly for the first time, and can shut tidoly's login for one person: their
// tidoly sessions end, the one opened at home with the password stays.
func TestTheOwnerSeesWhoCameAndCanShutThemOut(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	if _, err := e.api.AddLocalUser(ctx, "엄마", "mom@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := e.api.SeenArrivals(ctx); err != nil {
		t.Fatal(err)
	}
	if arrivals, _ := e.api.Arrivals(ctx); len(arrivals) != 0 {
		t.Fatalf("arrivals before anyone came: %v", arrivals)
	}
	e.login("mom@example.com", "member") // the owner, with the account she made here: no news
	away := e.login("cousin@example.com", "member")
	arrivals, _ := e.api.Arrivals(ctx)
	if len(arrivals) != 1 || arrivals[0].Email != "cousin@example.com" || arrivals[0].TidolyAt == "" {
		t.Fatalf("arrivals: %v", arrivals)
	}
	cousin := arrivals[0]
	if err := e.api.SetLocalPassword(ctx, cousin.ID, "secret"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"email": "cousin@example.com", "password": "secret"})
	res := e.do("POST", "/api/auth/login", "", bytes.NewReader(body), "application/json")
	home := decode(t, res)["accessToken"].(string)

	if err := e.api.Block(ctx, cousin.ID, true); err != nil {
		t.Fatal(err)
	}
	if res := e.do("GET", "/api/users/me", away, nil, ""); res.StatusCode != 401 {
		t.Errorf("their tidoly session after being shut out: %d, want 401", res.StatusCode)
	}
	if res := e.do("GET", "/api/users/me", home, nil, ""); res.StatusCode != 200 {
		t.Errorf("their session from home: %d, want 200", res.StatusCode)
	}
	res = e.callback(claimsFor("cousin@example.com", "member", "v9"), "v9")
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Errorf("a ticket for someone shut out: %d, want 403", res.StatusCode)
	}
	e.api.SeenArrivals(ctx)
	if arrivals, _ := e.api.Arrivals(ctx); len(arrivals) != 0 {
		t.Errorf("arrivals after the owner looked: %v", arrivals)
	}
}

// Home's member list ends the tidoly sessions of people no longer on it, not the ones opened at home.
func TestTheMemberListEndsOnlyTidolySessions(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	if _, err := e.api.AddLocalUser(ctx, "엄마", "mom@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	away := e.login("mom@example.com", "owner")
	body, _ := json.Marshal(map[string]string{"email": "mom@example.com", "password": "secret"})
	home := decode(t, e.do("POST", "/api/auth/login", "", bytes.NewReader(body), "application/json"))["accessToken"].(string)
	if n, err := e.store.DropTidolySessionsExcept(ctx, []string{"someone@example.com"}); err != nil || n != 1 {
		t.Fatalf("drop: %d %v", n, err)
	}
	if res := e.do("GET", "/api/users/me", away, nil, ""); res.StatusCode != 401 {
		t.Errorf("the tidoly session: %d, want 401", res.StatusCode)
	}
	if res := e.do("GET", "/api/users/me", home, nil, ""); res.StatusCode != 200 {
		t.Errorf("the session from home: %d, want 200", res.StatusCode)
	}
}

// Something else home signs — a ticket without the stow type — does not pass for a stow ticket.
func TestOnlyStowTicketsPass(t *testing.T) {
	key := newKey(t)
	c := claimsFor("a@example.com", "member", "v")
	payload, _ := json.Marshal(c)
	body := b64.EncodeToString([]byte(`{"alg":"EdDSA"}`)) + "." + b64.EncodeToString(payload)
	code := body + "." + b64.EncodeToString(ed25519.Sign(key, []byte(body)))
	v := ticket.Verifier{Key: key.Public().(ed25519.PublicKey), Issuer: testHome, Slug: testSlug}
	if _, err := v.Verify(code, "v", time.Now()); err == nil {
		t.Error("a ticket without typ passed")
	}
	if _, err := v.Verify(signTicket(key, c), "v", time.Now()); err != nil {
		t.Errorf("a stow ticket: %v", err)
	}
}

// Connecting: the owner's browser goes to home with a challenge and comes back with a code; the server
// trades the code and the verifier for its key, keeps it, and asks home for the member list with it.
func TestConnectingToTidoly(t *testing.T) {
	ownAccounts = true
	defer func() { ownAccounts = false }()
	var members atomic.Value
	members.Store("")
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stow/server-key":
			var req struct{ Code, Verifier string }
			json.NewDecoder(r.Body).Decode(&req)
			if req.Code != "the-code" || challenge(req.Verifier) != lastChallenge.Load() {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"slug": "mom", "host": "mom.stow.tidoly.app", "key": "tdly_stow_secret"})
		case "/stow/members":
			members.Store(r.Header.Get("Authorization") + " " + r.URL.Query().Get("slug"))
			json.NewEncoder(w).Encode(map[string]any{"members": []any{}})
		case "/stow/tunnel": // the key came without a tunnel server; now there is one
			if r.Header.Get("Authorization") != "Bearer tdly_stow_secret" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"frps": map[string]any{"serverAddr": "frps.stow.test", "serverPort": 7000}}) // as home writes it
		}
	}))
	defer home.Close()
	e := start(t, t.TempDir(), newKey(t))
	e.api.cfg.HomeURL = home.URL
	var told atomic.Value
	e.api.cfg.Linked = func(l Link) { told.Store(l) }
	ctx := context.Background()
	if _, err := e.api.AddLocalUser(ctx, "엄마", "mom@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	if e.api.tidoly() {
		t.Fatal("tidoly's login before connecting")
	}
	req, _ := http.NewRequest("POST", e.server.URL+"/login", strings.NewReader("email=mom@example.com&password=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	session := res.Cookies()[0]

	res, _ = e.browse("/admin/tidoly/connect", session)
	to, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 303 || !strings.HasPrefix(to.String(), home.URL+"/stow/connect?") || to.Query().Get("return") != e.server.URL+"/admin/tidoly/back" {
		t.Fatalf("connect: %d to %s", res.StatusCode, to)
	}
	lastChallenge.Store(to.Query().Get("cc"))
	var connect *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "stow_connect" {
			connect = c
		}
	}
	back := "/admin/tidoly/back?" + url.Values{"code": {"the-code"}, "state": {to.Query().Get("state")}}.Encode()
	if res, _ := e.browse(back, session); res.StatusCode == 303 {
		t.Fatal("tidoly's answer was taken without the cookie of the browser that started")
	}
	if res, _ := e.browse(back, session, connect); res.StatusCode != 303 {
		t.Fatalf("back from tidoly: %d", res.StatusCode)
	}
	if l := e.api.Link(); l.Slug != "mom" || l.Host != "mom.stow.tidoly.app" || l.Key != "tdly_stow_secret" {
		t.Fatalf("link: %+v", l)
	}
	if l, _ := told.Load().(Link); l.Slug != "mom" {
		t.Error("the tunnel was not told")
	}
	if raw := e.store.Setting(ctx, linkSetting); !strings.Contains(raw, "tdly_stow_secret") {
		t.Error("the link is not kept for the next start")
	}
	if _, err := e.api.fetchMembers(ctx); err != nil || members.Load() != "Bearer tdly_stow_secret mom" {
		t.Errorf("member list: %v, asked with %q", err, members.Load())
	}
	// no tunnel server in the answer: asked of home with the key, kept, and the tunnel told
	if l, _ := told.Load().(Link); l.Frps != "" {
		t.Fatalf("a tunnel server before tidoly named one: %+v", l)
	}
	if err := e.api.fetchTunnel(ctx); err != nil || e.api.Link().Frps != "frps.stow.test:7000" {
		t.Fatalf("tunnel server: %v, %+v", err, e.api.Link())
	}
	if l, _ := told.Load().(Link); l.Frps != "frps.stow.test:7000" || l.Key != "tdly_stow_secret" {
		t.Errorf("the tunnel was not told of its server: %+v", l)
	}
	if !strings.Contains(e.store.Setting(ctx, linkSetting), "frps.stow.test:7000") {
		t.Error("the tunnel server is not kept for the next start")
	}
	_, page := e.browse("/admin", session)
	if !strings.Contains(page, "https://mom.stow.tidoly.app") || strings.Contains(page, "tdly_stow_secret") {
		t.Error("the admin page must show the address outside, and never the key")
	}
	if !regexp.MustCompile(`사용자 <button[^>]*data-act="add-user".*?<form class="switch" method="post" action="/admin/tidoly/auto"[^>]*title="tidoly 에 등록한 계정을 바로 사용합니다[^"]*"><label><input type="checkbox" name="on" value="1" role="switch" checked>`).MatchString(page) {
		t.Error("the users heading must carry the switch for tidoly's accounts, on at first, with its tooltip")
	}
	off, _ := http.NewRequest("POST", e.server.URL+"/admin/tidoly/auto", strings.NewReader("")) // the box unticked sends no "on"
	off.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	off.AddCookie(session)
	if res, err := noFollow.Do(off); err != nil || res.StatusCode != 303 || e.api.AutoAdd(ctx) {
		t.Errorf("the switch turned off: %v, still %v", err, e.api.AutoAdd(ctx))
	}
	e.api.SetAutoAdd(ctx, true)
	if strings.Contains(page, "data-refresh") {
		t.Error("the admin page looks again with nothing on its way")
	}
	_, status := e.browse("/status", session)
	far, near := strings.Index(status, `<code>https://mom.stow.tidoly.app</code>`), strings.Index(status, "<code>http://light.test</code>")
	if far < 0 || near < 0 || far > near || !strings.Contains(status, "어디서나") || !strings.Contains(status, "집 안에서만") {
		t.Errorf("connected, the phone's address that works anywhere must come first, the one at home after it")
	}
	if !strings.Contains(status, "서버 주소에 넣습니다: https://mom.stow.tidoly.app") || !strings.Contains(status, "Login with tidoly") {
		t.Error("connected, the family invite must give the address outside and tidoly's login")
	}
	outsideBusy.Store(true)
	_, page = e.browse("/admin", session)
	outsideBusy.Store(false)
	if !strings.Contains(page, `data-refresh="5"`) {
		t.Error("the admin page does not look again while the tunnel or certificate is on its way")
	}
	// disconnected from the first steps' page: back there, and the old address is offered again
	unlink, _ := http.NewRequest("POST", e.server.URL+"/admin/tidoly/disconnect", strings.NewReader("next=%2Foutside"))
	unlink.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unlink.AddCookie(session)
	if res, err := noFollow.Do(unlink); err != nil || res.Header.Get("Location") != "/outside" {
		t.Fatalf("disconnect from the first steps: %v to %q", err, res.Header.Get("Location"))
	}
	if e.api.Link().Slug != "" || e.store.Setting(ctx, linkSetting) != "" {
		t.Errorf("disconnect: %+v", e.api.Link())
	}
	_, step := e.browse("/outside", session)
	if !strings.Contains(step, "전에 쓰던 주소 <b>https://mom.stow.tidoly.app</b>") || strings.Contains(step, "한 주 30명") ||
		!strings.Contains(step, `href="/admin/tidoly/connect?next=/outside" data-connect>다시 연결</a>`) {
		t.Error("not connected now but before: the old address and 다시 연결, not the newcomer's words")
	}
	// connecting from the first steps comes back to them
	res, _ = e.browse("/admin/tidoly/connect?next=/outside", session)
	to, _ = url.Parse(res.Header.Get("Location"))
	lastChallenge.Store(to.Query().Get("cc"))
	var cookies []*http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "stow_connect" || c.Name == "stow_connect_next" {
			cookies = append(cookies, c)
		}
	}
	back = "/admin/tidoly/back?" + url.Values{"code": {"the-code"}, "state": {to.Query().Get("state")}}.Encode()
	if res, _ := e.browse(back, append([]*http.Cookie{session}, cookies...)...); res.Header.Get("Location") != "/outside" {
		t.Errorf("connected from the first steps: to %q", res.Header.Get("Location"))
	}
	e.api.Disconnect(ctx)

	// from the app's window: a link good once starts it in a browser that is not logged in here, and that
	// browser takes tidoly's answer — the window then sees the link made
	req, _ = http.NewRequest("POST", e.server.URL+"/admin/tidoly/connect-link", nil)
	req.AddCookie(session)
	res, err = noFollow.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("connect link: %v %d", err, res.StatusCode)
	}
	var minted struct{ URL string }
	json.NewDecoder(res.Body).Decode(&minted)
	res.Body.Close()
	if res, _ := e.browse(minted.URL[:strings.Index(minted.URL, "ot=")+3] + "forged"); res.Header.Get("Location") != "/login" {
		t.Errorf("a made-up link: to %q", res.Header.Get("Location"))
	}
	res, _ = e.browse(minted.URL) // the browser: no session here
	to, _ = url.Parse(res.Header.Get("Location"))
	if !strings.HasPrefix(to.String(), home.URL+"/stow/connect?") {
		t.Fatalf("the link in a browser: %d to %s", res.StatusCode, to)
	}
	lastChallenge.Store(to.Query().Get("cc"))
	var browser []*http.Cookie
	for _, c := range res.Cookies() {
		browser = append(browser, c)
	}
	if res, _ := e.browse(minted.URL); res.Header.Get("Location") != "/login" {
		t.Errorf("the link a second time: to %q", res.Header.Get("Location"))
	}
	back = "/admin/tidoly/back?" + url.Values{"code": {"the-code"}, "state": {to.Query().Get("state")}}.Encode()
	res, done := e.browse(back, browser...)
	if res.StatusCode != 200 || !strings.Contains(done, "tidoly 에 연결했습니다") || e.api.Link().Slug != "mom" {
		t.Fatalf("back in the browser: %d, %+v", res.StatusCode, e.api.Link())
	}
	_, state := e.browse("/admin/tidoly/state", session)
	if !strings.Contains(state, `"connected":true`) {
		t.Errorf("the window's look: %s", state)
	}
}

var lastChallenge atomic.Value

// Once this server has its own certificate, plain http that came through the tunnel is sent to https;
// at home nothing changes, and before the certificate nothing does either.
func TestHTTPOutsideGoesToHTTPSOnceThereIsACertificate(t *testing.T) {
	ownAccounts = true
	defer func() { ownAccounts = false }()
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	if err := e.api.setLink(ctx, Link{Slug: "mom", Host: "mom.stow.tidoly.app", Key: "tdly_stow_secret"}); err != nil {
		t.Fatal(err)
	}
	var have atomic.Bool
	e.api.cfg.HTTPS = func(host string) bool { return have.Load() && host == "mom.stow.tidoly.app" }
	get := func(host string) *http.Response {
		req, _ := http.NewRequest("GET", e.server.URL+"/api/server/ping?x=1", nil)
		req.Host = host
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := get("mom.stow.tidoly.app"); res.StatusCode != 200 {
		t.Errorf("before the certificate: %d", res.StatusCode)
	}
	have.Store(true)
	if res := get("mom.stow.tidoly.app"); res.StatusCode != 308 || res.Header.Get("Location") != "https://mom.stow.tidoly.app/api/server/ping?x=1" {
		t.Errorf("through the tunnel: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := get("192.168.0.10:30891"); res.StatusCode != 200 {
		t.Errorf("at home: %d", res.StatusCode)
	}
}

// Home writes the tunnel server as frpc's own settings; "host:port" is taken too.
func TestTheTunnelServerAsHomeWritesIt(t *testing.T) {
	for in, want := range map[string]string{
		`{"serverAddr":"stow-tunnel.tidoly.app","serverPort":7001}`: "stow-tunnel.tidoly.app:7001",
		`"stow-tunnel.tidoly.app:7001"`:                             "stow-tunnel.tidoly.app:7001",
		`{"serverAddr":"stow-tunnel.tidoly.app"}`:                   "stow-tunnel.tidoly.app",
		`null`: "",
		`{}`:   "",
	} {
		var a frpsAddress
		if err := json.Unmarshal([]byte(in), &a); err != nil || string(a) != want {
			t.Errorf("%s: %q %v, want %q", in, a, err, want)
		}
	}
}

// In the line for a tunnel, home tells the place and when the next place opens; out of it, it leaves the
// field out.
func TestTheTunnelLineIsAskedOfHome(t *testing.T) {
	var queued atomic.Bool
	queued.Store(true)
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stow/tunnel" || r.Header.Get("Authorization") != "Bearer tdly_stow_secret" {
			w.WriteHeader(401)
			return
		}
		answer := map[string]any{"frps": map[string]any{"serverAddr": "frps.stow.test", "serverPort": 7000}}
		if queued.Load() {
			answer["waiting"] = map[string]any{"position": 4, "opensAt": "2026-10-26T00:00:00.000Z"}
		}
		json.NewEncoder(w).Encode(answer)
	}))
	defer home.Close()
	e := start(t, t.TempDir(), newKey(t))
	e.api.cfg.HomeURL = home.URL
	ctx := context.Background()
	if _, _, ok := e.api.TunnelWaiting(ctx); ok {
		t.Error("not connected to tidoly, yet in a line")
	}
	if err := e.api.setLink(ctx, Link{Slug: "mom", Host: "mom.stow.tidoly.app", Key: "tdly_stow_secret"}); err != nil {
		t.Fatal(err)
	}
	position, opens, ok := e.api.TunnelWaiting(ctx)
	if !ok || position != 4 || !opens.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("in the line: %d %v %v", position, opens, ok)
	}
	queued.Store(false)
	if _, _, ok := e.api.TunnelWaiting(ctx); ok {
		t.Error("out of the line, yet in it")
	}
}
