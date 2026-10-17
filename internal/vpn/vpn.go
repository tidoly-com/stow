// Package vpn lets phones reach this server from anywhere over WireGuard, without tidoly's tunnel: the
// house's router forwards one UDP port here, and wireguard-go runs inside this program on a network stack
// of its own (netstack) — no network interface, no administrator rights (trials/2026-10-15-wireguard).
//
// Inside the tunnel the server is 10.30.0.1:<port>, a private address: the photo server takes requests
// there as made at home (internal/lan), so its own accounts and passwords work — over WireGuard one is
// "at home". Each phone or computer gets its own key and an address of its own (10.30.0.2, .3, …); a phone
// takes its settings from a QR code. Off unless the owner switches it on.
package vpn

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// ServerIP is this server inside the tunnel.
const ServerIP = "10.30.0.1"

// Device is a phone or computer allowed in.
type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IP      string `json:"ip"`      // its address inside the tunnel
	Public  string `json:"public"`  // its key, base64
	Private string `json:"private"` // kept so that its QR can be shown again; never leaves home
	Added   string `json:"added"`
}

// Config is what is kept between runs.
type Config struct {
	On       bool     `json:"on"`
	Private  string   `json:"private"`  // this server's key, base64
	Endpoint string   `json:"endpoint"` // the house's address outside, as a phone reaches it: an IP or a name
	Devices  []Device `json:"devices"`
}

// VPN runs WireGuard while it is switched on.
type VPN struct {
	Port    int          // UDP, the one the router forwards
	Handler http.Handler // the photo server, served inside the tunnel

	mu  sync.Mutex
	dev *device.Device
	srv *http.Server
}

// NewKey makes a WireGuard key pair, base64.
func NewKey() (private, public string, err error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return "", "", err
	}
	priv[0] &= 248 // a Curve25519 private key
	priv[31] = (priv[31] & 127) | 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return b64(priv), b64(pub), nil
}

// NextIP is the first address inside the tunnel no device has.
func NextIP(devices []Device) (string, error) {
	used := map[string]bool{ServerIP: true}
	for _, d := range devices {
		used[d.IP] = true
	}
	for i := 2; i < 255; i++ {
		ip := fmt.Sprintf("10.30.0.%d", i)
		if !used[ip] {
			return ip, nil
		}
	}
	return "", errors.New("no address left")
}

// PhoneConf is a device's settings for its WireGuard app: only this server goes through the tunnel.
func PhoneConf(c Config, d Device, port int) string {
	serverPub, _ := publicOf(c.Private)
	endpoint := c.Endpoint
	if _, _, err := net.SplitHostPort(endpoint); err != nil { // no port of its own: the router forwards this one
		endpoint = net.JoinHostPort(endpoint, fmt.Sprint(port))
	}
	return fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = %s/32\n\n[Peer]\nPublicKey = %s\nEndpoint = %s\nAllowedIPs = %s/32\nPersistentKeepalive = 25\n",
		d.Private, d.IP, serverPub, endpoint, ServerIP)
}

// Apply makes WireGuard match the config: down when off, up with these devices when on.
func (v *VPN) Apply(c Config) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stop()
	if !c.On {
		return nil
	}
	priv, err := hexKey(c.Private)
	if err != nil {
		return fmt.Errorf("this server's key: %w", err)
	}
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(ServerIP)}, nil, 1420)
	if err != nil {
		return err
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "vpn: "))
	var setting strings.Builder
	fmt.Fprintf(&setting, "private_key=%s\nlisten_port=%d\n", priv, v.Port)
	for _, d := range c.Devices {
		pub, err := hexKey(d.Public)
		if err != nil {
			continue
		}
		fmt.Fprintf(&setting, "public_key=%s\nallowed_ip=%s/32\n", pub, d.IP)
	}
	if err := dev.IpcSet(setting.String()); err != nil {
		dev.Close()
		return err
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return err
	}
	ln, err := tnet.ListenTCP(&net.TCPAddr{IP: net.ParseIP(ServerIP), Port: v.Port})
	if err != nil {
		dev.Close()
		return err
	}
	srv := &http.Server{Handler: v.Handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("vpn: %v", err)
		}
	}()
	v.dev, v.srv = dev, srv
	log.Printf("vpn: WireGuard on UDP %d, %d device(s)", v.Port, len(c.Devices))
	return nil
}

// Close takes WireGuard down.
func (v *VPN) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stop()
}

func (v *VPN) stop() {
	if v.srv != nil {
		v.srv.Close()
		v.srv = nil
	}
	if v.dev != nil {
		v.dev.Close()
		v.dev = nil
	}
}

func publicOf(private string) (string, error) {
	priv, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(priv) != 32 {
		return "", errors.New("not a key")
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	return b64(pub), err
}

func hexKey(k string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(b) != 32 {
		return "", errors.New("not a key")
	}
	return hex.EncodeToString(b), nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
