// tidoly-light: one program on the owner's own machine that receives photos from the Immich phone app and
// keeps them in plain folders. At home people log in with the accounts it keeps; connected to tidoly, it is
// reached from outside through the tunnel and lets in the people tidoly-home vouches for.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tidoly-com/stow/internal/certs"
	"github.com/tidoly-com/stow/internal/coupons"
	"github.com/tidoly-com/stow/internal/disk"
	"github.com/tidoly-com/stow/internal/faces"
	"github.com/tidoly-com/stow/internal/immich"
	"github.com/tidoly-com/stow/internal/install"
	"github.com/tidoly-com/stow/internal/jobs"
	"github.com/tidoly-com/stow/internal/library"
	"github.com/tidoly-com/stow/internal/media"
	"github.com/tidoly-com/stow/internal/nearby"
	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/thumbs"
	"github.com/tidoly-com/stow/internal/tunnel"
	"github.com/tidoly-com/stow/internal/update"
	"github.com/tidoly-com/stow/internal/vpn"
	"github.com/tidoly-com/stow/internal/web"
)

func main() {
	dataDir := flag.String("data", "data", "folder for the photos and the database")
	configFile := flag.String("config", "", "settings file written by the setup (stow issue #18); without a folder in it, the setup is shown in the browser instead of the photos")
	listen := flag.String("listen", "127.0.0.1:30891", "address to listen on; 30891 is stow's port everywhere, inside the range a Kubernetes NodePort can take")
	publicURL := flag.String("public-url", "", "how phones reach this server at home (default: http://<listen>)")
	homeURL := flag.String("home-url", "https://www.tidoly.com", "tidoly-home, as a phone's browser reaches it")
	homeKey := flag.String("home-key", homeTicketKey, "tidoly-home's ticket public key, base64url")
	tunnelServer := flag.String("tunnel-server", "", "tidoly's frps for stow, host:port, over the one tidoly names when connecting; its certificate must be one Let's Encrypt signed")
	slug := flag.String("slug", "", "for trials: the slug this server answers for, instead of the one the owner connects to; its key is asked of home at start")
	workers := flag.Int("workers", 0, "background jobs to run at a time (default: half the processor cores, at least 2 and at most 8)")
	ffmpeg := flag.String("ffmpeg", "", "path to ffmpeg, for pictures of HEIC photos and videos (default: next to this program, then PATH)")
	faceHelper := flag.String("face-helper", "", "the face helper (stow-faces), or a command that starts one; without it no faces are looked for")
	couponHelper := flag.String("coupon-helper", "", "the coupon helper (macOS: helpers/coupons-mac), or a command that starts one; without it coupons go in without their last day")
	faceThreshold := flag.Float64("face-threshold", 0.363, "how alike two faces must be to count as one person (0 to 1)")
	name := flag.String("name", "", "what this server is called on the other devices in the house (default: this computer's name)")
	inspect := flag.String("inspect", "", "print what this program reads from one photo or video, then exit")
	updateURL := flag.String("update-url", "https://github.com/tidoly-com/stow/releases/latest/download", "where releases are downloaded from: latest.json, the app, SHA256SUMS")
	ownerPassword := flag.Bool("owner-password", false, "ask on this terminal for a new password for the server's owner, set it, and exit — for a server with no browser of its own (\"sudo tidoly-stow password\")")
	resetOwner := flag.Bool("reset-owner", false, "print a link, good once for 10 minutes and only in a browser on this PC, that sets the owner's password again; then exit")
	flag.Parse()
	if flag.Arg(0) == "password" { // sudo tidoly-stow password
		*ownerPassword = true
	}
	if *ownerPassword {
		if *configFile == "" && *dataDir == "data" {
			*configFile = serviceConfig()
		}
		if err := setOwnerPassword(*configFile, *dataDir); err != nil {
			fmt.Fprintln(os.Stderr, "tidoly stow:", err)
			os.Exit(1)
		}
		return
	}
	if *inspect != "" {
		info, err := media.Read(*inspect, time.Local)
		if err != nil {
			log.Fatal(err)
		}
		out, _ := json.MarshalIndent(info, "", "  ")
		fmt.Println(string(out))
		return
	}
	if *configFile != "" {
		cfg, _ := install.Load(*configFile)
		if cfg.Data == "" {
			runSetup(*configFile, *listen)
			return
		}
		*dataDir = cfg.Data
		if cfg.Update != "" { // set in the settings file, for trying a release
			*updateURL = cfg.Update
		}
	}
	if *workers <= 0 {
		// enough to get through a large folder in good time, few enough that the machine stays usable
		// and reasonably quiet while it does
		*workers = min(max(runtime.NumCPU()/2, 2), 8)
	}
	if *publicURL == "" {
		*publicURL = "http://" + reachable(*listen)
	}
	*homeURL = strings.TrimRight(*homeURL, "/")

	key, err := loadHomeKey(*homeKey, *homeURL, *slug != "")
	if err != nil {
		log.Fatalf("home key: %v", err)
	}
	if key == nil {
		log.Printf("no ticket key for tidoly-home: logging in with tidoly is off, only this server's own accounts work")
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	hidden, err := library.Hidden(*dataDir) // the database and the small pictures live with the photos, out of sight
	if err == nil && *resetOwner {
		token, err := web.MakeOwnerReset(filepath.Join(hidden, ownerResetFile))
		if err != nil {
			log.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(*listen)
		fmt.Println("http://127.0.0.1:" + port + "/owner/reset?t=" + token)
		return
	}
	if err != nil {
		log.Fatalf("data folder: %v", err)
	}
	st, err := store.Open(filepath.Join(hidden, "stow.db"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer st.Close()
	lib, err := library.Open(*dataDir)
	if err != nil {
		log.Fatalf("library: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopWithParent(stop)

	// The face helper: ready now, or — ours, whose model is fetched on first start — once the model is here.
	var helper, helperLater *faces.Helper
	sface := sfaceModel(hidden)
	if fields := command(*faceHelper); len(fields) == 1 && strings.HasPrefix(filepath.Base(fields[0]), "stow-faces") {
		h := faces.New([]string{fields[0], "-models", filepath.Dir(sface.Path)})
		defer h.Close()
		if exists(sface.Path) {
			helper = h
		} else {
			helperLater = h
		}
	} else if len(fields) > 0 {
		helper = faces.New(fields)
		defer helper.Close()
	}
	var couponHelp *coupons.Helper
	if fields := command(*couponHelper); len(fields) > 0 {
		couponHelp = coupons.New(fields)
		defer couponHelp.Close()
	}
	runner := jobs.NewRunner(st)
	_, port, _ := net.SplitHostPort(*listen)
	localPort, _ := strconv.Atoi(port)
	// https outside ends here, with this PC's own certificate, on a port of its own that only the tunnel uses
	tlsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("https: %v", err)
	}
	tun := &tunnel.Tunnel{Server: *tunnelServer, LocalPort: localPort, TLSPort: tlsListener.Addr().(*net.TCPAddr).Port, Dir: hidden}
	defer tun.Close()
	keeper := &certs.Keeper{Dir: filepath.Join(hidden, "tls"), Poll: 10 * time.Second, Retry: time.Hour}
	out := &outsideLink{ctx: ctx, tun: tun, keeper: keeper}
	api := immich.New(immich.Config{
		Faces: helper, FaceThreshold: *faceThreshold, Coupons: couponHelp,
		Name: serverName(*name), PublicURL: *publicURL, HomeURL: *homeURL, HomeKey: key, Link: immich.Link{Slug: *slug},
		Linked:   out.set,
		HTTPS:    keeper.Have,
		JobKinds: []string{"metadata", "thumbnail"},
		Thumbs:   &thumbs.Maker{Dir: filepath.Join(hidden, "derived"), FFmpeg: findFFmpeg(*ffmpeg)},
	}, st, lib, runner)
	updates := &update.Checker{Current: version, Base: *updateURL}
	// WireGuard from anywhere, on the same port number over UDP: off until the owner switches it on
	vpnSet := &vpn.Manager{VPN: &vpn.VPN{Port: localPort, Handler: api}, Settings: st, Outside: vpn.PublicAddress}
	defer vpnSet.VPN.Close()
	keeper.Home = api.CallHome
	keeper.Ready = func() { out.set(api.Link()) } // the tunnel takes https now

	if err := api.LoadLocations(ctx); err != nil {
		log.Fatalf("storage: %v", err)
	}
	pages := &web.Web{Store: st, User: api.SessionUser, Address: *publicURL, FacesState: api.FacesState, SetFaces: api.SetFaces, CouponDays: api.CouponDays(), SetCoupons: api.SetCoupons, ScreenshotsOn: api.ScreenshotsOn, SetScreenshots: api.SetScreenshots, Changed: api.Changed, Events: api.WebEvents,
		Dir: *dataDir, ResetFile: filepath.Join(hidden, ownerResetFile), Licenses: licenseFolder(), Version: version, VPN: vpnSet, Update: updates, LocationDir: api.LocationDir, AddLocation: api.AddLocation,
		NeedsOwner: api.NeedsOwner, LocalLogin: api.WebLocalLogin, AddUser: api.AddLocalUser, SetPassword: api.SetLocalPassword, ChangePassword: api.ChangeOwnPassword,
		TidolyHere: api.TidolyHere,
		Links:      web.LinkFuncs{On: api.LinksOn, Open: api.OpenLink, Unlock: api.UnlockLink, New: api.NewSharedLink, Address: api.LinkAddress, Hash: immich.HashPassword}, Outside: outside(api, tun, keeper), Connect: api.ConnectStart, ConnectBack: api.ConnectBack, Disconnect: api.Disconnect,
		SetAutoAdd: api.SetAutoAdd, Block: api.Block, Arrivals: api.Arrivals, SeenArrivals: api.SeenArrivals,
		Contact:   *homeURL + "/contact",
		AddFolder: api.AddFolder, PauseFolder: api.PauseFolder, ScanFolder: api.ScanFolder, RemoveFolder: api.RemoveFolder, FolderState: folderState(api), Trash: api.Trash, Restore: api.Restore, Empty: api.EmptyTrash, Days: immich.TrashDays}
	api.Pages(pages.Handler(api.WebLoginStart, api.WebLogout))
	runner.Handle("metadata", api.ReadMetadata)
	runner.Handle("place", api.NamePlace)
	runner.Handle("thumbnail", api.MakeThumbnail)
	runner.Handle(store.AskedKind, api.MakeFull)
	if helper != nil || helperLater != nil {
		runner.Handle("face", api.FindFaces)
	}
	if helper != nil {
		if n, err := api.QueueMissingFaces(ctx); err != nil {
			log.Printf("faces: %v", err)
		} else if n > 0 {
			log.Printf("faces: %d photos not looked at yet, queued", n)
		}
	}
	if helperLater != nil {
		api.WaitForFaces()
		go fetchFaceModel(ctx, sface, func() {
			if err := api.EnableFaces(ctx, helperLater); err != nil {
				log.Printf("faces: %v", err)
			}
			runner.Wake()
		})
	}
	runner.Handle("coupon", api.FindCoupon)
	if n, err := api.QueueMissingCoupons(ctx); err != nil {
		log.Printf("coupons: %v", err)
	} else if n > 0 {
		log.Printf("coupons: %d pictures not looked at yet, queued", n)
	}
	go func() { // photos read before a face repeated in one photo was taken as one
		if n, err := api.LookAgainAtRepeats(ctx); err != nil {
			log.Printf("faces: %v", err)
		} else if n > 0 {
			log.Printf("faces: %d photos with the same face more than once, looked at again", n)
		}
	}()
	go func() { // pictures read before screenshots were gathered, or while it was switched off
		if n, err := api.FileScreenshots(ctx); err != nil {
			log.Printf("screenshots: %v", err)
		} else if n > 0 {
			log.Printf("screenshots: %d put in the album", n)
		}
	}()
	if n, err := api.QueueUndated(ctx); err != nil {
		log.Printf("undated photos: %v", err)
	} else if n > 0 {
		log.Printf("undated photos: %d whose day can now be told, read again", n)
	}
	if n, err := api.QueueMissingThumbnails(ctx); err != nil {
		log.Printf("thumbnails: %v", err)
	} else if n > 0 {
		log.Printf("thumbnails: %d photos without small pictures, queued", n)
	}
	if n, err := st.QueueMissingPlaces(ctx); err != nil {
		log.Printf("places: %v", err)
	} else if n > 0 {
		log.Printf("places: %d photos with coordinates and no place name yet, queued", n)
	}
	go func() { // in the background: a large library must not keep the server from coming up
		if n, err := api.PairLivePhotos(ctx); err != nil {
			log.Printf("live photos: %v", err)
		} else if n > 0 {
			log.Printf("live photos: %d short videos put together with their photos", n)
		}
	}()
	go runner.Run(ctx, *workers)
	api.ScanFolders(ctx) // in the background: folders that are only read, as they are now
	go api.WatchMembers(ctx, 15*time.Second)
	if l := api.Link(); l.Key != "" { // connected before: open the tunnel again
		out.set(l)
	}
	if err := vpnSet.Start(ctx); err != nil {
		log.Printf("vpn: %v", err)
	}
	go updates.Run(ctx)
	go api.PurgeTrash(ctx)
	go api.Remember(ctx)

	srv := &http.Server{Addr: *listen, Handler: api, ReadHeaderTimeout: 10 * time.Second}
	secure := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{GetCertificate: keeper.Certificate, MinVersion: tls.VersionTLS12}}
	go func() {
		if err := secure.ServeTLS(tlsListener, "", ""); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("https: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		secure.Shutdown(shutdown)
	}()
	if l := api.Link(); l.Slug != "" {
		log.Printf("tidoly-light on %s (phones at home: %s, connected to tidoly as %q), data in %s", *listen, *publicURL, l.Slug, *dataDir)
	} else {
		log.Printf("tidoly-light on %s (phones at home: %s, not connected to tidoly), data in %s", *listen, *publicURL, *dataDir)
	}
	if port, open := openPort(*listen); open {
		// so that a PC set up only to look at the photos finds this one (internal/nearby)
		if stop, err := nearby.Announce(serverName(*name), port); err != nil {
			log.Printf("not announced on the home network: %v", err)
		} else {
			defer stop()
		}
	}
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// stopWithParent ends the program, as SIGTERM does, when the program that started it says so by closing
// its standard input — Windows has no SIGTERM to send. The Windows app sets STOW_STOP_ON_STDIN.
func stopWithParent(stop func()) {
	if os.Getenv("STOW_STOP_ON_STDIN") != "1" {
		return
	}
	go func() {
		io.Copy(io.Discard, os.Stdin)
		stop()
	}()
}

// command is a helper's command: a program's path as given — it may hold spaces — or, if no such file is
// there, words to split (a trial's "python helper.py models").
func command(given string) []string {
	if given == "" {
		return nil
	}
	if exists(given) {
		return []string{given}
	}
	return strings.Fields(given)
}

// sfaceModel is the face helper's model that is not carried in the app — 37 MB — fetched into the hidden
// folder on first start (stow issue #10). OpenCV's, Apache-2.0, at the commit it last changed in.
func sfaceModel(hidden string) install.Part {
	return install.Part{
		Name:   "얼굴 모델 (SFace)",
		URL:    "https://media.githubusercontent.com/media/opencv/opencv_zoo/ba91a3b91d00d76e86540d4013f944bd6b514e39/models/face_recognition_sface/face_recognition_sface_2021dec.onnx",
		SHA256: "0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79",
		Path:   filepath.Join(hidden, "models", "face_recognition_sface_2021dec.onnx"),
	}
}

// fetchFaceModel fetches the model, trying again every ten minutes while it cannot, then calls ready.
func fetchFaceModel(ctx context.Context, model install.Part, ready func()) {
	os.MkdirAll(filepath.Dir(model.Path), 0o755)
	for {
		log.Printf("faces: fetching %s", model.URL)
		var progress install.Progress
		install.Fetch(ctx, []install.Part{model}, &progress)
		if report := progress.Report(); report.Finished && report.Error == "" {
			log.Printf("faces: model in place")
			ready()
			return
		} else {
			log.Printf("faces: model not fetched: %s — again in ten minutes", report.Error)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

// serverName is what this server is called on the other devices: the name given, or this computer's.
func serverName(given string) string {
	if given != "" {
		return given
	}
	host, _ := os.Hostname()
	return strings.TrimSuffix(host, ".local")
}

// openPort is the port of an address listened on by the whole network, not only by this machine.
func openPort(listen string) (int, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, false
	}
	ip := net.ParseIP(host)
	return n, host == "" || (ip != nil && ip.IsUnspecified())
}

// findFFmpeg looks for ffmpeg where it was said to be, then beside this program, then on PATH.
func findFFmpeg(given string) string {
	candidates := []string{given}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "ffmpeg"), filepath.Join(filepath.Dir(self), "ffmpeg.exe"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if stat, err := os.Stat(c); err == nil && !stat.IsDir() {
			log.Printf("ffmpeg: %s", c)
			return c
		}
	}
	if found, err := exec.LookPath("ffmpeg"); err == nil {
		log.Printf("ffmpeg: %s (from PATH)", found)
		return found
	}
	log.Printf("ffmpeg not found: HEIC photos and videos will have no small pictures")
	return ""
}

// runSetup serves the first steps of the setup until the folder for the photos is chosen; then it ends,
// for whatever keeps this program running to start it again as the photo server.
func runSetup(configFile, listen string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopWithParent(stop)
	done := make(chan struct{})
	setup := &web.Install{Config: configFile, Suggest: suggestFolder(), Parts: parts(), Done: func() { close(done) }}
	srv := &http.Server{Addr: listen, Handler: setup.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("setup on %s: no folder for the photos yet", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// suggestFolder is where the setup offers to keep the photos: in the pictures folder where there is one.
func suggestFolder() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Windows and Linux: a second disk with more room than the home folder's is where photos go best — a
	// D: drive, a disk mounted at /data (2026-10-18). Not on the Mac: its other disks are mostly ones that
	// get unplugged.
	if runtime.GOOS != "darwin" {
		if dir, free := web.Roomiest(); dir != "" {
			_, homeFree, ok := disk.Space(home)
			if same, err := disk.Same(dir, home); ok && free > homeFree && err == nil && !same {
				return filepath.Join(dir, "stow")
			}
		}
	}
	if pictures := filepath.Join(home, "Pictures"); runtime.GOOS != "linux" || exists(pictures) {
		return filepath.Join(pictures, "stow")
	}
	return filepath.Join(home, "stow")
}

// parts lists what the setup fetches before the photo server starts. The Mac app carries ffmpeg next to
// this program; Windows and Linux installs are to fetch it here (stow issue #18).
func parts() []install.Part { return nil }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// homeTicketKey is tidoly-home's public key for tickets, carried in the program so that nothing at start
// can swap it — the one home shows at https://www.tidoly.com/stow/key. A trial with home's stand-in passes
// -home-key "" to take the stand-in's instead.
const homeTicketKey = "t-OYjz7KYMD5OGeCdyimO2fgUZ95vqPuZjWEVlwvNIM"

// licenseFolder is LICENSES/ beside the folder this program is in — where every installer puts the notices
// (the Mac app's Resources, the Windows and Linux tidoly-stow folder); "" when run from the source.
func licenseFolder() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(self)), "LICENSES")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// version is what this build is, set by the build scripts (-X main.version=…); shown in the help, to be copied
// into an inquiry.
var version = "dev"

// ownerResetFile, in the hidden folder, holds the secret of the link -reset-owner prints.
const ownerResetFile = "owner-reset"

// outsideLink keeps the tunnel and the certificate following the link to tidoly: the tunnel opens with
// the key and takes https once there is a certificate for the address, which is asked for on connecting.
type outsideLink struct {
	ctx    context.Context
	tun    *tunnel.Tunnel
	keeper *certs.Keeper

	mu     sync.Mutex
	host   string
	cancel context.CancelFunc
}

func (o *outsideLink) set(l immich.Link) {
	o.tun.Set(l.Slug, l.Key, l.Frps, o.keeper.Have(l.Host))
	host := l.Host
	if l.Key == "" {
		host = ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if host == o.host {
		return
	}
	if o.cancel != nil {
		o.cancel()
	}
	o.host, o.cancel = host, nil
	if host == "" {
		o.keeper.Keep(o.ctx, "") // forget
		return
	}
	ctx, cancel := context.WithCancel(o.ctx)
	o.cancel = cancel
	go o.keeper.Keep(ctx, host)
}

// outside tells the admin page how this server is reached from outside.
func outside(api *immich.Server, tun *tunnel.Tunnel, keeper *certs.Keeper) func(context.Context) web.Outside {
	return func(ctx context.Context) web.Outside {
		host := api.Link().Host
		return web.Outside{Host: host, Tunnel: tun.State(), Problem: api.LinkProblem(), AutoAdd: api.AutoAdd(ctx), Cert: keeper.State(), HTTPS: keeper.Have(host),
			Busy: tun.Connecting() || keeper.Asking(), Last: api.LastHost(ctx)}
	}
}

// loadHomeKey reads the pinned key, or — for trials, which give their own slug — asks home for it once.
func loadHomeKey(pinned, homeURL string, trial bool) (ed25519.PublicKey, error) {
	if pinned == "" {
		if !trial {
			return nil, nil
		}
		log.Printf("no -home-key given: trusting whatever %s answers with (trial use only)", homeURL)
		res, err := (&http.Client{Timeout: 10 * time.Second}).Get(homeURL + "/stow/key")
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			return nil, err
		}
		pinned = body.Key
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(pinned, "="))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("not an Ed25519 public key")
	}
	return key, nil
}

// reachable turns the address the server listens on into one a phone can use: listening on every
// address, that is this machine's address on the home network.
func reachable(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || (host != "" && host != "0.0.0.0" && host != "::") {
		return listen
	}
	if conn, err := net.Dial("udp", "192.0.2.1:9"); err == nil { // nothing is sent: this only asks which address would be used
		defer conn.Close()
		if at, ok := conn.LocalAddr().(*net.UDPAddr); ok && at.IP.IsPrivate() {
			return net.JoinHostPort(at.IP.String(), port)
		}
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// folderState hands the web pages what the server knows of the latest look at a read-only folder.
func folderState(api *immich.Server) func(string) (web.FolderState, bool) {
	return func(id string) (web.FolderState, bool) {
		scan := api.FolderScan(id)
		if scan == nil {
			return web.FolderState{}, false
		}
		failure, _ := scan.Failure.Load().(string)
		return web.FolderState{Running: scan.Running.Load(), Seen: scan.Seen.Load(), Added: scan.Added.Load(), Same: scan.Same.Load(), Gone: scan.Gone.Load(),
			Total: scan.Total.Load(), Counted: scan.Counted.Load(), Failure: failure}, true
	}
}
