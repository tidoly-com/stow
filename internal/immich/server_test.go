package immich

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"

	"github.com/tidoly-com/stow/internal/coupons"
	"github.com/tidoly-com/stow/internal/disk"
	"github.com/tidoly-com/stow/internal/faces"
	"github.com/tidoly-com/stow/internal/jobs"
	"github.com/tidoly-com/stow/internal/library"
	"github.com/tidoly-com/stow/internal/media/mediatest"
	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/thumbs"
	"github.com/tidoly-com/stow/internal/ticket"
	"github.com/tidoly-com/stow/internal/vpn"
	"github.com/tidoly-com/stow/internal/web"
)

const (
	testHome = "https://home.test"
	testSlug = "demo"
)

var ffmpegPath, _ = exec.LookPath("ffmpeg") // empty when not installed: HEIC and video tests skip

var withFaces bool // start() gives the server a face helper only while this is set

var outsideBusy atomic.Bool // the tunnel or certificate on its way, as the admin page is told

var withCoupons bool // start() gives the server a coupon helper only while this is set

// env is one run of the server over a data folder; start it twice on the same folder to play a restart.
type env struct {
	t        *testing.T
	dir      string
	key      ed25519.PrivateKey
	store    *store.Store
	server   *httptest.Server
	api      *Server
	stopJobs func()
}

// The test folders are all on one disk: adding a place there is allowed except where a test says not.
func init() {
	sameDisk = func(a, b string) (bool, error) { return false, nil }
	tooSmall = 0 // the test pictures are a few hundred bytes
}

// ownAccounts makes the next server started one not connected to tidoly: it has only its own accounts.
var ownAccounts bool

func start(t *testing.T, dir string, key ed25519.PrivateKey) *env {
	t.Helper()
	hidden, err := library.Hidden(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(hidden, "stow.db"))
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := jobs.NewRunner(st)
	ctx, cancel := context.WithCancel(context.Background())
	var helper *faces.Helper
	if withFaces {
		helper = faces.New([]string{os.Args[0], "-test.run=TestFakeFaceHelper", "--", "fake-face-helper"})
		t.Cleanup(helper.Close)
	}
	var couponHelp *coupons.Helper
	if withCoupons {
		couponHelp = coupons.New([]string{os.Args[0], "-test.run=TestFakeCouponHelper", "--", "fake-coupon-helper"})
		t.Cleanup(couponHelp.Close)
	}
	// the address the tests reach the server at is the one tidoly sends logins back to, as with a trial
	server := httptest.NewUnstartedServer(nil)
	link := Link{Slug: testSlug}
	if ownAccounts {
		link = Link{}
	}
	api := New(Config{PublicURL: "http://" + server.Listener.Addr().String(), HomeURL: testHome, Faces: helper, FaceThreshold: 0.363, Coupons: couponHelp,
		HomeKey: key.Public().(ed25519.PublicKey), Link: link,
		JobKinds: []string{"metadata", "thumbnail"},
		Thumbs:   &thumbs.Maker{Dir: filepath.Join(hidden, "derived"), FFmpeg: ffmpegPath}}, st, lib, runner)
	if err := api.LoadLocations(ctx); err != nil {
		t.Fatal(err)
	}
	pages := &web.Web{Store: st, User: api.SessionUser, Address: "http://light.test", FacesState: api.FacesState, SetFaces: api.SetFaces, CouponDays: api.CouponDays(), SetCoupons: api.SetCoupons, ScreenshotsOn: api.ScreenshotsOn, SetScreenshots: api.SetScreenshots, Changed: api.Changed, Events: api.WebEvents,
		Dir: dir, ResetFile: filepath.Join(dir, "owner-reset"), Contact: testHome + "/contact", Version: "test-1",
		VPN: &vpn.Manager{VPN: &vpn.VPN{Port: freeUDPPort(t), Handler: api}, Settings: st}, LocationDir: api.LocationDir, AddLocation: api.AddLocation,
		NeedsOwner: api.NeedsOwner, LocalLogin: api.WebLocalLogin, AddUser: api.AddLocalUser, SetPassword: api.SetLocalPassword, ChangePassword: api.ChangeOwnPassword,
		TidolyHere: api.TidolyHere, Connect: api.ConnectStart, ConnectBack: api.ConnectBack, Disconnect: api.Disconnect,
		Outside: func(ctx context.Context) web.Outside {
			return web.Outside{Host: api.Link().Host, Problem: api.LinkProblem(), AutoAdd: api.AutoAdd(ctx), Busy: outsideBusy.Load(), Last: api.LastHost(ctx)}
		},
		SetAutoAdd: api.SetAutoAdd, Block: api.Block, Arrivals: api.Arrivals, SeenArrivals: api.SeenArrivals,
		AddFolder: api.AddFolder, PauseFolder: api.PauseFolder, ScanFolder: api.ScanFolder, RemoveFolder: api.RemoveFolder,
		FolderState: func(id string) (web.FolderState, bool) {
			scan := api.FolderScan(id)
			if scan == nil {
				return web.FolderState{}, false
			}
			return web.FolderState{Running: scan.Running.Load(), Seen: scan.Seen.Load(), Added: scan.Added.Load(), Same: scan.Same.Load(), Gone: scan.Gone.Load()}, true
		},
		Trash: api.Trash, Restore: api.Restore, Empty: api.EmptyTrash, Days: TrashDays,
		Links: web.LinkFuncs{On: api.LinksOn, Open: api.OpenLink, Unlock: api.UnlockLink, New: api.NewSharedLink, Address: api.LinkAddress, Hash: HashPassword}}
	api.Pages(pages.Handler(api.WebLoginStart, api.WebLogout))
	runner.Handle("metadata", api.ReadMetadata)
	runner.Handle("place", api.NamePlace)
	runner.Handle("thumbnail", api.MakeThumbnail)
	if withFaces {
		runner.Handle("face", api.FindFaces)
		if _, err := api.QueueMissingFaces(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runner.Handle("coupon", api.FindCoupon)
	if _, err := api.QueueMissingThumbnails(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { runner.Run(ctx, 2); close(done) }()
	server.Config.Handler = api
	server.Start()
	e := &env{t: t, dir: dir, key: key, store: st, server: server, api: api}
	e.stopJobs = func() { cancel(); <-done }
	t.Cleanup(e.stop)
	return e
}

func (e *env) stop() {
	e.server.Close()
	e.stopJobs()
	e.store.Close()
}

func (e *env) userID(email string) string {
	u, err := e.store.UpsertUser(context.Background(), "sub-"+email, email, email, email == "owner@example.com")
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

// settle waits until no job is queued or running.
func (e *env) settle() map[string]int {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if counts, err := e.store.JobCounts(context.Background()); err == nil && counts["queued"]+counts["running"] == 0 {
			return counts
		}
	}
	e.t.Fatal("jobs did not settle")
	return nil
}

func newKey(t *testing.T) ed25519.PrivateKey {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

var b64 = base64.RawURLEncoding

// signTicket plays tidoly-home.
func signTicket(key ed25519.PrivateKey, c ticket.Claims) string {
	header := b64.EncodeToString([]byte(`{"alg":"EdDSA","typ":"stow-ticket"}`))
	payload, _ := json.Marshal(c)
	body := header + "." + b64.EncodeToString(payload)
	return body + "." + b64.EncodeToString(ed25519.Sign(key, []byte(body)))
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64.EncodeToString(sum[:])
}

func (e *env) do(method, path, token string, body io.Reader, contentType string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.server.URL+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieToken, Value: token})
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func decode(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	defer res.Body.Close()
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return body
}

// callback posts a ticket the way the app does after the browser comes back.
func (e *env) callback(c ticket.Claims, verifier string) *http.Response {
	back := "app.immich:///oauth-callback?" + url.Values{"code": {signTicket(e.key, c)}, "state": {"s"}}.Encode()
	body, _ := json.Marshal(map[string]string{"url": back, "state": "s", "codeVerifier": verifier})
	return e.do("POST", "/api/oauth/callback", "", bytes.NewReader(body), "application/json")
}

func claimsFor(email, role, verifier string) ticket.Claims {
	return ticket.Claims{Issuer: testHome, Slug: testSlug, Subject: "sub-" + email, Email: email, Name: email, Role: role,
		Challenge: challenge(verifier), Expires: time.Now().Add(time.Minute).Unix(), ID: store.NewID()}
}

// login returns the session token, taken from the cookie because that is all the app keeps. The owner has
// an account here before coming with tidoly, as one made at home when the server was set up.
func (e *env) login(email, role string) string {
	e.t.Helper()
	if role == "owner" {
		e.userID(email)
	}
	res := e.callback(claimsFor(email, role, "verifier-"+email), "verifier-"+email)
	defer res.Body.Close()
	if res.StatusCode != 201 {
		e.t.Fatalf("login: %d", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == cookieToken {
			return c.Value
		}
	}
	e.t.Fatal("login set no access token cookie")
	return ""
}

func form(name string, data []byte, createdAt string) (io.Reader, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("fileCreatedAt", createdAt)
	w.WriteField("fileModifiedAt", createdAt)
	part, _ := w.CreateFormFile("assetData", name)
	part.Write(data)
	w.Close()
	return &buf, w.FormDataContentType()
}

func (e *env) upload(token, name string, data []byte) (int, map[string]any) {
	e.t.Helper()
	body, ctype := form(name, data, "2026-09-25T06:13:18.000Z")
	res := e.do("POST", "/api/assets", token, body, ctype)
	return res.StatusCode, decode(e.t, res)
}

func (e *env) sync(token string) []map[string]any {
	e.t.Helper()
	res := e.do("POST", "/api/sync/stream", token, strings.NewReader(`{"types":["AuthUsersV2","UsersV1","AssetsV2","AssetExifsV1","AlbumsV2","AlbumUsersV1","AlbumToAssetsV1","PeopleV1","AssetFacesV3"]}`), "application/json")
	defer res.Body.Close()
	if res.StatusCode != 200 {
		e.t.Fatalf("sync: %d", res.StatusCode)
	}
	var lines []map[string]any
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(nil, 1<<20)
	for scan.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scan.Bytes(), &line); err != nil {
			e.t.Fatalf("sync line: %v", err)
		}
		lines = append(lines, line)
	}
	return lines
}

func count(lines []map[string]any, kind string) (n int) {
	for _, l := range lines {
		if l["type"] == kind {
			n++
		}
	}
	return
}

func filesUnder(t *testing.T, dir string) (names []string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	return
}

func TestNothingWithoutASession(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	_, up := e.upload(token, "a.jpg", []byte("photo"))
	id := up["id"].(string)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/users/me"}, {"POST", "/api/sync/stream"}, {"POST", "/api/sync/ack"}, {"POST", "/api/assets"},
		{"GET", "/api/assets/" + id + "/original"}, {"GET", "/api/assets/" + id + "/thumbnail"},
		{"GET", "/api/assets/" + id + "/video/playback"}, {"POST", "/api/auth/validateToken"}, {"GET", "/api/server/storage"},
	} {
		for _, bad := range []string{"", "not-a-token"} {
			res := e.do(c.method, c.path, bad, nil, "")
			res.Body.Close()
			if res.StatusCode != 401 {
				t.Errorf("%s %s with token %q: %d, want 401", c.method, c.path, bad, res.StatusCode)
			}
		}
	}
	res := e.do("POST", "/api/auth/login", "", strings.NewReader(`{"email":"a@b.c","password":"x"}`), "application/json")
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("password login: %d, want 401", res.StatusCode)
	}
}

func TestTicketRefusals(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	good := claimsFor("a@example.com", "member", "v")
	cases := map[string]func() *http.Response{
		"another slug": func() *http.Response { c := good; c.Slug = "other"; c.ID = "1"; return e.callback(c, "v") },
		"expired": func() *http.Response {
			c := good
			c.Expires = time.Now().Add(-time.Second).Unix()
			c.ID = "2"
			return e.callback(c, "v")
		},
		"another issuer": func() *http.Response {
			c := good
			c.Issuer = "https://evil.test"
			c.ID = "3"
			return e.callback(c, "v")
		},
		"stolen on the way": func() *http.Response { c := good; c.ID = "4"; return e.callback(c, "someone-else") },
		"signed by someone else": func() *http.Response {
			other := *e
			other.key = newKey(t)
			c := good
			c.ID = "5"
			return other.callback(c, "v")
		},
	}
	for name, call := range cases {
		res := call()
		res.Body.Close()
		if res.StatusCode != 401 || len(res.Cookies()) != 0 {
			t.Errorf("%s: %d with %d cookies, want 401 and none", name, res.StatusCode, len(res.Cookies()))
		}
	}
	first := e.callback(good, "v")
	first.Body.Close()
	second := e.callback(good, "v")
	second.Body.Close()
	if first.StatusCode != 201 || second.StatusCode != 401 {
		t.Errorf("same ticket twice: %d then %d, want 201 then 401", first.StatusCode, second.StatusCode)
	}
}

func TestUploadSurvivesRestart(t *testing.T) {
	dir, key := t.TempDir(), newKey(t)
	e := start(t, dir, key)
	token := e.login("owner@example.com", "owner")
	status, up := e.upload(token, "IMG_0001.jpg", []byte("first photo"))
	if status != 201 || up["status"] != "created" {
		t.Fatalf("upload: %d %v", status, up)
	}
	e.stop()

	e = start(t, dir, key)
	if res := e.do("GET", "/api/users/me", token, nil, ""); res.StatusCode != 200 {
		t.Fatalf("session did not survive the restart: %d", res.StatusCode)
	}
	lines := e.sync(token)
	if count(lines, "AssetV2") != 1 || count(lines, "AssetExifV1") != 1 || count(lines, "SyncCompleteV1") != 1 {
		t.Fatalf("after restart the stream has %d assets, %d exifs", count(lines, "AssetV2"), count(lines, "AssetExifV1"))
	}
	res := e.do("GET", "/api/assets/"+up["id"].(string)+"/original", token, nil, "")
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(got) != "first photo" {
		t.Fatalf("original after restart: %q", got)
	}
	// the owner finds the file without this program: person / year / month / the phone's name
	year, month := time.Date(2026, 9, 25, 6, 13, 18, 0, time.UTC).Local().Format("2006"), time.Date(2026, 9, 25, 6, 13, 18, 0, time.UTC).Local().Format("01")
	want := fmt.Sprintf("owner@example.com/%s/%s/IMG_0001.jpg", year, month)
	if files := filesUnder(t, filepath.Join(dir, "originals")); len(files) != 1 || files[0] != want {
		t.Fatalf("originals holds %v, want [%s]", files, want)
	}
	if counts := e.settle(); counts["done"] != 2 || counts["failed"] != 0 {
		t.Fatalf("jobs after restart: %v, want both jobs done", counts)
	}
}

func TestDuplicateAndSameName(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	token := e.login("owner@example.com", "owner")
	_, first := e.upload(token, "IMG_0001.jpg", []byte("one"))
	status, again := e.upload(token, "IMG_0001.jpg", []byte("one"))
	if status != 200 || again["status"] != "duplicate" || again["id"] != first["id"] {
		t.Fatalf("same bytes again: %d %v", status, again)
	}
	status, other := e.upload(token, "IMG_0001.jpg", []byte("two")) // another photo that happens to share the name
	if status != 201 || other["id"] == first["id"] {
		t.Fatalf("same name, other bytes: %d %v", status, other)
	}
	files := filesUnder(t, filepath.Join(dir, "originals"))
	if len(files) != 2 || !strings.HasSuffix(files[0], "/IMG_0001 (2).jpg") || !strings.HasSuffix(files[1], "/IMG_0001.jpg") {
		t.Fatalf("originals holds %v", files)
	}
	if left := filesUnder(t, filepath.Join(dir, ".stow", "tmp")); len(left) != 0 {
		t.Fatalf("tmp still holds %v", left)
	}
	// another member uploading the same bytes keeps a copy of their own
	other2 := e.login("kid@example.com", "member")
	if status, up := e.upload(other2, "IMG_0001.jpg", []byte("one")); status != 201 || up["status"] != "created" {
		t.Fatalf("second member: %d %v", status, up)
	}
}

func TestCutUploadLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	token := e.login("owner@example.com", "owner")

	// send the head of a form that promises more than it delivers, then hang up
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	head := "--B\r\nContent-Disposition: form-data; name=\"assetData\"; filename=\"big.mp4\"\r\n\r\n" + strings.Repeat("x", 200_000)
	fmt.Fprintf(conn, "POST /api/assets HTTP/1.1\r\nHost: x\r\nCookie: %s=%s\r\nContent-Type: multipart/form-data; boundary=B\r\nContent-Length: 50000000\r\n\r\n%s", cookieToken, token, head)
	conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for len(filesUnder(t, filepath.Join(dir, ".stow", "tmp"))) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if left := filesUnder(t, filepath.Join(dir, ".stow", "tmp")); len(left) != 0 {
		t.Fatalf("tmp still holds %v after the phone hung up", left)
	}
	if files := filesUnder(t, filepath.Join(dir, "originals")); len(files) != 0 {
		t.Fatalf("a cut upload reached originals: %v", files)
	}
	if lines := e.sync(token); count(lines, "AssetV2") != 0 {
		t.Fatal("a cut upload was recorded")
	}
	if status, up := e.upload(token, "big.mp4", []byte("the whole file this time")); status != 201 || up["status"] != "created" {
		t.Fatalf("upload after the cut one: %d %v", status, up)
	}
}

func TestSyncSendsOnlyWhatIsNew(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	e.upload(token, "a.jpg", []byte("a"))
	e.upload(token, "b.jpg", []byte("b"))
	lines := e.sync(token)
	if count(lines, "AuthUserV2") != 1 || count(lines, "AssetV2") != 2 {
		t.Fatalf("first sync: %d users, %d assets", count(lines, "AuthUserV2"), count(lines, "AssetV2"))
	}
	var acks []string
	for _, l := range lines {
		acks = append(acks, l["ack"].(string))
	}
	body, _ := json.Marshal(map[string]any{"acks": acks})
	if res := e.do("POST", "/api/sync/ack", token, bytes.NewReader(body), "application/json"); res.StatusCode != 204 {
		t.Fatalf("ack: %d", res.StatusCode)
	}
	e.upload(token, "c.jpg", []byte("c"))
	lines = e.sync(token)
	if count(lines, "AuthUserV2") != 0 || count(lines, "AssetV2") != 1 || count(lines, "AssetExifV1") != 1 {
		t.Fatalf("after ack: %d users, %d assets, %d exifs — want only the new photo", count(lines, "AuthUserV2"), count(lines, "AssetV2"), count(lines, "AssetExifV1"))
	}
	// a second device of the same person starts from nothing
	phone2 := e.login("owner@example.com", "owner")
	if lines := e.sync(phone2); count(lines, "AssetV2") != 3 {
		t.Fatalf("new session got %d assets, want 3", count(lines, "AssetV2"))
	}
}

func TestOnePersonCannotReachAnothersPhotos(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner, kid := e.login("owner@example.com", "owner"), e.login("kid@example.com", "member")
	_, up := e.upload(owner, "a.jpg", []byte("private"))
	for _, path := range []string{"/original", "/thumbnail", "/video/playback"} {
		res := e.do("GET", "/api/assets/"+up["id"].(string)+path, kid, nil, "")
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("%s as another member: %d, want 404", path, res.StatusCode)
		}
	}
	if lines := e.sync(kid); count(lines, "AssetV2") != 0 {
		t.Error("another member's sync lists the photo")
	}
}

func TestRemovedMemberAndLogout(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner, kid := e.login("owner@example.com", "owner"), e.login("kid@example.com", "member")
	if n, err := e.store.DropTidolySessionsExcept(context.Background(), []string{"OWNER@example.com"}); err != nil || n != 1 {
		t.Fatalf("drop: %d %v", n, err)
	}
	if res := e.do("GET", "/api/users/me", kid, nil, ""); res.StatusCode != 401 {
		t.Errorf("removed member: %d, want 401", res.StatusCode)
	}
	if res := e.do("GET", "/api/users/me", owner, nil, ""); res.StatusCode != 200 {
		t.Errorf("remaining member: %d, want 200", res.StatusCode)
	}
	if res := e.do("POST", "/api/auth/logout", owner, nil, ""); res.StatusCode != 200 {
		t.Errorf("logout: %d", res.StatusCode)
	}
	if res := e.do("GET", "/api/users/me", owner, nil, ""); res.StatusCode != 401 {
		t.Errorf("after logout: %d, want 401", res.StatusCode)
	}
}

// The app learns that an upload is in the library from a socket.io event, not from the upload's answer.
func TestUploadIsAnnouncedToThePersonsDevices(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner, kid := e.login("owner@example.com", "owner"), e.login("kid@example.com", "member")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/api/socket.io/?EIO=4&transport=websocket"

	if _, res, err := websocket.Dial(ctx, address, nil); err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("socket without a session: %v, want 401", err)
	}
	connect := func(token string) *websocket.Conn {
		conn, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {cookieToken + "=" + token}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		if _, open, _ := conn.Read(ctx); !strings.HasPrefix(string(open), `0{`) {
			t.Fatalf("open packet: %q", open)
		}
		conn.Write(ctx, websocket.MessageText, []byte("40"))
		if _, joined, _ := conn.Read(ctx); !strings.HasPrefix(string(joined), `40{"sid"`) {
			t.Fatalf("namespace answer: %q", joined)
		}
		return conn
	}
	ownerPhone, kidPhone := connect(owner), connect(kid)

	_, up := e.upload(owner, "a.jpg", mediatest.JPEG(nil, 10, 10))
	// two things reach the phone, in either order: the photo is ready to show, and its details were read
	var ready, updated bool
	for i := 0; i < 2; i++ {
		_, packet, err := ownerPhone.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var event []json.RawMessage
		var payload struct {
			Asset struct{ ID, Checksum string }
			Exif  struct{ AssetID string }
		}
		if !strings.HasPrefix(string(packet), "42") || json.Unmarshal(packet[2:], &event) != nil || len(event) != 2 {
			t.Fatalf("event: %s", packet)
		}
		switch string(event[0]) {
		case `"on_asset_update"`:
			updated = true
		case `"AssetUploadReadyV2"`:
			ready = true
			if json.Unmarshal(event[1], &payload) != nil || payload.Asset.ID != up["id"] || payload.Exif.AssetID != up["id"] || payload.Asset.Checksum == "" {
				t.Fatalf("event payload: %s", packet)
			}
			// by the time the phone hears of it, the small picture is there to fetch
			if res := e.do("GET", "/api/assets/"+up["id"].(string)+"/thumbnail", owner, nil, ""); res.Header.Get("Content-Type") != "image/jpeg" {
				t.Errorf("thumbnail at the moment of the event: %s", res.Header.Get("Content-Type"))
			}
		}
	}
	if !ready || !updated {
		t.Fatalf("ready: %v, updated: %v", ready, updated)
	}

	// the other member's phone hears nothing about it
	quiet, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	if _, packet, err := kidPhone.Read(quiet); err == nil {
		t.Fatalf("another member received %s", packet)
	}
}

func lineFor(lines []map[string]any, kind, idKey, id string) map[string]any {
	for _, l := range lines {
		if data, _ := l["data"].(map[string]any); l["type"] == kind && data[idKey] == id {
			return data
		}
	}
	return nil
}

// The phone says when it got the file; the file says when the photo was taken. The timeline follows the file.
func TestWhatTheFileSaysReachesTheApp(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	_, up := e.upload(token, "20260925_151318.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)) // sent as created 06:13:18Z
	old := mediatest.Camera
	old.Taken, old.Offset = "2019:01:02 08:00:00", "-05:00"
	_, upOld := e.upload(token, "received-today.jpg", mediatest.JPEG(old.Bytes(), 400, 300))
	_, upBare := e.upload(token, "Screenshot.jpg", mediatest.JPEG(nil, 1080, 2340))
	if counts := e.settle(); counts["done"] != 8 || counts["failed"] != 0 { // 3 read, 3 pictured, 2 with coordinates to name
		t.Fatalf("jobs: %v, want 8 done", counts)
	}
	lines := e.sync(token)

	asset, exif := lineFor(lines, "AssetV2", "id", up["id"].(string)), lineFor(lines, "AssetExifV1", "assetId", up["id"].(string))
	if asset["fileCreatedAt"] != "2026-09-25T06:13:18.000Z" || asset["localDateTime"] != "2026-09-25T15:13:18.000Z" {
		t.Errorf("camera photo times: %v / %v", asset["fileCreatedAt"], asset["localDateTime"])
	}
	if asset["width"] != 300.0 || asset["height"] != 400.0 { // stored on its side, shown upright
		t.Errorf("camera photo shown as %vx%v, want 300x400", asset["width"], asset["height"])
	}
	for key, want := range map[string]any{"make": "samsung", "model": "SM-S938N", "orientation": "6", "exifImageWidth": 400.0, "exifImageHeight": 300.0,
		"iso": 50.0, "fNumber": 1.8, "focalLength": 6.3, "exposureTime": "1/120", "timeZone": "UTC+9", "dateTimeOriginal": "2026-09-25T06:13:18.000Z",
		"lensModel": "Galaxy S25+ Rear Camera"} {
		if exif[key] != want {
			t.Errorf("exif %s: %v, want %v", key, exif[key], want)
		}
	}
	if lat, _ := exif["latitude"].(float64); lat < 37.5664 || lat > 37.5666 {
		t.Errorf("latitude: %v", exif["latitude"])
	}
	if exif["city"] != "중구" || exif["state"] != "서울특별시" || exif["country"] != "대한민국" {
		t.Errorf("place: %v, %v, %v — want 중구, 서울특별시, 대한민국", exif["city"], exif["state"], exif["country"])
	}

	// taken years before it was uploaded: the timeline shows it where it was taken, not where it arrived
	asset, exif = lineFor(lines, "AssetV2", "id", upOld["id"].(string)), lineFor(lines, "AssetExifV1", "assetId", upOld["id"].(string))
	if asset["fileCreatedAt"] != "2019-01-02T13:00:00.000Z" || asset["localDateTime"] != "2019-01-02T08:00:00.000Z" || exif["timeZone"] != "UTC-5" {
		t.Errorf("old photo: %v / %v / %v", asset["fileCreatedAt"], asset["localDateTime"], exif["timeZone"])
	}

	// a file that says nothing keeps the phone's time, and missing location is not a failure
	asset, exif = lineFor(lines, "AssetV2", "id", upBare["id"].(string)), lineFor(lines, "AssetExifV1", "assetId", upBare["id"].(string))
	if asset["fileCreatedAt"] != "2026-09-25T06:13:18.000Z" || asset["width"] != 1080.0 || exif["latitude"] != nil || exif["make"] != nil || exif["city"] != nil {
		t.Errorf("screenshot: %v", asset)
	}
}

func TestMetadataArrivingLaterIsSentAgain(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	_, up := e.upload(token, "a.jpg", mediatest.JPEG(nil, 10, 10))
	e.settle()
	var acks []string
	for _, l := range e.sync(token) {
		acks = append(acks, l["ack"].(string))
	}
	body, _ := json.Marshal(map[string]any{"acks": acks})
	e.do("POST", "/api/sync/ack", token, bytes.NewReader(body), "application/json").Body.Close()
	if lines := e.sync(token); count(lines, "AssetV2") != 0 {
		t.Fatal("nothing changed, yet the asset was sent again")
	}
	lat, lon := 35.1, 129.0
	if err := e.store.SetMeta(context.Background(), up["id"].(string), store.Meta{Lat: &lat, Lon: &lon}, 0); err != nil {
		t.Fatal(err)
	}
	lines := e.sync(token)
	if exif := lineFor(lines, "AssetExifV1", "assetId", up["id"].(string)); count(lines, "AssetV2") != 1 || exif["latitude"] != 35.1 {
		t.Fatalf("after new metadata: %d assets, exif %v", count(lines, "AssetV2"), exif)
	}
}

func TestADamagedFileFailsAloneAndTheQueueGoesOn(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	good := mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)
	_, broken := e.upload(token, "broken.jpg", good[:100]) // cut off inside its headers
	_, fine := e.upload(token, "fine.jpg", good)
	_, unknown := e.upload(token, "notes.txt", []byte("not a picture at all"))
	// the damaged file fails both of its jobs; the good one is read, pictured and placed; the text file has nothing to do
	if counts := e.settle(); counts["failed"] != 2 || counts["done"] != 5 {
		t.Fatalf("jobs: %v, want 2 failed and 5 done", counts)
	}
	lines := e.sync(token)
	if count(lines, "AssetV2") != 3 { // the damaged file is still in the library, just without metadata
		t.Fatalf("assets: %d, want 3", count(lines, "AssetV2"))
	}
	if exif := lineFor(lines, "AssetExifV1", "assetId", fine["id"].(string)); exif["make"] != "samsung" {
		t.Errorf("the good file after the bad one: %v", exif["make"])
	}
	for _, id := range []string{broken["id"].(string), unknown["id"].(string)} {
		if exif := lineFor(lines, "AssetExifV1", "assetId", id); exif["make"] != nil || exif["latitude"] != nil {
			t.Errorf("unreadable file has metadata: %v", exif)
		}
	}
}

func (e *env) picture(token, id, size string) (image.Config, int) {
	e.t.Helper()
	res := e.do("GET", "/api/assets/"+id+"/thumbnail?size="+size, token, nil, "")
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if res.StatusCode != 200 || err != nil {
		e.t.Fatalf("%s of %s: status %d, %v", size, id, res.StatusCode, err)
	}
	return cfg, len(data)
}

// Grids get small pictures, made after the upload and never while someone waits for a page.
func TestSmallPicturesStandUprightAndAreNotTheOriginal(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	token := e.login("owner@example.com", "owner")
	original := mediatest.JPEG(mediatest.Camera.Bytes(), 3200, 2400) // taken with the phone held upright: stored on its side
	_, up := e.upload(token, "portrait.jpg", original)
	_, small := e.upload(token, "icon.jpg", mediatest.JPEG(nil, 80, 60))
	if counts := e.settle(); counts["failed"] != 0 {
		t.Fatalf("jobs: %v", counts)
	}
	id := up["id"].(string)
	thumb, thumbBytes := e.picture(token, id, "thumbnail")
	if thumb.Width != 256 || thumb.Height != 341 {
		t.Errorf("thumbnail is %dx%d, want 256x341 (upright, short side 256)", thumb.Width, thumb.Height)
	}
	preview, previewBytes := e.picture(token, id, "preview")
	if preview.Width != 1080 || preview.Height != 1440 {
		t.Errorf("preview is %dx%d, want 1080x1440", preview.Width, preview.Height)
	}
	if thumbBytes*10 > len(original) || previewBytes >= len(original) {
		t.Errorf("original %d bytes, preview %d, thumbnail %d — the small ones are not small", len(original), previewBytes, thumbBytes)
	}
	t.Logf("original %d bytes, preview %d, thumbnail %d", len(original), previewBytes, thumbBytes)
	if tiny, _ := e.picture(token, small["id"].(string), "thumbnail"); tiny.Width != 80 || tiny.Height != 60 {
		t.Errorf("a picture smaller than a thumbnail came out as %dx%d, want it left at 80x60", tiny.Width, tiny.Height)
	}

	// another member cannot fetch them
	kid := e.login("kid@example.com", "member")
	if res := e.do("GET", "/api/assets/"+id+"/thumbnail", kid, nil, ""); res.StatusCode != 404 {
		t.Errorf("another member: %d, want 404", res.StatusCode)
	}

	// derived/ can be thrown away: the originals are untouched and the pictures come back at the next start
	before, _ := os.ReadFile(filepath.Join(dir, "originals", filesUnder(t, filepath.Join(dir, "originals"))[1]))
	e.stop()
	if err := os.RemoveAll(filepath.Join(dir, ".stow", "derived")); err != nil {
		t.Fatal(err)
	}
	e = start(t, dir, e.key)
	e.settle()
	if again, _ := e.picture(token, id, "thumbnail"); again.Width != 256 {
		t.Errorf("after deleting derived/: thumbnail is %d wide", again.Width)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "originals", filesUnder(t, filepath.Join(dir, "originals"))[1])); !bytes.Equal(before, after) || len(after) == 0 {
		t.Error("an original changed")
	}
}

// Looking at the list must not start any work: before the job has run, the request gets what is there.
func TestAskingForAPictureMakesNone(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	token := e.login("owner@example.com", "owner")
	e.stopJobs() // nothing runs in the background from here on
	e.stopJobs = func() {}
	original := mediatest.JPEG(nil, 800, 600)
	_, up := e.upload(token, "a.jpg", original)
	for i := 0; i < 3; i++ {
		res := e.do("GET", "/api/assets/"+up["id"].(string)+"/thumbnail", token, nil, "")
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !bytes.Equal(data, original) {
			t.Fatalf("before the job ran, the request got %d bytes that are not the original", len(data))
		}
	}
	if made := filesUnder(t, filepath.Join(dir, ".stow", "derived")); len(made) != 0 {
		t.Fatalf("requests made %v", made)
	}
	if counts, _ := e.store.JobCounts(context.Background()); counts["queued"] != 2 {
		t.Fatalf("jobs: %v, want both still queued", counts)
	}
}

func TestVideoGetsAStillWhenFFmpegIsThere(t *testing.T) {
	if ffmpegPath == "" {
		t.Skip("ffmpeg is not installed")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	if out, err := exec.Command(ffmpegPath, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=d=2:s=640x360:r=30", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	turned := filepath.Join(t.TempDir(), "turned.mp4")
	if out, err := exec.Command(ffmpegPath, "-v", "error", "-y", "-display_rotation", "270", "-i", clip, "-c", "copy", turned).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	data, _ := os.ReadFile(turned)
	_, up := e.upload(token, "20260925_135748.mp4", data)
	if counts := e.settle(); counts["failed"] != 0 {
		t.Fatalf("jobs: %v", counts)
	}
	if still, _ := e.picture(token, up["id"].(string), "preview"); still.Width != 360 || still.Height != 640 {
		t.Errorf("still of a video filmed upright is %dx%d, want 360x640", still.Width, still.Height)
	}
}

func TestWithoutFFmpegAVideoIsStillStored(t *testing.T) {
	saved := ffmpegPath
	ffmpegPath = ""
	defer func() { ffmpegPath = saved }()
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	// the smallest thing that reads as a video: a file type box and an empty movie box
	clip := append([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2"), []byte("\x00\x00\x00\x08moov")...)
	status, up := e.upload(token, "clip.mp4", clip)
	if counts := e.settle(); status != 201 || counts["failed"] != 0 {
		t.Fatalf("upload %d, jobs %v — a missing ffmpeg must not fail anything", status, counts)
	}
	res := e.do("GET", "/api/assets/"+up["id"].(string)+"/thumbnail", token, nil, "")
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("placeholder: %d, cache %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
}

// TestFakeFaceHelper is not a test: started with "fake-face-helper" as its last argument, it plays the
// face helper. It "sees" in a photo the people its file name mentions, and describes each person the
// same way every time, give or take a little.
func TestFakeFaceHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "fake-face-helper" {
		return
	}
	cast := map[string][]float32{"alice": {1, 0, 0, 0}, "bob": {0, 1, 0, 0}, "carol": {0, 0, 1, 0}}
	in, out := bufio.NewScanner(os.Stdin), json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct{ Original string }
		json.Unmarshal(in.Bytes(), &req)
		found, n := []map[string]any{}, 0
		names := []string{}
		for _, name := range []string{"alice", "bob", "carol"} {
			for i := strings.Count(req.Original, name); i > 0; i-- {
				names = append(names, name)
			}
		}
		for _, name := range names {
			wobble := float32(len(req.Original)%7) / 50 // never exactly the same twice
			e := cast[name]
			left, wide := 0.1+0.3*float64(n), 0.2
			if strings.Contains(req.Original, "far away") {
				wide = 0.05 // a face in the background
			}
			found = append(found, map[string]any{"box": []float64{left, 0.2, left + wide, 0.5}, "score": 0.9,
				"embedding": []float32{e[0], e[1], e[2], wobble}})
			n++
		}
		out.Encode(map[string]any{"faces": found})
	}
	os.Exit(0)
}

func TestTheSameFaceInTwoPhotosBecomesAPerson(t *testing.T) {
	withFaces = true
	defer func() { withFaces = false }()
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	token := e.login("owner@example.com", "owner")
	photo := func(name string, shade int) string {
		_, up := e.upload(token, name, mediatest.JPEG(nil, 640+shade, 480)) // a different size makes different bytes
		return up["id"].(string)
	}
	alice1 := photo("alice at the beach.jpg", 0)
	bobAlone := photo("bob.jpg", 2)
	if counts := e.settle(); counts["failed"] != 0 {
		t.Fatalf("jobs: %v", counts)
	}
	lines := e.sync(token)
	if count(lines, "PersonV1") != 0 || count(lines, "AssetFaceV3") != 2 {
		t.Fatalf("one photo each: %d people, %d faces — nobody is a person from a single face", count(lines, "PersonV1"), count(lines, "AssetFaceV3"))
	}
	var acks []string
	for _, l := range lines {
		acks = append(acks, l["ack"].(string))
	}
	body, _ := json.Marshal(map[string]any{"acks": acks})
	e.do("POST", "/api/sync/ack", token, bytes.NewReader(body), "application/json").Body.Close()

	both := photo("alice and carol.jpg", 4)
	e.settle()
	lines = e.sync(token)
	var alice map[string]any
	for _, l := range lines {
		if l["type"] == "PersonV1" {
			alice = l["data"].(map[string]any)
		}
	}
	if count(lines, "PersonV1") != 1 || alice == nil {
		t.Fatalf("after a second photo of alice: %d people", count(lines, "PersonV1"))
	}
	// both of her faces point at her — the earlier one is sent again now that it belongs to someone
	hers := 0
	for _, l := range lines {
		data, _ := l["data"].(map[string]any)
		if l["type"] != "AssetFaceV3" {
			continue
		}
		switch {
		case data["personId"] == alice["id"] && (data["assetId"] == alice1 || data["assetId"] == both):
			hers++
			if data["imageWidth"].(float64) < 600 || data["boundingBoxX2"].(float64) <= data["boundingBoxX1"].(float64) {
				t.Errorf("face box: %v", data)
			}
		case data["personId"] != nil:
			t.Errorf("a face that is nobody's got a person: %v", data)
		case data["assetId"] == bobAlone:
			t.Errorf("bob's face was sent again though nothing about it changed")
		}
	}
	if hers != 2 {
		t.Fatalf("faces pointing at alice: %d, want 2", hers)
	}
	res := e.do("GET", "/api/people/"+alice["id"].(string)+"/thumbnail", token, nil, "")
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); res.StatusCode != 200 || err != nil || cfg.Width != 250 || cfg.Height != 250 {
		t.Errorf("her picture: status %d, %v, %dx%d", res.StatusCode, err, cfg.Width, cfg.Height)
	}
	// the same face twice in one photo — a mirror, a collage — is kept once, and joins alice
	photo("alice with alice in a mirror.jpg", 6)
	e.settle()
	joined, alone := 0, 0
	for _, l := range e.sync(e.login("owner@example.com", "owner")) { // a fresh device, to see everything
		if data, _ := l["data"].(map[string]any); l["type"] == "AssetFaceV3" && strings.Contains(fmt.Sprint(data["assetId"]), "") {
			switch {
			case data["boundingBoxX1"] == nil:
			case data["personId"] == alice["id"]:
				joined++
			case data["personId"] == nil:
				alone++
			}
		}
	}
	if joined != 3 || alone != 2 { // beach, with carol, and the larger of the two in the mirror; the other is left out — alone: bob and carol
		t.Errorf("faces that are alice: %d, nobody's: %d, want 3 and 2", joined, alone)
	}

	// a face too small to tell apart is not kept at all, however often it shows up
	photo("bob far away.jpg", 8)
	photo("bob far away again.jpg", 10)
	e.settle()
	if lone, _ := e.store.LoneFaces(context.Background(), e.userID("owner@example.com")); lone != 2 { // bob close up, carol
		t.Errorf("faces that are nobody yet: %d, want 2", lone)
	}

	// putting alice away: the app is told she is hidden, the web list drops her, and a new photo of her stays away
	browser := e.browserLogin("owner@example.com", "owner")
	if _, page := e.browse("/people", browser); !strings.Contains(page, alice["id"].(string)) || !strings.Contains(page, "한 번만 나온 얼굴 2개") {
		t.Errorf("people page before hiding: %s", page)
	}
	hide := func(id string, form url.Values, cookie *http.Cookie) int {
		req, _ := http.NewRequest("POST", e.server.URL+"/people/"+id+"/hide", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if status := hide(alice["id"].(string), nil, e.browserLogin("kid@example.com", "member")); status != 404 {
		t.Errorf("another member hiding her: %d, want 404", status)
	}
	if status := hide(alice["id"].(string), nil, browser); status != 303 {
		t.Fatalf("hide: %d", status)
	}
	photo("alice again.jpg", 12)
	e.settle()
	if _, page := e.browse("/people", browser); strings.Contains(page, `href="/people/`+alice["id"].(string)) || !strings.Contains(page, "숨긴 인물 1") {
		t.Errorf("people page after hiding: %s", page)
	}
	hiddenInSync := false
	for _, l := range e.sync(e.login("owner@example.com", "owner")) {
		if data, _ := l["data"].(map[string]any); l["type"] == "PersonV1" && data["id"] == alice["id"] {
			hiddenInSync = data["isHidden"] == true
		}
	}
	if !hiddenInSync {
		t.Error("the app is not told that she is hidden")
	}
	// naming her on the web page reaches the app; so does naming her in the app
	nameReq, _ := http.NewRequest("POST", e.server.URL+"/people/"+alice["id"].(string)+"/name", strings.NewReader(url.Values{"name": {"  앨리스 "}}.Encode()))
	nameReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	nameReq.AddCookie(browser)
	if res, err := noFollow.Do(nameReq); err != nil || res.StatusCode != 303 {
		t.Fatalf("name: %v", err)
	}
	named := ""
	for _, l := range e.sync(e.login("owner@example.com", "owner")) {
		if data, _ := l["data"].(map[string]any); l["type"] == "PersonV1" && data["id"] == alice["id"] {
			named, _ = data["name"].(string)
		}
	}
	if named != "앨리스" {
		t.Errorf("her name in the app's sync: %q", named)
	}
	if status, raw := e.call("PUT", "/api/people/"+alice["id"].(string), e.login("owner@example.com", "owner"), map[string]any{"name": "Alice"}); status != 200 || !strings.Contains(string(raw), `"name":"Alice"`) {
		t.Errorf("naming her from the app: %d %s", status, raw)
	}
	if status, _ := e.call("PUT", "/api/people/"+alice["id"].(string), e.login("kid@example.com", "member"), map[string]any{"name": "x"}); status != 404 {
		t.Errorf("another member naming her: %d, want 404", status)
	}
	if _, page := e.browse("/people/"+alice["id"].(string), browser); !strings.Contains(page, `value="Alice"`) {
		t.Error("the web page does not show the name given in the app")
	}
	if status := hide(alice["id"].(string), url.Values{"show": {"1"}}, browser); status != 303 {
		t.Fatalf("show again: %d", status)
	}
	if _, page := e.browse("/people", browser); !strings.Contains(page, `href="/people/`+alice["id"].(string)) {
		t.Error("she did not come back")
	}

	kid := e.login("kid@example.com", "member")
	if res := e.do("GET", "/api/people/"+alice["id"].(string)+"/thumbnail", kid, nil, ""); res.StatusCode != 404 {
		t.Errorf("another member asking for her picture: %d, want 404", res.StatusCode)
	}
	if lines := e.sync(kid); count(lines, "PersonV1") != 0 || count(lines, "AssetFaceV3") != 0 {
		t.Error("another member's sync carries the owner's people")
	}
}

// ── the web pages ───────────────────────────────────────────────────────────────────────────────────────────

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) browse(path string, cookies ...*http.Cookie) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.server.URL+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := noFollow.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

// browserLogin walks a browser through home and back, and returns its session cookie.
func (e *env) browserLogin(email, role string) *http.Cookie {
	e.t.Helper()
	if role == "owner" {
		e.userID(email)
	}
	res, _ := e.browse("/auth/start")
	to, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 303 || !strings.HasPrefix(to.String(), testHome+"/stow/login?") || len(res.Cookies()) != 1 {
		e.t.Fatalf("login start: %d %s, %d cookies", res.StatusCode, to, len(res.Cookies()))
	}
	// home signs a ticket bound to the challenge it was given, and sends the browser back
	claims := claimsFor(email, role, "")
	claims.Challenge = to.Query().Get("cc")
	back := "/api/oauth/mobile-redirect?" + url.Values{"code": {signTicket(e.key, claims)}, "state": {to.Query().Get("state")}}.Encode()
	res, _ = e.browse(back, res.Cookies()[0])
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		e.t.Fatalf("login finish: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, c := range res.Cookies() {
		if c.Name == "stow_session" && c.Value != "" {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				e.t.Errorf("session cookie: HttpOnly %v, SameSite %v", c.HttpOnly, c.SameSite)
			}
			return c
		}
	}
	e.t.Fatal("no session cookie")
	return nil
}

func TestPagesNeedALogin(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	e.userID("owner@example.com") // set up already: the login page, not the first account's
	for _, path := range []string{"/", "/places", "/people", "/people/x", "/status"} {
		if res, body := e.browse(path); res.StatusCode != 303 || res.Header.Get("Location") != "/login" || strings.Contains(body, "thumbnail") {
			t.Errorf("%s without a login: %d to %q", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	for _, path := range []string{"/web/month/2026-09", "/web/photo/x"} {
		if res, body := e.browse(path); res.StatusCode != 401 || len(body) > 5 {
			t.Errorf("%s without a login: %d, %d bytes", path, res.StatusCode, len(body))
		}
	}
	if res, body := e.browse("/login"); res.StatusCode != 200 || !strings.Contains(body, "/auth/start") {
		t.Errorf("login page: %d", res.StatusCode)
	}
	// a ticket that comes back without this browser having started a login goes to the phone app, as before
	back := "/api/oauth/mobile-redirect?" + url.Values{"code": {"x"}, "state": {"y"}}.Encode()
	if res, _ := e.browse(back, &http.Cookie{Name: "stow_login", Value: "other-state.verifier"}); res.StatusCode != 307 || !strings.HasPrefix(res.Header.Get("Location"), "app.immich:///") || len(res.Cookies()) != 0 {
		t.Errorf("stray return: %d to %q with %d cookies", res.StatusCode, res.Header.Get("Location"), len(res.Cookies()))
	}
}

func TestBrowserSeesItsOwnLibraryAndNoOneElses(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	phone := e.login("owner@example.com", "owner")
	_, up := e.upload(phone, "seoul.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)) // September 2026, Seoul
	old := mediatest.Camera
	old.Taken, old.LatRef = "2019:01:02 08:00:00", ""
	e.upload(phone, "old.jpg", mediatest.JPEG(old.Bytes(), 420, 300))
	e.upload(phone, "broken.jpg", mediatest.JPEG(nil, 400, 300)[:100])
	e.settle()
	id := up["id"].(string)

	// September 2026 holds the Seoul photo and the damaged file, which keeps the date the phone gave it
	res, page := e.browse("/", owner)
	if res.StatusCode != 200 || !strings.Contains(page, `data-month="2026-09" data-count="2"`) || !strings.Contains(page, `data-month="2019-01" data-count="1"`) {
		t.Fatalf("timeline: %d\n%s", res.StatusCode, page)
	}
	if strings.Index(page, "2026-09") > strings.Index(page, "2019-01") {
		t.Error("months are not newest first")
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("content security policy: %q", csp)
	}
	_, cells := e.browse("/web/month/2026-09", owner)
	if !strings.Contains(cells, "/api/assets/"+id+"/thumbnail") || strings.Contains(cells, "/original") {
		t.Errorf("month cells must point at small pictures only: %s", cells)
	}
	if !strings.Contains(cells, "2026년 9월 25일 (금)") || !strings.Contains(cells, `data-w="300" data-h="400"`) {
		t.Errorf("month cells must come by day and carry the shape of each photo: %s", cells)
	}
	if _, none := e.browse("/?show=favorites", owner); strings.Contains(none, "data-month") {
		t.Error("the favorites tab lists a month though nothing is a favorite")
	}
	if _, one := e.browse("/web/photo/"+id, owner); !strings.Contains(one, "2026년 9월 25일 (금) 15:13") || !strings.Contains(one, "중구, 서울특별시") || !strings.Contains(one, "SM-S938N") {
		t.Errorf("one photo: %s", one)
	}
	if _, places := e.browse("/places", owner); !strings.Contains(places, ">대한민국</a>") || !strings.Contains(places, ">서울특별시</a>") ||
		!strings.Contains(places, `href="/places?in=`+url.QueryEscape("대한민국|서울특별시|중구")+`"`) {
		t.Errorf("places must list the country, the state and the city: %s", places)
	}
	for _, in := range []string{"대한민국", "대한민국|서울특별시", "대한민국|서울특별시|중구"} {
		if _, there := e.browse("/places?in="+url.QueryEscape(in), owner); !strings.Contains(there, `data-month="2026-09"`) || !strings.Contains(there, "1장") {
			t.Errorf("photos of %s must come by month: %s", in, there)
		}
		if _, cells := e.browse("/web/month/2026-09?in="+url.QueryEscape(in), owner); !strings.Contains(cells, id) {
			t.Errorf("the month of %s must hold the photo: %s", in, cells)
		}
	}
	if _, cells := e.browse("/web/month/2026-09?in="+url.QueryEscape("대한민국|부산광역시"), owner); strings.Contains(cells, id) {
		t.Error("a photo from Seoul shows under Busan")
	}
	if _, status := e.browse("/status", owner); !strings.Contains(status, "broken.jpg") || !strings.Contains(status, "http://light.test") || !strings.Contains(status, "사진 정보 읽기") {
		t.Errorf("status: %s", status)
	}

	// the other member, logged in, sees none of it — not in lists, not by address
	if _, page := e.browse("/", kid); strings.Contains(page, "data-month") {
		t.Error("another member's timeline lists the owner's months")
	}
	if _, cells := e.browse("/web/month/2026-09", kid); strings.Contains(cells, id) {
		t.Error("another member's month lists the owner's photo")
	}
	if res, _ := e.browse("/web/photo/"+id, kid); res.StatusCode != 404 {
		t.Errorf("another member opening the photo: %d, want 404", res.StatusCode)
	}
	if res, _ := e.browse("/api/assets/"+id+"/thumbnail", kid); res.StatusCode != 404 {
		t.Errorf("another member fetching the picture: %d, want 404", res.StatusCode)
	}
	if _, status := e.browse("/status", kid); strings.Contains(status, "broken.jpg") {
		t.Error("another member's status names the owner's file")
	}

	// logging out ends it
	req, _ := http.NewRequest("POST", e.server.URL+"/logout", nil)
	req.AddCookie(owner)
	if res, err := noFollow.Do(req); err != nil || res.StatusCode != 303 {
		t.Fatalf("logout: %v", err)
	}
	if res, _ := e.browse("/", owner); res.StatusCode != 303 {
		t.Errorf("after logout: %d, want a redirect to the login page", res.StatusCode)
	}
}

func TestOverHTTPSTheCookiesAreLockedToThisHost(t *testing.T) {
	api := New(Config{PublicURL: "http://192.168.0.10:30891", HomeURL: testHome, HomeKey: newKey(t).Public().(ed25519.PublicKey),
		Link: Link{Slug: testSlug, Host: "demo.stow.tidoly.app", Key: "k"}}, nil, nil, nil)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/auth/start", nil)
	r.Host = "demo.stow.tidoly.app" // through the tunnel: plain http from frpc, https outside
	api.WebLoginStart(rec, r)
	c := rec.Result().Cookies()[0]
	if c.Name != "__Host-stow_login" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Errorf("login cookie over https: %+v", c)
	}
	if got := api.cookieName(r, "stow_session"); got != "__Host-stow_session" {
		t.Errorf("session cookie name over https: %s", got)
	}
}

func TestPagesLoadNothingFromElsewhere(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	for _, path := range []string{"/login", "/", "/places", "/people", "/status", "/licenses", "/static/app.css", "/static/app.js"} {
		res, body := e.browse(path, owner)
		if res.StatusCode != 200 {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
		for _, outside := range []string{"http://", "https://", `src="//`, `href="//`, "url(//", "@import"} {
			for from := 0; ; {
				at := strings.Index(body[from:], outside)
				if at < 0 {
					break
				}
				at += from
				from = at + len(outside)
				// a link one follows by clicking loads nothing; the source link (AGPL) is one
				if tag := body[strings.LastIndex(body[:at], "<"):at]; strings.HasPrefix(tag, "<a ") || strings.HasPrefix(body[at:], "http://light.test") {
					continue
				}
				t.Errorf("%s reaches outside: …%.60s…", path, body[at:])
			}
		}
	}
}

// Run with STOW_MEASURE=1 to print how the pages hold up with ten thousand photos (stow issue #11).
func TestMeasureTenThousandPhotos(t *testing.T) {
	if os.Getenv("STOW_MEASURE") == "" {
		t.Skip("set STOW_MEASURE=1 to run")
	}
	e := start(t, t.TempDir(), newKey(t))
	e.stopJobs()
	e.stopJobs = func() {}
	cookie := e.browserLogin("owner@example.com", "owner")
	user, _ := e.store.UpsertUser(context.Background(), "sub-owner@example.com", "owner@example.com", "owner", true)
	ctx := context.Background()
	begin := time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10000; i++ { // seven years, with one month holding a tenth of everything
		at := begin.Add(time.Duration(i) * 6 * time.Hour)
		if i%10 == 0 {
			at = time.Date(2024, 8, 1+i%28, 12, i%60, 0, 0, time.UTC)
		}
		a := store.Asset{ID: store.NewID(), OwnerID: user.ID, Checksum: []byte(fmt.Sprint(i)), OriginalName: fmt.Sprintf("IMG_%05d.jpg", i),
			Path: fmt.Sprint(i), Type: "IMAGE", FileCreatedAt: store.Timestamp(at), FileModifiedAt: store.Timestamp(at)}
		if err := e.store.AddAsset(ctx, &a, nil); err != nil {
			t.Fatal(err)
		}
	}
	timed := func(path string) (time.Duration, int) {
		best := time.Hour
		var size int
		for i := 0; i < 5; i++ {
			start := time.Now()
			res, body := e.browse(path, cookie)
			if res.StatusCode != 200 {
				t.Fatalf("%s: %d", path, res.StatusCode)
			}
			best, size = min(best, time.Since(start)), len(body)
		}
		return best, size
	}
	d, n := timed("/")
	_, page := e.browse("/", cookie)
	t.Logf("timeline shell: %v, %d bytes, %d months", d.Round(time.Millisecond/10), n, strings.Count(page, "data-month"))
	d, n = timed("/web/month/2024-08")
	_, cells := e.browse("/web/month/2024-08", cookie)
	t.Logf("the largest month: %v, %d bytes, %d photos", d.Round(time.Millisecond/10), n, strings.Count(cells, "<img"))
	d, n = timed("/web/month/2021-03")
	t.Logf("an ordinary month: %v, %d bytes", d.Round(time.Millisecond/10), n)
	d, _ = timed("/status")
	t.Logf("status: %v", d.Round(time.Millisecond/10))
}

// ── albums ──────────────────────────────────────────────────────────────────────────────────────────────────

func (e *env) call(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	res := e.do(method, path, token, reader, "application/json")
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (e *env) ackAll(token string, lines []map[string]any) {
	var acks []string
	for _, l := range lines {
		acks = append(acks, l["ack"].(string))
	}
	if status, _ := e.call("POST", "/api/sync/ack", token, map[string]any{"acks": acks}); status != 204 {
		e.t.Fatalf("ack: %d", status)
	}
}

func TestAlbumMadeOnOneDeviceShowsOnAnother(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	phone, tablet := e.login("owner@example.com", "owner"), e.login("owner@example.com", "owner")
	kid := e.login("kid@example.com", "member")
	_, a := e.upload(phone, "a.jpg", mediatest.JPEG(nil, 40, 30))
	_, b := e.upload(phone, "b.jpg", mediatest.JPEG(nil, 42, 30))
	_, kids := e.upload(kid, "kid.jpg", mediatest.JPEG(nil, 44, 30))
	e.settle()
	e.ackAll(tablet, e.sync(tablet))

	status, raw := e.call("POST", "/api/albums", phone, map[string]any{"albumName": "여름 휴가", "assetIds": []string{a["id"].(string)}})
	var album map[string]any
	json.Unmarshal(raw, &album)
	id, _ := album["id"].(string)
	if status != 201 || album["albumName"] != "여름 휴가" || album["assetCount"] != 1.0 || album["albumThumbnailAssetId"] != a["id"] {
		t.Fatalf("create: %d %s", status, raw)
	}
	// adding: one new photo, one already in, one that belongs to someone else
	status, raw = e.call("PUT", "/api/albums/"+id+"/assets", phone, map[string]any{"ids": []string{b["id"].(string), a["id"].(string), kids["id"].(string)}})
	var added []map[string]any
	json.Unmarshal(raw, &added)
	if status != 200 || len(added) != 3 || added[0]["success"] != true || added[1]["error"] != "duplicate" || added[2]["error"] != "not_found" {
		t.Fatalf("add: %d %s", status, raw)
	}

	// the tablet hears of the album, whose it is, and what is in it
	lines := e.sync(tablet)
	if count(lines, "AlbumV2") != 1 || count(lines, "AlbumUserV1") != 1 || count(lines, "AlbumToAssetV1") != 2 || count(lines, "AssetV2") != 0 {
		t.Fatalf("tablet after the album was made: %d albums, %d owners, %d photos in it, %d assets resent",
			count(lines, "AlbumV2"), count(lines, "AlbumUserV1"), count(lines, "AlbumToAssetV1"), count(lines, "AssetV2"))
	}
	if data := lineFor(lines, "AlbumV2", "id", id); data["name"] != "여름 휴가" {
		t.Errorf("album on the tablet: %v", data)
	}
	e.ackAll(tablet, lines)

	// rename and take one photo out, on the tablet; the phone hears of both
	e.ackAll(phone, e.sync(phone))
	if status, _ := e.call("PATCH", "/api/albums/"+id, tablet, map[string]any{"albumName": "제주"}); status != 200 {
		t.Fatalf("rename: %d", status)
	}
	if status, _ := e.call("DELETE", "/api/albums/"+id+"/assets", tablet, map[string]any{"ids": []string{a["id"].(string)}}); status != 200 {
		t.Fatalf("remove: %d", status)
	}
	lines = e.sync(phone)
	gone := lineFor(lines, "AlbumToAssetDeleteV1", "albumId", id)
	if data := lineFor(lines, "AlbumV2", "id", id); data["name"] != "제주" || data["thumbnailAssetId"] != b["id"] || gone["assetId"] != a["id"] {
		t.Fatalf("phone after the tablet's changes: album %v, removed %v", data, gone)
	}
	e.ackAll(phone, lines)

	// the web page lists it
	browser := e.browserLogin("owner@example.com", "owner")
	if _, page := e.browse("/albums", browser); !strings.Contains(page, "제주") || !strings.Contains(page, "1장") {
		t.Errorf("albums page: %s", page)
	}
	_, page := e.browse("/albums/"+id, browser)
	at := strings.Index(page, `data-month="`)
	if at < 0 || !strings.Contains(page, `data-query="?album=`+id+`"`) || !strings.Contains(page, "1장") {
		t.Fatalf("album page must list its photos by month: %s", page)
	}
	month := page[at+len(`data-month="`):][:7]
	if _, cells := e.browse("/web/month/"+month+"?album="+id, browser); !strings.Contains(cells, b["id"].(string)) || strings.Contains(cells, a["id"].(string)) {
		t.Errorf("the month of the album must hold its photo only: %s", cells)
	}

	// another member: not listed, not readable, not changeable
	if _, raw := e.call("GET", "/api/albums", kid, nil); string(bytes.TrimSpace(raw)) != "[]" {
		t.Errorf("another member's album list: %s", raw)
	}
	for _, c := range []struct{ method, path string }{{"GET", "/api/albums/" + id}, {"PATCH", "/api/albums/" + id}, {"DELETE", "/api/albums/" + id},
		{"PUT", "/api/albums/" + id + "/assets"}, {"DELETE", "/api/albums/" + id + "/assets"}} {
		if status, _ := e.call(c.method, c.path, kid, map[string]any{"ids": []string{kids["id"].(string)}, "albumName": "x"}); status != 404 {
			t.Errorf("%s %s as another member: %d, want 404", c.method, c.path, status)
		}
	}
	if lines := e.sync(kid); count(lines, "AlbumV2") != 0 || count(lines, "AlbumToAssetV1") != 0 {
		t.Error("another member's sync carries the album")
	}
	if res, _ := e.browse("/albums/"+id, e.browserLogin("kid@example.com", "member")); res.StatusCode != 404 {
		t.Errorf("another member opening the album page: %d", res.StatusCode)
	}

	// deleting the album leaves the photos
	if status, _ := e.call("DELETE", "/api/albums/"+id, phone, nil); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	lines = e.sync(phone)
	if lineFor(lines, "AlbumDeleteV1", "albumId", id) == nil || count(lines, "AlbumV2") != 0 {
		t.Fatalf("after deleting: %v", lines)
	}
	if res := e.do("GET", "/api/assets/"+b["id"].(string)+"/original", phone, nil, ""); res.StatusCode != 200 {
		t.Errorf("a photo of the deleted album: %d, want it still there", res.StatusCode)
	}
	// a device logging in afresh sees no trace of the album's contents
	if lines := e.sync(e.login("owner@example.com", "owner")); count(lines, "AlbumV2") != 0 || count(lines, "AlbumToAssetV1") != 0 {
		t.Error("a fresh device still gets the deleted album")
	}
}

// ── deleting ────────────────────────────────────────────────────────────────────────────────────────────────

// Grouping faces is on at first. Turned off, a photo that comes in is not looked at and the app is told
// there is no face recognition; turned on again, that photo is looked at too.
func TestFaceGroupingCanBeTurnedOff(t *testing.T) {
	withFaces = true
	defer func() { withFaces = false }()
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	token := e.login("owner@example.com", "owner")
	if got := e.api.FacesState(ctx); got != "on" {
		t.Fatalf("face grouping at first: %s", got)
	}
	if err := e.api.SetFaces(ctx, false); err != nil {
		t.Fatal(err)
	}
	_, up := e.upload(token, "alice at the beach.jpg", mediatest.JPEG(nil, 640, 480))
	id := up["id"].(string)
	e.settle()
	if done, _ := e.store.HasFaces(ctx, id); done {
		t.Error("a photo was looked at for faces while that was off")
	}
	res := e.do("GET", "/api/server/features", token, nil, "")
	var features map[string]any
	json.NewDecoder(res.Body).Decode(&features)
	res.Body.Close()
	if features["facialRecognition"] != false || e.api.FacesState(ctx) != "off" {
		t.Errorf("off: features %v, state %s", features["facialRecognition"], e.api.FacesState(ctx))
	}
	if err := e.api.SetFaces(ctx, true); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if done, _ := e.store.HasFaces(ctx, id); !done {
		t.Error("turned on again, the photo that came in meanwhile was not looked at")
	}
}

func TestADeletedPhotoIsGoneEverywhere(t *testing.T) {
	withFaces = true
	defer func() { withFaces = false }()
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	phone, tablet := e.login("owner@example.com", "owner"), e.login("owner@example.com", "owner")
	kid := e.login("kid@example.com", "member")
	up := func(token, name string, n int) string {
		_, body := e.upload(token, name, mediatest.JPEG(mediatest.Camera.Bytes(), 640+n, 480))
		return body["id"].(string)
	}
	a1, a2, a3 := up(phone, "alice one.jpg", 0), up(phone, "alice two.jpg", 2), up(phone, "alice three.jpg", 4)
	kids := up(kid, "kid.jpg", 6)
	e.settle()
	_, raw := e.call("POST", "/api/albums", phone, map[string]any{"albumName": "모음", "assetIds": []string{a1, a2}})
	var album map[string]any
	json.Unmarshal(raw, &album)
	e.call("PATCH", "/api/albums/"+album["id"].(string), phone, map[string]any{"albumThumbnailAssetId": a1})
	first := e.sync(tablet)
	var person string
	for _, l := range first {
		if l["type"] == "PersonV1" {
			person = l["data"].(map[string]any)["id"].(string)
		}
	}
	if person == "" || count(first, "AssetFaceV3") != 3 {
		t.Fatalf("before deleting: person %q, %d faces", person, count(first, "AssetFaceV3"))
	}
	e.ackAll(tablet, first)
	onDisk := len(filesUnder(t, filepath.Join(dir, "originals")))

	// the kid tries to delete the owner's photo: nothing happens
	if status, _ := e.call("DELETE", "/api/assets", kid, map[string]any{"ids": []string{a1}, "force": true}); status != 204 {
		t.Fatalf("delete as another member: %d", status)
	}
	if res := e.do("GET", "/api/assets/"+a1+"/original", phone, nil, ""); res.StatusCode != 200 {
		t.Fatal("another member deleted the owner's photo")
	}

	// the owner deletes it on the phone
	if status, _ := e.call("DELETE", "/api/assets", phone, map[string]any{"ids": []string{a1}, "force": true}); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	for _, path := range []string{"/original", "/thumbnail", "/thumbnail?size=preview"} {
		if res := e.do("GET", "/api/assets/"+a1+path, phone, nil, ""); res.StatusCode != 404 {
			t.Errorf("%s after deleting: %d, want 404", path, res.StatusCode)
		}
	}
	if left := len(filesUnder(t, filepath.Join(dir, "originals"))); left != onDisk-1 {
		t.Errorf("originals on disk: %d, want %d", left, onDisk-1)
	}
	for _, f := range filesUnder(t, filepath.Join(dir, ".stow", "derived")) {
		if strings.Contains(f, a1) {
			t.Errorf("a small picture of the deleted photo is still on disk: %s", f)
		}
	}
	// the tablet is told: the photo, its face, and the album it was in — whose cover falls back to what is left
	lines := e.sync(tablet)
	if lineFor(lines, "AssetDeleteV1", "assetId", a1) == nil || count(lines, "AssetFaceDeleteV1") != 1 || count(lines, "PersonDeleteV1") != 0 {
		t.Fatalf("tablet after the delete: %d assets gone, %d faces gone, %d people gone",
			count(lines, "AssetDeleteV1"), count(lines, "AssetFaceDeleteV1"), count(lines, "PersonDeleteV1"))
	}
	if data := lineFor(lines, "AlbumV2", "id", album["id"].(string)); data == nil || data["thumbnailAssetId"] != a2 {
		t.Errorf("the album after its cover photo was deleted: %v", data)
	}
	if res := e.do("GET", "/api/people/"+person+"/thumbnail", phone, nil, ""); res.StatusCode != 200 {
		t.Errorf("the person, who still has two photos: picture %d", res.StatusCode)
	}
	e.ackAll(tablet, lines)

	// deleting the rest of her photos takes the person along
	e.call("DELETE", "/api/assets", phone, map[string]any{"ids": []string{a2, a3}, "force": true})
	lines = e.sync(tablet)
	if count(lines, "AssetDeleteV1") != 2 || lineFor(lines, "PersonDeleteV1", "personId", person) == nil {
		t.Fatalf("after deleting all her photos: %d assets gone, person gone: %v", count(lines, "AssetDeleteV1"), lineFor(lines, "PersonDeleteV1", "personId", person) != nil)
	}
	if res := e.do("GET", "/api/people/"+person+"/thumbnail", phone, nil, ""); res.StatusCode != 404 {
		t.Errorf("the person's picture after she is gone: %d", res.StatusCode)
	}
	// a fresh device learns of none of it, and the kid's library is as it was
	if fresh := e.sync(e.login("owner@example.com", "owner")); count(fresh, "AssetV2") != 0 || count(fresh, "AssetFaceV3") != 0 || count(fresh, "PersonV1") != 0 || count(fresh, "AlbumToAssetV1") != 0 {
		t.Error("a fresh device still gets something of the deleted photos")
	}
	if res := e.do("GET", "/api/assets/"+kids+"/original", kid, nil, ""); res.StatusCode != 200 {
		t.Error("the other member's photo was touched")
	}
	// the same photo can be uploaded again and is a new photo
	if status, body := e.upload(phone, "alice one.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 640, 480)); status != 201 || body["id"] == a1 {
		t.Errorf("uploading it again: %d %v", status, body)
	}
	// and from the web page
	e.settle()
	browser := e.browserLogin("owner@example.com", "owner")
	again := e.sync(e.login("owner@example.com", "owner"))
	var newID string
	for _, l := range again {
		if l["type"] == "AssetV2" {
			newID = l["data"].(map[string]any)["id"].(string)
		}
	}
	req, _ := http.NewRequest("POST", e.server.URL+"/web/photos/delete", strings.NewReader(url.Values{"id": {newID}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(browser)
	if res, err := noFollow.Do(req); err != nil || res.StatusCode != 204 {
		t.Fatalf("delete from the web page: %v", err)
	}
	if _, page := e.browse("/", browser); strings.Contains(page, "data-month") {
		t.Error("the timeline still lists a month after its only photo went to the trash")
	}
	if _, page := e.browse("/trash", browser); !strings.Contains(page, newID) {
		t.Error("the trash page does not show it")
	}
}

// What the app calls "delete" must be something a person can take back.
func TestDeleteGoesToTheTrashAndComesBack(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	phone, tablet := e.login("owner@example.com", "owner"), e.login("owner@example.com", "owner")
	kid := e.login("kid@example.com", "member")
	data := mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)
	_, up := e.upload(phone, "keep.jpg", data)
	_, other := e.upload(phone, "other.jpg", mediatest.JPEG(nil, 404, 300))
	id := up["id"].(string)
	e.settle()
	e.ackAll(tablet, e.sync(tablet))
	var features, config map[string]any
	_, raw := e.call("GET", "/api/server/features", "", nil)
	json.Unmarshal(raw, &features)
	_, raw = e.call("GET", "/api/server/config", "", nil)
	json.Unmarshal(raw, &config)
	if features["trash"] != true || config["trashDays"] != 30.0 {
		t.Fatalf("the app is not told there is a trash: %v, %v days", features["trash"], config["trashDays"])
	}

	// "delete" in the app, as it asks when the server has a trash
	if status, _ := e.call("DELETE", "/api/assets", phone, map[string]any{"ids": []string{id}}); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	lines := e.sync(tablet)
	if data := lineFor(lines, "AssetV2", "id", id); data == nil || data["deletedAt"] == nil || count(lines, "AssetDeleteV1") != 0 {
		t.Fatalf("the tablet should hear that it is in the trash, not that it is gone: %v", data)
	}
	e.ackAll(tablet, lines)
	if files := filesUnder(t, filepath.Join(dir, "originals")); len(files) != 2 {
		t.Fatalf("files on disk after trashing: %v — nothing may be removed yet", files)
	}
	browser := e.browserLogin("owner@example.com", "owner")
	if _, page := e.browse("/", browser); !strings.Contains(page, `data-count="1"`) {
		t.Errorf("the web timeline should be down to one photo: %s", page)
	}
	if _, page := e.browse("/places", browser); strings.Contains(page, "중구") {
		t.Error("a trashed photo still makes a place")
	}
	if _, page := e.browse("/trash", browser); !strings.Contains(page, id) || !strings.Contains(page, "30일") || !strings.Contains(page, "29일 뒤 완전히 지워집니다") {
		t.Errorf("trash page: %s", page)
	}

	// another member can neither restore nor empty it
	e.call("POST", "/api/trash/restore/assets", kid, map[string]any{"ids": []string{id}})
	e.call("POST", "/api/trash/empty", kid, nil)
	if got, _ := e.store.Trashed(context.Background(), e.userID("owner@example.com")); len(got) != 1 {
		t.Fatalf("after another member's attempts the owner's trash holds %d, want 1", len(got))
	}

	// restore: it is back everywhere
	status, raw := e.call("POST", "/api/trash/restore/assets", phone, map[string]any{"ids": []string{id}})
	if status != 200 || !strings.Contains(string(raw), `"count":1`) {
		t.Fatalf("restore: %d %s", status, raw)
	}
	if data := lineFor(e.sync(tablet), "AssetV2", "id", id); data == nil || data["deletedAt"] != nil {
		t.Fatalf("the tablet after the restore: %v", data)
	}
	if _, page := e.browse("/", browser); !strings.Contains(page, `data-count="2"`) {
		t.Error("the web timeline did not get it back")
	}

	// trashed again, then uploaded again from the phone: that takes it out of the trash
	e.call("DELETE", "/api/assets", phone, map[string]any{"ids": []string{id}})
	if status, body := e.upload(phone, "keep.jpg", data); status != 200 || body["id"] != id {
		t.Fatalf("uploading a trashed photo again: %d %v", status, body)
	}
	if got, _ := e.store.Trashed(context.Background(), e.userID("owner@example.com")); len(got) != 0 {
		t.Error("uploading it again did not take it out of the trash")
	}

	// emptying the trash is what removes the file
	e.call("DELETE", "/api/assets", phone, map[string]any{"ids": []string{id, other["id"].(string)}})
	status, raw = e.call("POST", "/api/trash/empty", phone, nil)
	if status != 200 || !strings.Contains(string(raw), `"count":2`) {
		t.Fatalf("empty: %d %s", status, raw)
	}
	if files := filesUnder(t, filepath.Join(dir, "originals")); len(files) != 0 {
		t.Errorf("files on disk after emptying the trash: %v", files)
	}
	if lines := e.sync(tablet); count(lines, "AssetDeleteV1") != 2 {
		t.Errorf("the tablet after the trash was emptied: %d photos gone, want 2", count(lines, "AssetDeleteV1"))
	}
}

func TestFavouriteMarkTravelsBetweenDevices(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	phone, tablet := e.login("owner@example.com", "owner"), e.login("owner@example.com", "owner")
	kid := e.login("kid@example.com", "member")
	_, up := e.upload(phone, "a.jpg", mediatest.JPEG(nil, 40, 30))
	id := up["id"].(string)
	e.settle()
	e.ackAll(tablet, e.sync(tablet))
	if status, _ := e.call("PUT", "/api/assets", kid, map[string]any{"ids": []string{id}, "isFavorite": true}); status != 204 {
		t.Fatalf("another member: %d", status)
	}
	if lines := e.sync(tablet); count(lines, "AssetV2") != 0 {
		t.Fatal("another member marked the owner's photo")
	}
	if status, _ := e.call("PUT", "/api/assets", phone, map[string]any{"ids": []string{id}, "isFavorite": true}); status != 204 {
		t.Fatalf("favourite: %d", status)
	}
	lines := e.sync(tablet)
	if data := lineFor(lines, "AssetV2", "id", id); data == nil || data["isFavorite"] != true {
		t.Fatalf("the tablet after the phone marked it: %v", data)
	}
	e.ackAll(tablet, lines)
	e.call("PUT", "/api/assets", phone, map[string]any{"ids": []string{id}, "isFavorite": false})
	if data := lineFor(e.sync(tablet), "AssetV2", "id", id); data == nil || data["isFavorite"] != false {
		t.Fatalf("the tablet after the phone unmarked it: %v", data)
	}
	// a change this server does not keep is refused rather than swallowed
	if status, _ := e.call("PUT", "/api/assets", phone, map[string]any{"ids": []string{id}, "visibility": "archive"}); status != 400 {
		t.Errorf("archiving: %d, want 400", status)
	}
}

// Casting: the app borrows a short session for the television, which can then fetch pictures by address alone.
func TestALentSessionReadsPicturesAndEnds(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	phone := e.login("owner@example.com", "owner")
	kid := e.login("kid@example.com", "member")
	_, up := e.upload(phone, "a.jpg", mediatest.JPEG(nil, 40, 30))
	_, kids := e.upload(kid, "kid.jpg", mediatest.JPEG(nil, 44, 30))
	e.settle()
	status, raw := e.call("POST", "/api/sessions", phone, map[string]any{"duration": 60, "deviceType": "Google Cast", "deviceOS": "Google Cast"})
	var lent map[string]any
	json.Unmarshal(raw, &lent)
	key, _ := lent["token"].(string)
	if status != 201 || key == "" || key == phone || lent["expiresAt"] == nil {
		t.Fatalf("lend: %d %s", status, raw)
	}
	// before casting the app asks what kind of file it is, and casts nothing without the answer
	status, raw = e.call("GET", "/api/assets/"+up["id"].(string), phone, nil)
	var info map[string]any
	json.Unmarshal(raw, &info)
	if status != 200 || info["originalMimeType"] != "image/jpeg" || info["id"] != up["id"] {
		t.Errorf("asset info: %d %s", status, raw)
	}
	for _, field := range []string{"checksum", "createdAt", "duration", "fileCreatedAt", "fileModifiedAt", "hasMetadata", "height", "id", "isArchived",
		"isEdited", "isFavorite", "isOffline", "isTrashed", "localDateTime", "originalFileName", "originalPath", "ownerId", "thumbhash", "type",
		"updatedAt", "visibility", "width"} { // what the app's AssetResponseDto must have
		if _, ok := info[field]; !ok {
			t.Errorf("asset info lacks %s", field)
		}
	}
	if status, _ := e.call("GET", "/api/assets/"+kids["id"].(string), phone, nil); status != 404 {
		t.Errorf("asset info of another member's photo: %d, want 404", status)
	}
	fetch := func(path string) int {
		res, err := http.Get(e.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	id := up["id"].(string)
	if got := fetch("/api/assets/" + id + "/thumbnail?size=preview&sessionKey=" + key); got != 200 {
		t.Errorf("the television fetching the owner's picture: %d", got)
	}
	if got := fetch("/api/assets/" + kids["id"].(string) + "/thumbnail?sessionKey=" + key); got != 404 {
		t.Errorf("the television fetching another member's picture: %d, want 404", got)
	}
	if got := fetch("/api/assets/" + id + "/thumbnail?sessionKey=wrong"); got != 401 {
		t.Errorf("a made-up key: %d, want 401", got)
	}
	// the key is for reading: it does not work on anything that changes the library
	req, _ := http.NewRequest("DELETE", e.server.URL+"/api/assets?sessionKey="+key, strings.NewReader(`{"ids":["`+id+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Errorf("deleting with the key in the address: %d, want 401", res.StatusCode)
	}
	// and it ends
	if err := e.store.CreateTimedSession(context.Background(), "spent", e.userID("owner@example.com"), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := fetch("/api/assets/" + id + "/thumbnail?sessionKey=spent"); got != 401 {
		t.Errorf("a session past its time: %d, want 401", got)
	}
}

// The web pages upload through the same door as the phone, with the browser's own session.
func TestBrowserCanUpload(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	browser := e.browserLogin("owner@example.com", "owner")
	if _, page := e.browse("/", browser); !strings.Contains(page, `type="file"`) {
		t.Fatal("no upload control on the page")
	}
	body, ctype := form("from-the-mac.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300), "2026-10-13T03:00:00.000Z")
	req, _ := http.NewRequest("POST", e.server.URL+"/api/assets", body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(browser)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("upload with the browser's session: %v %v", err, res.StatusCode)
	}
	res.Body.Close()
	e.settle()
	if _, page := e.browse("/", browser); !strings.Contains(page, `data-month="2026-09" data-count="1"`) { // filed by when it was taken, not when the file was touched
		t.Errorf("timeline after the upload: %s", page)
	}
	// without a session the same request is refused
	body, ctype = form("x.jpg", []byte("x"), "2026-10-13T03:00:00.000Z")
	if res := e.do("POST", "/api/assets", "", body, ctype); res.StatusCode != 401 {
		t.Errorf("upload without a session: %d", res.StatusCode)
	}
}

// A shared photo is seen by everyone on the server and still belongs to the one who uploaded it (#13).
func TestASharedPhotoIsSeenByAllAndOwnedByOne(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	_, up := e.upload(e.login("owner@example.com", "owner"), "seoul.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300))
	e.settle()
	id := up["id"].(string)
	act := func(who *http.Cookie, path string, form url.Values) int {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(who)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	sees := func(who *http.Cookie, path string) bool {
		_, page := e.browse(path, who)
		return strings.Contains(page, "data-month")
	}
	small := "/api/assets/" + id + "/thumbnail"

	if res, _ := e.browse(small, kid); sees(kid, "/") || res.StatusCode != 404 {
		t.Fatal("a photo that is not shared is seen by someone else")
	}
	if act(kid, "/web/photos/share", url.Values{"id": {id}, "on": {"1"}}); sees(kid, "/") {
		t.Fatal("someone else shared the owner's photo")
	}
	if code := act(owner, "/web/photos/share", url.Values{"id": {id}, "on": {"1"}}); code != 204 {
		t.Fatalf("share: %d", code)
	}
	if !sees(kid, "/") || !sees(kid, "/?show=shared") || sees(kid, "/?show=mine") || sees(kid, "/?show=favorites") {
		t.Error("the other member must see a shared photo under all and shared, and not under mine or favorites")
	}
	if !sees(owner, "/?show=mine") || !sees(owner, "/?show=shared") {
		t.Error("the owner must see their shared photo under mine and shared")
	}
	_, cells := e.browse("/web/month/2026-09?show=shared", kid)
	if !strings.Contains(cells, id) || !strings.Contains(cells, "theirs") || !strings.Contains(cells, "owner@example.com 님이 공유") {
		t.Errorf("a shared photo must say whose it is: %s", cells)
	}
	if _, mine := e.browse("/web/month/2026-09", owner); strings.Contains(mine, "theirs") || !strings.Contains(mine, "shared ") || !strings.Contains(mine, `data-quick="share"`) {
		t.Errorf("the owner's own shared photo must be marked as shared, not as someone else's: %s", mine)
	}
	if res, _ := e.browse(small, kid); res.StatusCode != 200 {
		t.Errorf("small picture of a shared photo: %d", res.StatusCode)
	}
	if res, _ := e.browse("/api/assets/"+id+"/original", kid); res.StatusCode != 200 {
		t.Errorf("original of a shared photo: %d", res.StatusCode)
	}
	if _, one := e.browse("/web/photo/"+id, kid); !strings.Contains(one, "올린 사람") || strings.Contains(one, `data-act="delete"`) || strings.Contains(one, `data-act="share"`) {
		t.Errorf("someone else's photo must name its owner and offer neither deleting nor sharing: %s", one)
	}
	// only the owner deletes, marks, or stops sharing
	act(kid, "/web/photos/delete", url.Values{"id": {id}})
	act(kid, "/web/photos/favorite", url.Values{"id": {id}, "on": {"1"}})
	act(kid, "/web/photos/share", url.Values{"id": {id}})
	if !sees(owner, "/") || !sees(kid, "/") || sees(owner, "/?show=favorites") {
		t.Error("someone else deleted, marked or stopped sharing the owner's photo")
	}
	// in the trash it is gone for the others, and back when restored
	act(owner, "/web/photos/delete", url.Values{"id": {id}})
	if res, _ := e.browse(small, kid); sees(kid, "/") || res.StatusCode != 404 {
		t.Error("a shared photo in its owner's trash is still seen by someone else")
	}
	act(owner, "/web/photos/restore", url.Values{"id": {id}})
	if act(owner, "/web/photos/share", url.Values{"id": {id}}); sees(kid, "/") {
		t.Error("a photo is still seen by someone else after its owner stopped sharing it")
	}
	if res, _ := e.browse(small, kid); res.StatusCode != 404 {
		t.Errorf("small picture after sharing stopped: %d", res.StatusCode)
	}
}

// The owner of the server adds a folder on another disk and sends new photos there; what was uploaded
// before stays where it is and is still shown (#14).
// A place on a disk photos are kept on already gives no room: it is refused, and the page says why.
// Registering a folder too high would bring in what browsers cache and programs carry: the system disk's
// top, the home folder and above, system and program folders are refused; inside a good folder, program
// and cache folders are passed over.
func TestSystemFoldersAreNotRegistered(t *testing.T) {
	home, _ := os.UserHomeDir()
	refused := []string{"/", home, filepath.Dir(home)}
	if runtime.GOOS == "darwin" {
		refused = append(refused, filepath.Join(home, "Library", "Caches"), "/System/Library", "/Applications")
	}
	for _, dir := range refused {
		if !systemFolder(dir) {
			t.Errorf("%s may be registered", dir)
		}
	}
	for _, dir := range []string{filepath.Join(home, "Pictures"), filepath.Join(home, "Pictures", "trip"), "/Volumes/EOS_DIGITAL", "/media/me/card"} {
		if systemFolder(dir) {
			t.Errorf("%s may not be registered", dir)
		}
	}
	for name, want := range map[string]bool{"Caches": true, "GPUCache": true, "Code Cache": true, "Library": true, "AppData": true, "node_modules": true, ".thumbnails": true, "$RECYCLE.BIN": true, "DCIM": false, "2024 여행": false, "Camera": false} {
		if passOver(name) != want {
			t.Errorf("passOver(%q) = %v", name, !want)
		}
	}
	e := start(t, t.TempDir(), newKey(t))
	if _, err := e.api.AddFolder(context.Background(), home, ""); !errors.As(err, new(ErrSystemFolder)) {
		t.Errorf("registering the home folder: %v", err)
	}
	owner := e.browserLogin("owner@example.com", "owner")
	if _, list := e.browse("/admin/folders?for=import&at="+url.QueryEscape(home), owner); !strings.Contains(list, "등록할 수 없습니다") || !strings.Contains(list, "disabled>이 폴더로") {
		t.Errorf("the register picker at the home folder: %s", list)
	}
}

func TestAPlaceOnTheSameDiskIsRefused(t *testing.T) {
	sameDisk = disk.Same
	defer func() { sameDisk = func(a, b string) (bool, error) { return false, nil } }()
	e := start(t, t.TempDir(), newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	if _, err := e.api.AddLocation(context.Background(), filepath.Join(t.TempDir(), "more")); !errors.As(err, new(ErrSameDisk)) {
		t.Fatalf("a place on the data folder's disk: %v", err)
	}
	req, _ := http.NewRequest("POST", e.server.URL+"/admin/locations", strings.NewReader(url.Values{"path": {filepath.Join(t.TempDir(), "more")}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(owner)
	res, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 400 || !strings.Contains(string(page), "같은 디스크입니다") {
		t.Errorf("the admin page on a place on the same disk: %d", res.StatusCode)
	}
}

// Choosing a place, the picker offers only the disks photos are not put on yet — or says there is none —
// and will not take a folder on a used one; choosing a folder of one's own photos to read, it does not mind.
func TestThePickerShowsTheSameDisk(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	os.MkdirAll(filepath.Join(dir, "inside"), 0o755)
	_, list := e.browse("/admin/folders?at="+url.QueryEscape(dir), owner)
	if !strings.Contains(list, "사진을 이미 두는 디스크") || !strings.Contains(list, "disabled>이 폴더로") {
		t.Errorf("the place picker on the data folder's disk: %s", list)
	}
	// at the start only the other disks are offered; this machine's own disk is not one
	_, list = e.browse("/admin/folders?at=", owner)
	if strings.Contains(list, `data-at="/"`) || (!strings.Contains(list, "다른 디스크가 없습니다") && !strings.Contains(list, "/Volumes/") && !strings.Contains(list, "/media/") && !strings.Contains(list, "/mnt/")) {
		t.Errorf("the place picker's start: %s", list)
	}
	_, list = e.browse("/admin/folders?for=import&at="+url.QueryEscape(dir), owner)
	if strings.Contains(list, "이미 두는 디스크") || strings.Contains(list, "disabled>이 폴더로") {
		t.Errorf("the folder picker for reading photos minds the disk: %s", list)
	}
}

func TestNewPhotosGoToAnAddedLocationAndOldOnesStay(t *testing.T) {
	data, other := t.TempDir(), filepath.Join(t.TempDir(), "photo")
	e := start(t, data, newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	phone := e.login("owner@example.com", "owner")
	post := func(who *http.Cookie, path string, form url.Values) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(who)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	files := func(dir string) (n int) {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jpg") {
				n++
			}
			return nil
		})
		return
	}
	home := filepath.Join(data, "originals")
	_, first := e.upload(phone, "first.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300))

	if res, _ := e.browse("/admin", kid); res.StatusCode != 404 {
		t.Errorf("the admin page for someone who is not the owner: %d", res.StatusCode)
	}
	if res := post(kid, "/admin/locations", url.Values{"path": {other}}); res.StatusCode != 404 {
		t.Errorf("someone who is not the owner added a location: %d", res.StatusCode)
	}
	os.MkdirAll(filepath.Join(data, "seen-by-the-picker"), 0o755)
	if _, list := e.browse("/admin/folders?at="+url.QueryEscape(data), owner); !strings.Contains(list, "seen-by-the-picker") || strings.Contains(list, "stow.db") {
		t.Errorf("the folder picker must list folders and no files: %s", list)
	}
	if res, _ := e.browse("/admin/folders?at="+url.QueryEscape(data), kid); res.StatusCode != 404 {
		t.Errorf("the folder picker for someone who is not the owner: %d", res.StatusCode)
	}
	if res := post(owner, "/admin/locations", url.Values{"path": {"relative/folder"}}); res.StatusCode != 400 {
		t.Errorf("a path that is not a full path was taken: %d", res.StatusCode)
	}
	if res := post(owner, "/admin/locations", url.Values{"path": {filepath.Join(data, ".stow", "stow.db", "inside-a-file")}}); res.StatusCode != 400 {
		t.Errorf("a folder that cannot be made was taken: %d", res.StatusCode)
	}
	if res := post(owner, "/admin/locations", url.Values{"path": {filepath.Dir(other)}, "name": {"../outside"}}); res.StatusCode != 400 {
		t.Errorf("a new folder name that leads out of the chosen folder was taken: %d", res.StatusCode)
	}
	added := post(owner, "/admin/locations", url.Values{"path": {filepath.Dir(other)}, "name": {filepath.Base(other)}})
	if added.StatusCode != 303 || !strings.Contains(added.Header.Get("Location"), "moved=") {
		t.Fatalf("add a location: %d to %q", added.StatusCode, added.Header.Get("Location"))
	}
	if _, page := e.browse(added.Header.Get("Location"), owner); !strings.Contains(page, "이제 새 사진은") {
		t.Error("adding a location does not say that new photos go there now")
	}
	_, page := e.browse("/admin", owner)
	id := regexp.MustCompile(`name="location" value="([0-9a-f-]{36})"`).FindStringSubmatch(page)
	if id == nil || !strings.Contains(page, other) {
		t.Fatalf("the new location is not on the admin page: %s", page)
	}
	// added because the place in use fills up: new photos go to the added one at once
	if _, second := e.upload(phone, "second.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 410, 300)); second["status"] != "created" || files(other) != 1 {
		t.Fatalf("a photo did not go to the location just added: %v, %d files", second, files(other))
	}
	post(owner, "/admin/location", url.Values{"location": {id[1]}}) // choosing it again changes nothing
	_, third := e.upload(phone, "third.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 420, 300))
	e.settle()
	if files(other) != 2 || files(home) != 1 {
		t.Errorf("after the change: %d files in the new location (want 2), %d in the data folder (want 1)", files(other), files(home))
	}
	for name, up := range map[string]map[string]any{"the old photo": first, "the new photo": third} {
		if res, _ := e.browse("/api/assets/"+up["id"].(string)+"/original", owner); res.StatusCode != 200 {
			t.Errorf("%s after the change: %d", name, res.StatusCode)
		}
		if res, _ := e.browse("/api/assets/"+up["id"].(string)+"/thumbnail", owner); res.StatusCode != 200 {
			t.Errorf("small picture of %s: %d", name, res.StatusCode)
		}
	}
	// one person's photos can go somewhere else than everyone's
	_, page = e.browse("/admin", owner)
	account := regexp.MustCompile(`(?s)kid@example\.com</span><span class="dim">[^<]*</span></span>\s*<span class="acts"><form method="post" action="/admin/location" data-auto><input type="hidden" name="user" value="([0-9a-f-]{36})"`).FindStringSubmatch(page)
	if account == nil || !strings.Contains(page, "owner@example.com</span>") {
		t.Fatalf("the admin page must list both accounts: %s", page)
	}
	post(owner, "/admin/location", url.Values{"user": {account[1]}, "location": {"home"}})
	e.upload(e.login("kid@example.com", "member"), "kid.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 430, 300))
	if files(other) != 2 || files(home) != 2 {
		t.Errorf("a person sent to the data folder: %d files in the added location (want 2), %d in the data folder (want 2)", files(other), files(home))
	}
}

// A folder of photos someone already had is read where it lies: its photos show in the library, and
// nothing in the folder is made, changed or deleted (#15).
func TestAFolderIsReadWhereItLies(t *testing.T) {
	data, folder := t.TempDir(), t.TempDir()
	e := start(t, data, newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	ctx := context.Background()
	post := func(who *http.Cookie, path string, form url.Values) int {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(who)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	put := func(rel string, body []byte) {
		t.Helper()
		full := filepath.Join(folder, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listing := func() string { // every file and folder with its size and time: what must not change
		var out []string
		filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
			info, _ := d.Info()
			out = append(out, fmt.Sprintf("%s %d %s", p, info.Size(), info.ModTime()))
			return nil
		})
		return strings.Join(out, "\n")
	}
	looked := func() { // waits for the look at the folder to end, then for the jobs it queued
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, page := e.browse("/admin", owner); !strings.Contains(page, "data-running") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the look at the folder did not end")
			}
		}
		e.settle()
	}
	recorded := func() []string {
		t.Helper()
		found, _ := e.store.Locations(ctx)
		var out []string
		for _, l := range found {
			if l.Kind == store.KindFolder {
				files, _ := e.store.FilesAt(ctx, l.ID)
				for rel := range files {
					out = append(out, rel)
				}
			}
		}
		sort.Strings(out)
		return out
	}

	same := mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)
	e.upload(e.login("owner@example.com", "owner"), "from-the-phone.jpg", same)
	put("a.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 410, 300))
	put("trip/b.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 420, 300))
	put("copy-of-the-phone-photo.jpg", same)
	put("notes.txt", []byte("not a photo"))
	put(".thumbnails/x.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 430, 300))
	before := listing()

	if code := post(kid, "/admin/import", url.Values{"path": {folder}}); code != 404 {
		t.Errorf("someone who is not the owner registered a folder: %d", code)
	}
	if code := post(owner, "/admin/import", url.Values{"path": {filepath.Join(data, "originals")}}); code != 400 {
		t.Errorf("the library's own folder was registered as a folder to read: %d", code)
	}
	if code := post(owner, "/admin/import", url.Values{"path": {folder}}); code != 303 {
		t.Fatalf("register a folder: %d", code)
	}
	looked()
	if got := recorded(); strings.Join(got, " ") != "a.jpg trip/b.jpg" {
		t.Fatalf("after the first look the library records %v; want a.jpg and trip/b.jpg — not the copy of a photo it has, the text file, or the hidden folder", got)
	}
	if after := listing(); after != before {
		t.Errorf("the folder was changed by being read:\n%s\n-- was --\n%s", after, before)
	}
	_, page := e.browse("/admin", owner)
	id := regexp.MustCompile(`action="/admin/import/scan"><input type="hidden" name="id" value="([0-9a-f-]{36})"`).FindStringSubmatch(page)
	if id == nil || !strings.Contains(page, "새로 2 · 이미 있던 사진 1") {
		t.Fatalf("the admin page must list the folder and how the look went: %s", page)
	}
	files, _ := e.store.FilesAt(ctx, id[1])
	if res, _ := e.browse("/api/assets/"+files["a.jpg"].AssetID+"/thumbnail", owner); res.StatusCode != 200 {
		t.Errorf("small picture of a photo in the folder: %d", res.StatusCode)
	}
	if res, _ := e.browse("/api/assets/"+files["a.jpg"].AssetID+"/original", owner); res.StatusCode != 200 {
		t.Errorf("a photo in the folder: %d", res.StatusCode)
	}

	// the list can be narrowed to the photos that lie in one place
	sees := func(path string) bool {
		_, page := e.browse(path, owner)
		return strings.Contains(page, "data-month")
	}
	if _, all := e.browse("/", owner); !strings.Contains(all, "모든 위치") || !sees("/?show=mine&at="+id[1]) || !sees("/?show=mine&at=home") {
		t.Error("with photos in two places the list must offer each place, and show the photos of each")
	}
	if _, in := e.browse("/web/month/2026-09?show=mine&at="+id[1], owner); !strings.Contains(in, files["a.jpg"].AssetID) || strings.Count(in, "data-photo") != 2 {
		t.Errorf("the photos of the folder alone: %s", in)
	}

	// switched off: gone from the library, kept on record; switched on: back, the same photos as before
	if code := post(owner, "/admin/import/pause", url.Values{"id": {id[1]}, "on": {"1"}}); code != 303 {
		t.Fatalf("switch the folder off: %d", code)
	}
	if res, _ := e.browse("/api/assets/"+files["a.jpg"].AssetID+"/thumbnail", owner); sees("/?show=mine&at="+id[1]) || res.StatusCode != 404 {
		t.Error("the photos of a folder that is switched off are still shown")
	}
	if got := recorded(); len(got) != 2 || listing() != before {
		t.Errorf("switching a folder off must keep what was read (%v on record) and leave the folder alone", got)
	}
	post(owner, "/admin/import/scan", url.Values{"id": {id[1]}}) // must do nothing while it is off
	post(owner, "/admin/import/pause", url.Values{"id": {id[1]}})
	looked()
	back, _ := e.store.FilesAt(ctx, id[1])
	if res, _ := e.browse("/api/assets/"+files["a.jpg"].AssetID+"/thumbnail", owner); !sees("/?show=mine&at="+id[1]) || res.StatusCode != 200 || back["a.jpg"].AssetID != files["a.jpg"].AssetID {
		t.Error("switched on again, the folder's photos must be back as the same photos, small pictures and all")
	}

	// deleted from the library: the file stays, and the next look does not bring the photo back
	post(owner, "/web/photos/delete", url.Values{"id": {files["a.jpg"].AssetID}})
	post(owner, "/trash", url.Values{"empty": {"1"}})
	if after := listing(); after != before {
		t.Errorf("deleting a photo from the library changed the folder:\n%s", after)
	}
	post(owner, "/admin/import/scan", url.Values{"id": {id[1]}})
	looked()
	if got := recorded(); strings.Join(got, " ") != "trip/b.jpg" {
		t.Errorf("after deleting a.jpg from the library and looking again: %v", got)
	}

	// the folder changes: a new file comes in, a file that is gone leaves
	put("c.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 440, 300))
	os.Remove(filepath.Join(folder, "trip", "b.jpg"))
	post(owner, "/admin/import/scan", url.Values{"id": {id[1]}})
	looked()
	if got := recorded(); strings.Join(got, " ") != "c.jpg" {
		t.Errorf("after the folder changed and was looked at again: %v", got)
	}

	// a file moved and renamed inside the folder stays the same photo
	was, _ := e.store.FilesAt(ctx, id[1])
	os.MkdirAll(filepath.Join(folder, "sorted"), 0o755)
	os.Rename(filepath.Join(folder, "c.jpg"), filepath.Join(folder, "sorted", "renamed.jpg"))
	post(owner, "/admin/import/scan", url.Values{"id": {id[1]}})
	looked()
	now, _ := e.store.FilesAt(ctx, id[1])
	if got := recorded(); strings.Join(got, " ") != "sorted/renamed.jpg" || now["sorted/renamed.jpg"].AssetID != was["c.jpg"].AssetID {
		t.Errorf("after a file was moved and renamed: %v — it must be the same photo as before, under its new path", got)
	}

	// taken out of the library again: the photos are forgotten, the files stay
	before = listing()
	if code := post(owner, "/admin/import/remove", url.Values{"id": {id[1]}}); code != 303 {
		t.Fatalf("unregister the folder: %d", code)
	}
	if got := recorded(); len(got) != 0 || listing() != before {
		t.Errorf("after unregistering: the library records %v, and the folder is\n%s", got, listing())
	}
}

// What the program keeps for itself lies with the photos, out of sight: someone who opens the data
// folder sees only their photos, and a fresh start on the same folder finds everything again.
func TestTheDataFolderShowsOnlyPhotos(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	e.upload(e.login("owner@example.com", "owner"), "a.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300))
	e.settle()
	entries, _ := os.ReadDir(dir)
	var seen []string
	for _, entry := range entries {
		seen = append(seen, entry.Name())
	}
	if strings.Join(seen, " ") != ".stow originals" {
		t.Errorf("the data folder holds %v; want only .stow and originals", seen)
	}
	// a data folder from before: the database and the small pictures beside the photos
	old := t.TempDir()
	os.WriteFile(filepath.Join(old, "stow.db"), []byte("database"), 0o644)
	os.MkdirAll(filepath.Join(old, "derived", "x"), 0o755)
	hidden, err := library.Hidden(old)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := os.ReadFile(filepath.Join(hidden, "stow.db")); err != nil || string(moved) != "database" {
		t.Errorf("the old database was not taken along: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hidden, "derived", "x")); err != nil {
		t.Errorf("the old small pictures were not taken along: %v", err)
	}
}

// A live photo — a photo and a few seconds of video under one name — is one photo in the library: the
// video is listed nowhere by itself, plays from the photo, and goes where the photo goes.
func TestALivePhotoIsOnePhoto(t *testing.T) {
	if ffmpegPath == "" {
		t.Skip("ffmpeg is not installed")
	}
	data, folder := t.TempDir(), t.TempDir()
	clip := func(name string, seconds string) {
		t.Helper()
		out, err := exec.Command(ffmpegPath, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=d="+seconds+":s=320x240:r=15", "-pix_fmt", "yuv420p", filepath.Join(folder, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("ffmpeg: %v %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(folder, "IMG_0001.JPG"), mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300), 0o644)
	clip("IMG_0001.MOV", "2")
	os.WriteFile(filepath.Join(folder, "IMG_0002.JPG"), mediatest.JPEG(mediatest.Camera.Bytes(), 410, 300), 0o644)
	clip("IMG_0002.MOV", "8") // too long to be the moving half of a photo: a video of its own
	clip("IMG_0003.MOV", "2") // short, but no photo of its name

	e := start(t, data, newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	ctx := context.Background()
	post := func(who *http.Cookie, path string, form url.Values) {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(who)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	post(owner, "/admin/import", url.Values{"path": {folder}})
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, page := e.browse("/admin", owner); !strings.Contains(page, "data-running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the look at the folder did not end")
		}
	}
	e.settle()

	found, _ := e.store.Locations(ctx)
	files, _ := e.store.FilesAt(ctx, found[len(found)-1].ID)
	photo, half := files["IMG_0001.JPG"].AssetID, files["IMG_0001.MOV"].AssetID
	_, cells := e.browse("/web/month/2026-09", owner)
	_, all := e.browse("/", owner)
	if strings.Contains(cells+all, half) || !strings.Contains(cells, photo) || !strings.Contains(cells, "LIVE") {
		t.Errorf("a live photo must be listed as one photo marked LIVE, without its moving half: %s", cells)
	}
	for name, id := range map[string]string{"a long video beside a photo": files["IMG_0002.MOV"].AssetID, "a short video with no photo": files["IMG_0003.MOV"].AssetID} {
		listed := false
		for _, month := range []string{"2026-09", time.Now().Format("2006-01")} {
			if _, in := e.browse("/web/month/"+month, owner); strings.Contains(in, id) {
				listed = true
			}
		}
		if !listed {
			t.Errorf("%s must stay a video of its own in the list", name)
		}
	}
	if _, one := e.browse("/web/photo/"+photo, owner); !strings.Contains(one, `data-act="live"`) || !strings.Contains(one, "/api/assets/"+half+"/video/playback") {
		t.Errorf("a live photo, opened, must offer to play its moving half: %s", one)
	}
	if res, _ := e.browse("/api/assets/"+half+"/video/playback", owner); res.StatusCode != 200 {
		t.Errorf("the moving half of a live photo: %d", res.StatusCode)
	}
	// shared, the moving half is shared with it; in the trash, it is gone with it
	if res, _ := e.browse("/api/assets/"+half+"/video/playback", kid); res.StatusCode != 404 {
		t.Errorf("the moving half of a photo that is not shared, for someone else: %d", res.StatusCode)
	}
	post(owner, "/web/photos/share", url.Values{"id": {photo}, "on": {"1"}})
	if res, _ := e.browse("/api/assets/"+half+"/video/playback", kid); res.StatusCode != 200 {
		t.Errorf("the moving half of a shared live photo, for someone else: %d", res.StatusCode)
	}
	post(owner, "/web/photos/delete", url.Values{"id": {photo}})
	if res, _ := e.browse("/api/assets/"+half+"/video/playback", kid); res.StatusCode != 404 {
		t.Errorf("the moving half of a live photo that is in the trash, for someone else: %d", res.StatusCode)
	}
	if _, trash := e.browse("/trash", owner); !strings.Contains(trash, photo) || strings.Contains(trash, half) {
		t.Errorf("the trash must show a live photo as one photo: %s", trash)
	}
	post(owner, "/trash", url.Values{"empty": {"1"}})
	if left, _ := e.store.FilesAt(ctx, found[len(found)-1].ID); left["IMG_0001.JPG"].AssetID != "" || left["IMG_0001.MOV"].AssetID != "" {
		t.Errorf("deleting a live photo must take its moving half along: %v", left)
	}
}

// A server used at home keeps its own accounts: the first visitor makes the owner's, the owner makes the
// others, and a login is an address, a password and a session cookie — in the browser and in the app.
func TestAServerWithItsOwnAccounts(t *testing.T) {
	ownAccounts = true
	defer func() { ownAccounts = false }()
	e := start(t, t.TempDir(), newKey(t))
	form := func(path string, values url.Values, cookies ...*http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	session := func(res *http.Response) *http.Cookie {
		for _, c := range res.Cookies() {
			if c.Name == "stow_session" && c.Value != "" {
				return c
			}
		}
		return nil
	}
	app := func(email, password string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"email": email, "password": password})
		res := e.do("POST", "/api/auth/login", "", bytes.NewReader(body), "application/json")
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	// nobody yet: every way in leads to making the first account
	if res, _ := e.browse("/"); res.Header.Get("Location") != "/login" {
		t.Fatalf("without a session: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := e.browse("/login"); res.Header.Get("Location") != "/setup" {
		t.Errorf("the login page of a server nobody has set up: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := form("/setup", url.Values{"name": {"Me"}, "email": {"me@home"}, "password": {"abcd"}, "again": {"abce"}}); res.StatusCode != 400 {
		t.Errorf("two different passwords at setup: %d", res.StatusCode)
	}
	first := form("/setup", url.Values{"name": {"Me"}, "email": {"me@home"}, "password": {"abcd"}, "again": {"abcd"}})
	owner := session(first)
	if first.StatusCode != 303 || owner == nil {
		t.Fatalf("setup: %d, session %v", first.StatusCode, owner)
	}
	if res := form("/setup", url.Values{"name": {"Thief"}, "email": {"thief@home"}, "password": {"abcd"}, "again": {"abcd"}}); session(res) != nil {
		t.Error("a second owner was made after the server was set up")
	}

	// the first steps after the account: screen, folders already here, auto-sorting, then outside and the phone
	if to := first.Header.Get("Location"); to != "/start/screen" {
		t.Errorf("after setup: to %q, want the first steps", to)
	}
	for path, want := range map[string]string{"/start/screen": `data-theme-set="dark"`, "/start/folders": `data-next="/start/folders"`, "/start/auto": `action="/admin/coupons"`} {
		if res, page := e.browse(path, owner); res.StatusCode != 200 || !strings.Contains(page, want) {
			t.Errorf("%s: %d %s", path, res.StatusCode, page)
		}
	}
	if _, page := e.browse("/start/folders", owner); !strings.Contains(page, "업로드 없이 이 장비의 사진을 인덱싱하여 봅니다") {
		t.Error("the folders step must say what it is for")
	}
	if _, page := e.browse("/welcome", owner); !strings.Contains(page, ">설정 완료</a>") || !strings.Contains(page, "백그라운드에서 동작합니다") {
		t.Error("the phone step must end with 설정 완료, and say the app keeps running when its window closes")
	}
	if res := form("/admin/coupons", url.Values{"on": {"1"}, "next": {"/start/auto"}}, owner); res.Header.Get("Location") != "/start/auto" {
		t.Errorf("a switch on a first step must come back to it: %q", res.Header.Get("Location"))
	}
	if res := form("/admin/coupons", url.Values{"next": {"https://elsewhere.example/"}}, owner); res.Header.Get("Location") != "/admin" {
		t.Errorf("a switch must not send anyone elsewhere: %q", res.Header.Get("Location"))
	}
	if res := form("/admin/import", url.Values{"path": {"/no/such/folder"}, "next": {"/start/folders"}}, owner); res.StatusCode != 400 {
		t.Errorf("a folder that cannot be read, from the first step: %d", res.StatusCode)
	}
	if _, page := e.browse("/admin", owner); !strings.Contains(page, "사용자 더하기") {
		t.Errorf("the first account must own the server and see the accounts: %s", page)
	}

	// the owner makes an account for someone else, who is not an owner
	if res := form("/admin/users", url.Values{"name": {"Mom"}, "email": {"mom@home"}, "password": {"1234"}}, owner); res.StatusCode != 303 {
		t.Fatalf("add a user: %d", res.StatusCode)
	}
	if res := form("/admin/users", url.Values{"name": {"Mom again"}, "email": {"MOM@home"}, "password": {"1234"}}, owner); res.StatusCode != 400 {
		t.Errorf("a second account for the same address: %d", res.StatusCode)
	}
	mom := session(form("/login", url.Values{"email": {"mom@home"}, "password": {"1234"}}))
	if mom == nil {
		t.Fatal("the new user could not log in")
	}
	if res, _ := e.browse("/admin", mom); res.StatusCode != 404 {
		t.Errorf("the admin page for someone who is not the owner: %d", res.StatusCode)
	}
	if res := form("/admin/users", url.Values{"name": {"X"}, "email": {"x@home"}, "password": {"1234"}}, mom); res.StatusCode != 404 {
		t.Errorf("someone who is not the owner made an account: %d", res.StatusCode)
	}

	// the app logs in with the same address and password, and its token works
	if code, _ := app("mom@home", "wrong"); code != 401 {
		t.Errorf("the app with a wrong password: %d", code)
	}
	code, in := app("mom@home", "1234")
	if code != 201 || in["accessToken"] == nil || in["isAdmin"] != false {
		t.Fatalf("the app's login: %d %v", code, in)
	}
	if status, up := e.upload(in["accessToken"].(string), "a.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)); status != 201 {
		t.Errorf("an upload with the token from a password login: %d %v", status, up)
	}
	res := e.do("GET", "/api/server/features", "", nil, "")
	var features map[string]any
	json.NewDecoder(res.Body).Decode(&features)
	res.Body.Close()
	if features["passwordLogin"] != true || features["oauth"] != false {
		t.Errorf("the app must be told to ask for a password, not to go to tidoly: %v", features)
	}

	// a new password from the owner replaces the old one
	form("/admin/users/password", url.Values{"user": {in["userId"].(string)}, "password": {"5678"}}, owner)
	if session(form("/login", url.Values{"email": {"mom@home"}, "password": {"1234"}})) != nil {
		t.Error("the old password still works")
	}
	if session(form("/login", url.Values{"email": {"mom@home"}, "password": {"5678"}})) == nil {
		t.Error("the new password does not work")
	}
	// and the person can make it their own: the one they have now, then the new one twice
	mom = session(form("/login", url.Values{"email": {"mom@home"}, "password": {"5678"}}))
	if _, page := e.browse("/", mom); !strings.Contains(page, `href="/me/password"`) {
		t.Error("no way to one's own password from the account menu")
	}
	if res := form("/me/password", url.Values{"current": {"1234"}, "password": {"9999"}, "again": {"9999"}}, mom); res.StatusCode != 400 {
		t.Errorf("own password with a wrong one now: %d", res.StatusCode)
	}
	if res := form("/me/password", url.Values{"current": {"5678"}, "password": {"9999"}, "again": {"9999"}}, mom); res.StatusCode != 200 {
		t.Errorf("own password: %d", res.StatusCode)
	}
	if session(form("/login", url.Values{"email": {"mom@home"}, "password": {"9999"}})) == nil {
		t.Error("one's own new password does not work")
	}
	// a run of wrong guesses makes that address wait, even with the right password
	for i := 0; i < 5; i++ {
		form("/login", url.Values{"email": {"me@home"}, "password": {"nope"}})
	}
	if session(form("/login", url.Values{"email": {"me@home"}, "password": {"abcd"}})) != nil {
		t.Error("after five wrong passwords the address must wait")
	}
}

// The server's own accounts log in only at home. Through the tunnel a request arrives from this machine
// (frpc hands it over from 127.0.0.1) but was sent to the domain: such a request must not get in with an
// address and a password, nor make the first account, nor walk this machine's folders.
func TestOwnAccountsLogInOnlyAtHome(t *testing.T) {
	ownAccounts = true
	defer func() { ownAccounts = false }()
	e := start(t, t.TempDir(), newKey(t))
	const away = "demo.stow.tidoly.app"
	send := func(host, method, path string, body io.Reader, contentType string, cookies ...*http.Cookie) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.server.URL+path, body)
		req.Host = host
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		page, _ := io.ReadAll(res.Body)
		return res, string(page)
	}
	form := func(host, path string, values url.Values, cookies ...*http.Cookie) (*http.Response, string) {
		t.Helper()
		return send(host, "POST", path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded", cookies...)
	}
	app := func(host, email, password string) (*http.Response, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"email": email, "password": password})
		return send(host, "POST", "/api/auth/login", bytes.NewReader(body), "application/json")
	}
	session := func(res *http.Response) *http.Cookie {
		for _, c := range res.Cookies() {
			if (c.Name == "stow_session" || c.Name == cookieToken) && c.Value != "" {
				return c
			}
		}
		return nil
	}
	home := strings.TrimPrefix(e.server.URL, "http://")
	// another address than the other tests': wrong tries are counted per address, for the whole program
	owner := url.Values{"name": {"Me"}, "email": {"me@house"}, "password": {"abcd"}, "again": {"abcd"}}

	// before anyone has set it up: from outside, no way to the first account
	if res, page := send(away, "GET", "/login", nil, ""); res.StatusCode != 200 || !strings.Contains(page, "집 안에서만") || strings.Contains(page, `action="/login"`) {
		t.Errorf("the login page from outside, before setup: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := form(away, "/setup", owner); session(res) != nil || !e.api.NeedsOwner(context.Background()) {
		t.Fatal("the first account was made from outside")
	}
	if res, page := form(home, "/setup", owner); session(res) == nil {
		t.Fatalf("the first account could not be made at home: %d %q %.300s", res.StatusCode, res.Header.Get("Location"), page)
	}

	// the right password from outside: refused before it is looked at, and not counted as a wrong try
	for i := 0; i < 6; i++ {
		if res, page := form(away, "/login", url.Values{"email": {"me@house"}, "password": {"abcd"}}); res.StatusCode != 403 || session(res) != nil || !strings.Contains(page, "집 안에서만") {
			t.Fatalf("a browser's password login from outside: %d, session %v", res.StatusCode, session(res))
		}
		if res, page := app(away, "me@house", "abcd"); res.StatusCode != 403 || session(res) != nil || strings.Contains(page, "accessToken") {
			t.Fatalf("the app's password login from outside: %d %s", res.StatusCode, page)
		}
	}
	if _, page := send(away, "GET", "/api/server/features", nil, ""); !strings.Contains(page, `"passwordLogin":false`) {
		t.Errorf("the app must not be offered a password login from outside: %s", page)
	}
	// at home the same password works — six refusals outside did not make it wait
	browser, _ := form(home, "/login", url.Values{"email": {"me@house"}, "password": {"abcd"}})
	cookie := session(browser)
	if cookie == nil {
		t.Fatalf("the password login at home: %d", browser.StatusCode)
	}
	if res, page := app(home, "me@house", "abcd"); res.StatusCode != 201 || !strings.Contains(page, "accessToken") {
		t.Errorf("the app's password login at home: %d", res.StatusCode)
	}
	if _, page := send(home, "GET", "/api/server/features", nil, ""); !strings.Contains(page, `"passwordLogin":true`) {
		t.Errorf("the app must be offered a password login at home: %s", page)
	}

	// this machine's folders are walked only at home; the rest of the admin page works anywhere
	if res, _ := send(away, "GET", "/admin/folders?at=", nil, "", cookie); res.StatusCode != 403 {
		t.Errorf("walking the folders from outside: %d", res.StatusCode)
	}
	for _, path := range []string{"/admin/locations", "/admin/import"} {
		if res, _ := form(away, path, url.Values{"path": {t.TempDir()}}, cookie); res.StatusCode != 403 {
			t.Errorf("%s from outside: %d", path, res.StatusCode)
		}
	}
	if res, page := send(away, "GET", "/admin", nil, "", cookie); res.StatusCode != 200 || strings.Contains(page, "data-browse") || !strings.Contains(page, "연결 설정") {
		t.Errorf("the admin page from outside: %d", res.StatusCode)
	}
	if res, page := send(home, "GET", "/admin", nil, "", cookie); res.StatusCode != 200 || !strings.Contains(page, "data-browse") {
		t.Errorf("the admin page at home: %d", res.StatusCode)
	}
	if res, _ := send(home, "GET", "/admin/folders?at=", nil, "", cookie); res.StatusCode != 200 {
		t.Errorf("walking the folders at home: %d", res.StatusCode)
	}
}

// An open web page hears of a photo the phone just uploaded, with its month, without reloading.
func TestWebPageHearsOfAnUpload(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.login("owner@example.com", "owner")
	browser := e.browserLogin("owner@example.com", "owner")
	req, _ := http.NewRequest("GET", e.server.URL+"/web/events", nil)
	req.AddCookie(browser)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	lines := bufio.NewScanner(res.Body)
	lines.Scan() // ": open"
	e.upload(owner, "a.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 40, 30))
	for lines.Scan() {
		if line := lines.Text(); strings.HasPrefix(line, "data: ") {
			if strings.Contains(line, `"event":"AssetUploadReadyV2"`) {
				if !strings.Contains(line, `"month":"2026-09"`) {
					t.Errorf("the month of the camera photo: %s", line)
				}
				return
			}
		}
	}
	t.Fatalf("no event for the upload: %v", lines.Err())
}

// The app shows how full the disk is: the one this person's photos go to, in Immich's form.
func TestStorageIsTheDiskThePhotosGoTo(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.login("owner@example.com", "owner")
	res := e.do("GET", "/api/server/storage", owner, nil, "")
	defer res.Body.Close()
	var got map[string]any
	json.NewDecoder(res.Body).Decode(&got)
	size, _ := got["diskSizeRaw"].(float64)
	use, _ := got["diskUseRaw"].(float64)
	free, _ := got["diskAvailableRaw"].(float64)
	pct, _ := got["diskUsagePercentage"].(float64)
	if size <= 0 || use+free != size || pct <= 0 || pct > 100 || !strings.HasSuffix(got["diskSize"].(string), "B") {
		t.Errorf("storage: %v", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 500 << 30: "500.0 GiB", 2 << 40: "2.0 TiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// The upload button is on the pages that show photos — not on admin, people or places — and an empty
// page offers a drop area that says where photos uploaded there go.
func TestUploadIsWherePhotosAre(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	browser := e.browserLogin("owner@example.com", "owner")
	for path, want := range map[string]string{"/": "폰의 Immich 앱으로", "/?show=shared": "바로 공유됩니다", "/?show=favorites": "바로 즐겨찾기", "/?show=videos": "동영상도"} {
		if _, page := e.browse(path, browser); !strings.Contains(page, `class="upload button quiet"`) || !strings.Contains(page, "data-pick") || !strings.Contains(page, want) {
			t.Errorf("%s: no upload button or drop area saying %q", path, want)
		}
	}
	for _, path := range []string{"/admin", "/help", "/people", "/places", "/trash"} {
		if _, page := e.browse(path, browser); strings.Contains(page, `class="upload button quiet"`) {
			t.Errorf("%s shows the upload button", path)
		}
	}
	token := e.login("owner@example.com", "owner")
	_, raw := e.call("POST", "/api/albums", token, map[string]any{"albumName": "여행"})
	var album map[string]any
	json.Unmarshal(raw, &album)
	if _, page := e.browse("/albums/"+album["id"].(string), browser); !strings.Contains(page, `class="upload button quiet"`) || !strings.Contains(page, "이 앨범에 들어갑니다") || !strings.Contains(page, `data-album="`) {
		t.Errorf("an empty album page: no upload into it")
	}
}

// The QR code leads a phone's camera to a page that needs no login and says what to do in the browser:
// install Immich, copy this server's address into it.
func TestQRLeadsToThePhonePage(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	res, page := e.browse("/phone")
	if res.StatusCode != 200 || !strings.Contains(page, "play.google.com") || !strings.Contains(page, "apps.apple.com") || !strings.Contains(page, `data-copy="http://light.test"`) {
		t.Fatalf("phone page: %d %s", res.StatusCode, page)
	}
}

// With tidoly's login the server takes no passwords at all.
// A person who only ever came with tidoly has no password here to guess, and the setup is over.
func TestNoPasswordsWithTidolyLogin(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	e.login("owner@example.com", "owner")
	body, _ := json.Marshal(map[string]string{"email": "owner@example.com", "password": ""})
	if res := e.do("POST", "/api/auth/login", "", bytes.NewReader(body), "application/json"); res.StatusCode != 401 {
		t.Errorf("a password login on a server that uses tidoly's: %d", res.StatusCode)
	}
	if res, _ := e.browse("/setup"); res.Header.Get("Location") != "/login" {
		t.Errorf("the setup page on a server that uses tidoly's login: %d", res.StatusCode)
	}
}

// The videos menu: only the videos, by date, of one's own and of what others share.
func TestVideosMenuListsOnlyVideos(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.login("owner@example.com", "owner")
	_, photo := e.upload(owner, "a.jpg", mediatest.JPEG(nil, 40, 30))
	_, clip := e.upload(owner, "clip.mp4", []byte("not much of a movie"))
	e.settle()
	browser := e.browserLogin("owner@example.com", "owner")
	_, page := e.browse("/?show=videos", browser)
	if !strings.Contains(page, "동영상 <small>1개</small>") || strings.Contains(page, `class="tabs"`) {
		t.Fatalf("videos page: %s", page)
	}
	at := strings.Index(page, `data-month="`)
	if at < 0 {
		t.Fatalf("videos page lists no month: %s", page)
	}
	month := page[at+len(`data-month="`):][:7]
	_, cells := e.browse("/web/month/"+month+"?show=videos", browser)
	if !strings.Contains(cells, clip["id"].(string)) || strings.Contains(cells, photo["id"].(string)) {
		t.Errorf("the month of the videos page must hold the video and not the photo: %s", cells)
	}
}

// TestFakeCouponHelper plays the coupon helper when the tests start it: the words of a picture whose path
// says "coupon" give its last day; others have no date.
func TestFakeCouponHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "fake-coupon-helper" {
		return
	}
	in, out := bufio.NewScanner(os.Stdin), json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct{ Picture string }
		json.Unmarshal(in.Bytes(), &req)
		switch {
		case strings.Contains(req.Picture, "old-coupon"):
			out.Encode(map[string]any{"lines": []string{"케이크", "교환기한 : 2020년 1월 3일"}})
		case strings.Contains(req.Picture, "coupon"):
			out.Encode(map[string]any{"lines": []string{"아메리카노", "유효기간", "2099.12.31"}})
		default:
			out.Encode(map[string]any{"lines": []string{"멤버십"}})
		}
	}
	os.Exit(0)
}

// barcodeJPEG is a picture with nothing in it but a barcode.
func barcodeJPEG(t *testing.T, text string) []byte {
	code, err := oned.NewCode128Writer().Encode(text, gozxing.BarcodeFormat_CODE_128, 600, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, code, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Coupons: off at first; switched on by the owner, the pictures already there are looked at and the ones
// with a barcode go to a "쿠폰" album — by their last day when the helper reads one, after those when it
// does not; one taken out of it stays out; new ones join by themselves.
func TestCouponsGatherInAnAlbumOnceSwitchedOn(t *testing.T) {
	withCoupons = true
	defer func() { withCoupons = false }()
	e := start(t, t.TempDir(), newKey(t))
	phone := e.login("owner@example.com", "owner")
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	up := func(name string, body []byte) string {
		_, a := e.upload(phone, name, body)
		return a["id"].(string)
	}
	coupon := up("coupon.jpg", barcodeJPEG(t, "123456789012"))
	member := up("member.jpg", barcodeJPEG(t, "987654321098")) // a barcode, no date in its words: in, its day not known
	plain := up("plain-coupon.jpg", mediatest.JPEG(nil, 401, 300))
	camera := up("camera-coupon.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 402, 300)) // taken with a camera: not looked at
	e.settle()
	post := func(who *http.Cookie, path string, form url.Values) int {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(who)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	album := func() (id, cells string) {
		t.Helper()
		_, page := e.browse("/albums", owner)
		m := regexp.MustCompile(`href="/albums/([0-9a-f-]{36})"`).FindStringSubmatch(page)
		if m == nil {
			return "", ""
		}
		_, inside := e.browse("/albums/"+m[1], owner) // the coupon album is drawn whole, by last day
		return m[1], inside
	}
	if id, _ := album(); id != "" {
		t.Fatal("an album was made while coupons were off")
	}
	if _, page := e.browse("/admin", owner); !strings.Contains(page, `action="/admin/coupons"`) {
		t.Fatalf("the admin page has no switch for coupons: %s", page)
	}
	if code := post(kid, "/admin/coupons", url.Values{"on": {"1"}}); code != 404 {
		t.Errorf("a member switched coupons on: %d", code)
	}
	if code := post(owner, "/admin/coupons", url.Values{"on": {"1"}}); code != 303 {
		t.Fatalf("switch coupons on: %d", code)
	}
	e.settle()
	id, cells := album()
	if _, page := e.browse("/albums", owner); !strings.Contains(page, ">쿠폰<") {
		t.Errorf("the coupon album is not called 쿠폰: %s", page)
	}
	if !strings.Contains(cells, coupon) || !strings.Contains(cells, member) || strings.Contains(cells, plain) || strings.Contains(cells, camera) {
		t.Fatalf("the coupon album must hold the pictures with a barcode only: %s", cells)
	}
	// taken out by its owner, a coupon stays out — also when coupons are switched off and on again
	if code := post(owner, "/web/album", url.Values{"album": {id}, "id": {coupon}, "remove": {"1"}}); code != 204 {
		t.Fatalf("take the coupon out: %d", code)
	}
	post(owner, "/admin/coupons", url.Values{})
	post(owner, "/admin/coupons", url.Values{"on": {"1"}})
	e.settle()
	if _, cells := album(); strings.Contains(cells, coupon) {
		t.Error("a coupon taken out of the album was put back")
	}
	// a new one joins by itself
	second := up("coupon-2.jpg", barcodeJPEG(t, "222222222222"))
	old := up("old-coupon.jpg", barcodeJPEG(t, "111111111111"))
	e.settle()
	// the coupon album: the ones still good by their last day, the past ones folded below
	_, page := e.browse("/albums/"+id, owner)
	good, unknown, gone := strings.Index(page, second), strings.Index(page, member), strings.Index(page, old)
	if good < 0 || unknown < 0 || gone < 0 || good > unknown || unknown > gone || !strings.Contains(page, "2099년 12월 31일까지") || !strings.Contains(page, "유효기간 모름") ||
		!strings.Contains(page, "지난 쿠폰 1장") || !strings.Contains(page, "2020년 1월 3일에 끝남") {
		t.Errorf("coupon album page: %s", page)
	}
}

// Without the coupon helper — Windows, Linux — a picture with a barcode still goes to the coupon album,
// its last day not known.
func TestCouponsGatherWithoutTheHelper(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	phone := e.login("owner@example.com", "owner")
	owner := e.browserLogin("owner@example.com", "owner")
	_, a := e.upload(phone, "coupon.jpg", barcodeJPEG(t, "123456789012"))
	_, b := e.upload(phone, "plain.jpg", mediatest.JPEG(nil, 401, 300))
	e.settle()
	if err := e.api.SetCoupons(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	e.settle()
	_, page := e.browse("/albums", owner)
	m := regexp.MustCompile(`href="/albums/([0-9a-f-]{36})"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no coupon album: %s", page)
	}
	_, inside := e.browse("/albums/"+m[1], owner)
	if !strings.Contains(inside, a["id"].(string)) || strings.Contains(inside, b["id"].(string)) || !strings.Contains(inside, "유효기간 모름") {
		t.Errorf("coupon album without the helper: %s", inside)
	}
}

// Memories: each day one, of this month's photos from one earlier year — not the same year two days running.
func TestMemoriesShowThisMonthFromAnEarlierYear(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	phone := e.login("owner@example.com", "owner")
	taken := func(when string, w int) string {
		x := mediatest.Camera
		x.Taken = when
		_, a := e.upload(phone, fmt.Sprintf("%d.jpg", w), mediatest.JPEG(x.Bytes(), w, 300))
		return a["id"].(string)
	}
	october21 := taken("2021:10:20 10:00:00", 400)
	october23 := taken("2023:10:05 10:00:00", 401)
	march22 := taken("2022:03:01 10:00:00", 402)
	thisYear := taken("2026:10:01 10:00:00", 403)
	e.settle()
	// what the phone holds after a full sync on a given day: its memories, their photos, and what was taken off
	sync := func(day int) (memories []map[string]any, inside map[string][]string, gone []string) {
		t.Helper()
		now := time.Date(2026, 10, day, 9, 0, 0, 0, time.Local)
		if err := e.api.MakeMemories(context.Background(), now); err != nil {
			t.Fatal(err)
		}
		if err := e.api.MakeMemories(context.Background(), now); err != nil { // a second run adds nothing
			t.Fatal(err)
		}
		res := e.do("POST", "/api/sync/stream", phone, strings.NewReader(`{"types":["AssetsV2","MemoriesV2","MemoryToAssetsV2"]}`), "application/json")
		defer res.Body.Close()
		inside = map[string][]string{}
		for scan := bufio.NewScanner(res.Body); scan.Scan(); {
			var line struct {
				Type string
				Data map[string]any
			}
			json.Unmarshal(scan.Bytes(), &line)
			switch line.Type {
			case "MemoryV2":
				memories = append(memories, line.Data)
			case "MemoryToAssetV2":
				inside[line.Data["memoryId"].(string)] = append(inside[line.Data["memoryId"].(string)], line.Data["assetId"].(string))
			case "MemoryDeleteV1":
				gone = append(gone, line.Data["memoryId"].(string))
			}
		}
		return memories, inside, gone
	}
	photoOf := map[float64]string{2021: october21, 2023: october23}
	check := func(day int, memories []map[string]any, inside map[string][]string) float64 {
		t.Helper()
		if len(memories) != 1 {
			t.Fatalf("day %d: want today's memory only, got %d: %v", day, len(memories), memories)
		}
		m := memories[0]
		year := m["data"].(map[string]any)["year"].(float64)
		at := fmt.Sprintf("2026-10-%d", day)
		if m["type"] != "on_this_day" || m["showAt"] != at+"T00:00:00.000Z" || m["hideAt"] != at+"T23:59:59.999Z" {
			t.Errorf("day %d: %v", day, m)
		}
		if got := inside[m["id"].(string)]; len(got) != 1 || got[0] != photoOf[year] {
			t.Errorf("day %d: the memory of %v holds %v", day, year, got)
		}
		for _, id := range inside[m["id"].(string)] {
			if id == march22 || id == thisYear {
				t.Errorf("day %d: a memory holds a photo of another month or of this year", day)
			}
		}
		return year
	}
	first, inside, gone := sync(14)
	a := check(14, first, inside)
	if len(gone) != 0 {
		t.Errorf("day 14: taken off before anything was shown: %v", gone)
	}
	second, inside, gone := sync(15)
	b := check(15, second, inside)
	if b == a || len(gone) != 1 || gone[0] != first[0]["id"] {
		t.Errorf("day 15: another year (%v after %v), and yesterday's taken off: %v", b, a, gone)
	}
	if third, _, gone := sync(16); len(third) != 0 || len(gone) != 2 {
		t.Errorf("day 16: both years shown this week rest — no memory; got %d, %d taken off", len(third), len(gone))
	}
	if again, inside, _ := sync(21); len(again) != 1 || check(21, again, inside) != a {
		t.Errorf("day 21: a week after, %v may show again", a)
	}
}

// The owner who forgot the password sets it again with the link the app's menu makes: only in a browser on
// this PC — not from another device at home, not through the tunnel — once, and not after it ends.
func TestTheOwnerSetsAForgottenPasswordAgainFromThisPC(t *testing.T) {
	ownAccounts = true
	defer func() { ownAccounts = false }()
	dir := t.TempDir()
	e := start(t, dir, newKey(t))
	ctx := context.Background()
	if _, err := e.api.AddLocalUser(ctx, "엄마", "mom@example.com", "forgotten"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.api.AddLocalUser(ctx, "아이", "kid@example.com", "1234"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "owner-reset")
	token, err := web.MakeOwnerReset(file)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("the link's file: %v", st.Mode())
	}
	ask := func(method, host, t2 string, form url.Values) (*http.Response, string) {
		t.Helper()
		if form == nil {
			form = url.Values{}
		}
		form.Set("t", t2)
		var req *http.Request
		if method == "GET" {
			req, _ = http.NewRequest("GET", e.server.URL+"/owner/reset?"+form.Encode(), nil)
		} else {
			req, _ = http.NewRequest("POST", e.server.URL+"/owner/reset", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Host = host
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(body)
	}
	for host, why := range map[string]string{"192.168.0.10:30891": "another device at home", testSlug + ".stow.tidoly.app": "through the tunnel"} {
		if res, _ := ask("GET", host, token, nil); res.StatusCode != 404 {
			t.Errorf("%s: %d", why, res.StatusCode)
		}
	}
	if res, _ := ask("GET", "127.0.0.1:30891", "not-it", nil); res.StatusCode != 404 {
		t.Errorf("a wrong link: %d", res.StatusCode)
	}
	if res, page := ask("GET", "127.0.0.1:30891", token, nil); res.StatusCode != 200 || !strings.Contains(page, "mom@example.com") {
		t.Fatalf("the link on this PC: %d", res.StatusCode)
	}
	if res, _ := ask("POST", "localhost:30891", token, url.Values{"password": {"newer"}, "again": {"newer!"}}); res.StatusCode != 400 {
		t.Errorf("two different passwords: %d", res.StatusCode)
	}
	res, _ := ask("POST", "localhost:30891", token, url.Values{"password": {"newer"}, "again": {"newer"}})
	if res.StatusCode != 303 || len(res.Cookies()) == 0 {
		t.Fatalf("setting it: %d", res.StatusCode)
	}
	body, _ := json.Marshal(map[string]string{"email": "mom@example.com", "password": "newer"})
	if res := e.do("POST", "/api/auth/login", "", bytes.NewReader(body), "application/json"); res.StatusCode != 201 {
		t.Errorf("logging in with the new password: %d", res.StatusCode)
	}
	if res, _ := ask("GET", "127.0.0.1:30891", token, nil); res.StatusCode != 404 {
		t.Errorf("the link a second time: %d", res.StatusCode)
	}
	token, _ = web.MakeOwnerReset(file)
	os.WriteFile(file, []byte(token+"\n"+strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)+"\n"), 0o600)
	if res, _ := ask("GET", "127.0.0.1:30891", token, nil); res.StatusCode != 404 {
		t.Errorf("a link past its time: %d", res.StatusCode)
	}
}

// The admin page's "가이드 보기" lands on the VPN guide inside the help, and the help says every way the
// owner gets back in.
func TestTheHelpHasTheGuidesThePagesPointTo(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	_, admin := e.browse("/admin", owner)
	if !strings.Contains(admin, `href="/help#vpn"`) {
		t.Fatal("the admin page does not point to the VPN guide")
	}
	res, help := e.browse("/help", owner)
	if res.StatusCode != 200 {
		t.Fatalf("help: %d", res.StatusCode)
	}
	reset := "관리자 비밀번호 변경" // from the app's menu; Linux has no menu
	if runtime.GOOS == "linux" {
		reset = "sudo tidoly-stow password"
	}
	for _, want := range []string{`<h3 id="vpn">`, `href="#vpn"`, `id="password"`, reset, "Login with tidoly", "먼저 등록을 해제하세요", `id="outside"`, `href="/start/screen"`,
		`href="` + testHome + `/contact" target="_blank"`, `data-copy="tidoly stow test-1 · `} {
		if !strings.Contains(help, want) {
			t.Errorf("the help lacks %q", want)
		}
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(` id="([a-z-]+)"`).FindAllStringSubmatch(help, -1) {
		if seen[m[1]] {
			t.Errorf("the anchor %q twice", m[1])
		}
		seen[m[1]] = true
	}
}

// An iPhone exports an edited copy beside the original (IMG_E1234 for IMG_1234) with its date and place
// stripped: read, it takes the original's, and does not show on the day it was exported.
func TestAnEditedCopyTakesTheOriginalsDate(t *testing.T) {
	data, folder := t.TempDir(), t.TempDir()
	e := start(t, data, newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	ctx := context.Background()
	taken := mediatest.Camera
	taken.Taken = "2015:05:22 11:42:45"
	os.WriteFile(filepath.Join(folder, "IMG_9308.JPG"), mediatest.JPEG(taken.Bytes(), 401, 300), 0o644)
	os.WriteFile(filepath.Join(folder, "IMG_E9308.JPG"), mediatest.JPEG(nil, 402, 300), 0o644) // no EXIF at all
	os.WriteFile(filepath.Join(folder, "IMG_E7777.JPG"), mediatest.JPEG(nil, 403, 300), 0o644) // an edit whose original is not here
	var me store.User
	users, _ := e.store.Users(ctx)
	for _, u := range users {
		if u.Email == "owner@example.com" {
			me = u
		}
	}
	id, err := e.api.AddFolder(ctx, folder, me.ID)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, page := e.browse("/admin", owner); !strings.Contains(page, "data-running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the look at the folder did not end")
		}
	}
	e.settle()
	files, _ := e.store.FilesAt(ctx, id)
	meta := func(name string) *store.Meta {
		a, err := e.store.AssetByID(ctx, files[name].AssetID)
		if err != nil || a.Meta == nil {
			t.Fatalf("%s: %v", name, err)
		}
		return a.Meta
	}
	orig, edit := meta("IMG_9308.JPG"), meta("IMG_E9308.JPG")
	if edit.LocalTime == "" || edit.LocalTime != orig.LocalTime || edit.TakenAt != orig.TakenAt {
		t.Errorf("the edited copy's time: %q, the original's %q", edit.LocalTime, orig.LocalTime)
	}
	if edit.Lat == nil || *edit.Lat != *orig.Lat || edit.Make != orig.Make {
		t.Errorf("the edited copy's place and camera: %v %q", edit.Lat, edit.Make)
	}
	if lone := meta("IMG_E7777.JPG"); lone.LocalTime != "" {
		t.Errorf("an edit without its original borrowed a time: %q", lone.LocalTime)
	}
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// WireGuard is off at first. The owner switches it on, sets the house's address and adds a phone at home;
// its QR shows at home only, and not to anyone but the owner.
func TestTheOwnerSetsUpWireGuardAtHome(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	owner := e.browserLogin("owner@example.com", "owner")
	kid := e.browserLogin("kid@example.com", "member")
	post := func(host, path string, form url.Values, who *http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = host
		req.AddCookie(who)
		res, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	home := "192.168.0.10:30891"
	if _, page := e.browse("/admin", owner); !strings.Contains(page, `action="/admin/vpn/switch"><input type="hidden" name="on" value="1"><button class="button quiet">켜기</button>`) {
		t.Fatal("WireGuard must be off at first, with a switch to turn it on")
	}
	if res := post(testSlug+".stow.tidoly.app", "/admin/vpn/switch", url.Values{"on": {"1"}}, owner); res.StatusCode != 403 {
		t.Errorf("switched on from outside: %d", res.StatusCode)
	}
	if res := post(home, "/admin/vpn/switch", url.Values{"on": {"1"}}, kid); res.StatusCode != 404 {
		t.Errorf("switched on by a member: %d", res.StatusCode)
	}
	if res := post(home, "/admin/vpn/switch", url.Values{"on": {"1"}}, owner); res.Header.Get("Location") != "/admin#vpn" {
		t.Fatalf("switch on: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	post(home, "/admin/vpn/endpoint", url.Values{"endpoint": {"203.0.113.7"}}, owner)
	post(home, "/admin/vpn/devices", url.Values{"name": {"엄마 폰"}}, owner)
	req, _ := http.NewRequest("GET", e.server.URL+"/admin", nil)
	req.Host = home
	req.AddCookie(owner)
	res, _ := noFollow.Do(req)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(body)
	m := regexp.MustCompile(`/admin/vpn/qr\?id=([0-9a-f]+)`).FindStringSubmatch(page)
	if !strings.Contains(page, "켜 둠") || !strings.Contains(page, `value="203.0.113.7"`) || !strings.Contains(page, "<b>엄마 폰</b>") || m == nil ||
		!strings.Contains(page, "<code>http://10.30.0.1:") {
		t.Fatalf("on, with the address and the phone: %s", page)
	}
	qr := func(host string, who *http.Cookie) *http.Response {
		req, _ := http.NewRequest("GET", e.server.URL+"/admin/vpn/qr?id="+m[1], nil)
		req.Host = host
		req.AddCookie(who)
		res, _ := noFollow.Do(req)
		res.Body.Close()
		return res
	}
	if res := qr(home, owner); res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("the phone's QR at home: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res := qr(testSlug+".stow.tidoly.app", owner); res.StatusCode != 404 {
		t.Errorf("the phone's QR from outside: %d", res.StatusCode)
	}
	if res := qr(home, kid); res.StatusCode != 404 {
		t.Errorf("the phone's QR to a member: %d", res.StatusCode)
	}
	if res := post(home, "/admin/vpn/switch", url.Values{}, owner); res.StatusCode != 303 {
		t.Errorf("switch off: %d", res.StatusCode)
	}
}
