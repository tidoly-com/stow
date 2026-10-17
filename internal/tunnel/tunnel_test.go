package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/server"
)

// A frps of our own, as tidoly's is meant to be: TLS only, and every login and proxy judged by a stand-in
// for tidoly-home's plugin that lets in one key for one slug.
type frps struct {
	addr, vhost, vhostTLS string
	ca                    []byte
	mu                    sync.Mutex
	keys                  []string // the keys the plugin was shown
}

func startFrps(t *testing.T) *frps {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)

	f := &frps{ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Op      string `json:"op"`
			Content struct {
				Metas     map[string]string                 `json:"metas"`
				User      struct{ Metas map[string]string } `json:"user"`
				Subdomain string                            `json:"subdomain"`
			} `json:"content"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		key := req.Content.Metas["stowkey"]
		if req.Op == "NewProxy" {
			key = req.Content.User.Metas["stowkey"]
		}
		f.mu.Lock()
		f.keys = append(f.keys, key)
		f.mu.Unlock()
		ok := key == "tdly_stow_mom" && (req.Op == "Login" || req.Content.Subdomain == "mom")
		json.NewEncoder(w).Encode(map[string]any{"reject": !ok, "reject_reason": "not allowed", "unchange": true})
	}))
	t.Cleanup(plugin.Close)

	bind, vhost, vhostTLS := freePort(t), freePort(t), freePort(t)
	cfg := &v1.ServerConfig{BindAddr: "127.0.0.1", BindPort: bind, VhostHTTPPort: vhost, VhostHTTPSPort: vhostTLS, SubDomainHost: "stow.test",
		HTTPPlugins: []v1.HTTPPluginOptions{{Name: "home", Addr: strings.TrimPrefix(plugin.URL, "http://"), Path: "/", Ops: []string{"Login", "NewProxy"}}}}
	cfg.Transport.TLS.Force = true
	cfg.Transport.TLS.CertFile, cfg.Transport.TLS.KeyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := cfg.Complete(); err != nil {
		t.Fatal(err)
	}
	svc, err := server.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx)
	t.Cleanup(func() { cancel(); svc.Close() })
	f.addr, f.vhost = net.JoinHostPort("127.0.0.1", strconv.Itoa(bind)), net.JoinHostPort("127.0.0.1", strconv.Itoa(vhost))
	f.vhostTLS = net.JoinHostPort("127.0.0.1", strconv.Itoa(vhostTLS))
	return f
}

func (f *frps) shown() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// get asks frps's web side for a page of <slug>.stow.test, as a phone outside would.
func (f *frps) get(slug string) (int, string) {
	req, _ := http.NewRequest("GET", "http://"+f.vhost+"/hello", nil)
	req.Host = slug + ".stow.test"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("waited in vain: %s", what)
}

func local(t *testing.T) int {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "stow at %s", r.Host)
	}))
	t.Cleanup(site.Close)
	return site.Listener.Addr().(*net.TCPAddr).Port
}

// The owner's key opens the owner's name, and requests to it reach this machine.
func TestTheTunnelCarriesRequestsToThisServer(t *testing.T) {
	f := startFrps(t)
	tun := &Tunnel{LocalPort: local(t), Dir: t.TempDir(), Roots: f.ca}
	defer tun.Close()
	tun.Set("mom", "tdly_stow_mom", f.addr, false) // the frps tidoly names
	waitFor(t, "a request through the tunnel", func() bool {
		code, body := f.get("mom")
		return code == 200 && body == "stow at mom.stow.test"
	})
	waitFor(t, "the state to say it is up", func() bool { return tun.State() == "" })
	tun.Close()
	waitFor(t, "the tunnel to close", func() bool { code, _ := f.get("mom"); return code != 200 })
}

// With a certificate of its own, the name's https comes through unopened: the phone outside meets this
// machine's certificate, not tidoly's. Its http still comes too.
func TestHTTPSComesThroughUnopened(t *testing.T) {
	f := startFrps(t)
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "stow over https at %s", r.Host)
	}))
	defer site.Close()
	tun := &Tunnel{LocalPort: local(t), TLSPort: site.Listener.Addr().(*net.TCPAddr).Port, Dir: t.TempDir(), Roots: f.ca}
	defer tun.Close()
	tun.Set("mom", "tdly_stow_mom", f.addr, true)
	var peer []byte
	waitFor(t, "a request over https through the tunnel", func() bool {
		conn, err := tls.Dial("tcp", f.vhostTLS, &tls.Config{ServerName: "mom.stow.test", InsecureSkipVerify: true})
		if err != nil {
			return false
		}
		defer conn.Close()
		peer = conn.ConnectionState().PeerCertificates[0].Raw
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: mom.stow.test\r\nConnection: close\r\n\r\n")
		body, _ := io.ReadAll(conn)
		return strings.Contains(string(body), "stow over https at mom.stow.test")
	})
	if !bytes.Equal(peer, site.Certificate().Raw) {
		t.Error("the certificate met outside is not this machine's: the tunnel opened the https")
	}
	waitFor(t, "the state to say it is up", func() bool { return tun.State() == "" })
	if code, body := f.get("mom"); code != 200 || body != "stow at mom.stow.test" {
		t.Errorf("http beside https: %d %s", code, body)
	}
}

// A key that is not that name's gets nothing, and the owner is told why that may be.
func TestAWrongKeyOpensNothing(t *testing.T) {
	f := startFrps(t)
	tun := &Tunnel{Server: f.addr, LocalPort: local(t), Dir: t.TempDir(), Roots: f.ca}
	defer tun.Close()
	tun.Set("mom", "tdly_stow_someone_else", "", false)
	waitFor(t, "the refusal", func() bool { return strings.Contains(tun.State(), "열쇠를 받지 않습니다") })
	if code, _ := f.get("mom"); code == 200 {
		t.Error("a wrong key opened the name")
	}
}

// Against a server whose certificate does not chain to the roots carried, the key is never sent.
func TestTheKeyGoesOnlyToTheRealServer(t *testing.T) {
	f := startFrps(t)
	tun := &Tunnel{Server: f.addr, LocalPort: local(t), Dir: t.TempDir()} // the roots carried here: Let's Encrypt's
	defer tun.Close()
	tun.Set("mom", "tdly_stow_mom", "", false)
	waitFor(t, "the connection to fail", func() bool { return strings.Contains(tun.State(), "닿지 않습니다") })
	if code, _ := f.get("mom"); code == 200 {
		t.Error("the name opened")
	}
	if keys := f.shown(); len(keys) != 0 {
		t.Errorf("the key reached a server that could not prove itself: %v", keys)
	}
}
