package vpn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func freeUDP(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// phone plays a phone's WireGuard app from its settings, on a network stack of its own, and returns how to
// make HTTP requests through it.
func phone(t *testing.T, conf string) *http.Client {
	t.Helper()
	get := func(key string) string {
		for _, line := range strings.Split(conf, "\n") {
			if k, v, ok := strings.Cut(line, " = "); ok && k == key {
				return v
			}
		}
		t.Fatalf("no %s in %q", key, conf)
		return ""
	}
	priv, _ := hexKey(get("PrivateKey"))
	pub, _ := hexKey(get("PublicKey"))
	addr := strings.TrimSuffix(get("Address"), "/32")
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(addr)}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "phone: "))
	t.Cleanup(dev.Close)
	if err := dev.IpcSet(fmt.Sprintf("private_key=%s\npublic_key=%s\nendpoint=%s\nallowed_ip=%s/32\npersistent_keepalive_interval=1\n",
		priv, pub, get("Endpoint"), ServerIP)); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return tnet.DialContext(ctx, network, address)
	}}}
}

// A phone with its key reaches the server through the tunnel, at the server's private address; one whose
// key was taken out does not; switched off, nothing listens.
func TestAPhoneReachesTheServerThroughTheTunnel(t *testing.T) {
	port := freeUDP(t)
	v := &VPN{Port: port, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "stow, asked at %s", r.Host)
	})}
	defer v.Close()
	priv, _, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	c := Config{On: true, Private: priv, Endpoint: "127.0.0.1"}
	for _, name := range []string{"엄마 폰", "아빠 폰"} {
		ip, _ := NextIP(c.Devices)
		dp, dpub, _ := NewKey()
		c.Devices = append(c.Devices, Device{ID: name, Name: name, IP: ip, Public: dpub, Private: dp})
	}
	if c.Devices[0].IP != "10.30.0.2" || c.Devices[1].IP != "10.30.0.3" {
		t.Fatalf("addresses: %+v", c.Devices)
	}
	if err := v.Apply(c); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("http://%s:%d/api/server/ping", ServerIP, port)
	ask := func(client *http.Client) (string, error) {
		var last error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			res, err := client.Get(url)
			if err != nil {
				last = err
				continue
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return string(body), nil
		}
		return "", last
	}
	mom := phone(t, PhoneConf(c, c.Devices[0], port))
	if body, err := ask(mom); err != nil || body != fmt.Sprintf("stow, asked at %s:%d", ServerIP, port) {
		t.Fatalf("through the tunnel: %q %v", body, err)
	}

	dad := phone(t, PhoneConf(c, c.Devices[1], port))
	c.Devices = c.Devices[:1] // dad's phone taken out
	if err := v.Apply(c); err != nil {
		t.Fatal(err)
	}
	dad.Timeout = 700 * time.Millisecond
	if _, err := dad.Get(url); err == nil {
		t.Error("a phone taken out still got in")
	}

	c.On = false
	if err := v.Apply(c); err != nil {
		t.Fatal(err)
	}
	if l, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port)); err != nil {
		t.Errorf("switched off, the port is still held: %v", err)
	} else {
		l.Close()
	}
}

type memSettings map[string]string

func (m memSettings) Setting(_ context.Context, name string) string { return m[name] }
func (m memSettings) SetSetting(_ context.Context, name, value string) error {
	m[name] = value
	return nil
}

// Off at first. Switched on, it makes the server's key and looks up the address outside; the address may
// carry the router's own port; a device's settings name the address and only this server.
func TestTheManagerKeepsWhatTheOwnerSets(t *testing.T) {
	ctx := context.Background()
	port := freeUDP(t)
	m := &Manager{VPN: &VPN{Port: port, Handler: http.NotFoundHandler()}, Settings: memSettings{},
		Outside: func(context.Context) (string, error) { return "203.0.113.7", nil }}
	defer m.VPN.Close()
	if c := m.Load(ctx); c.On || c.Private != "" {
		t.Fatalf("not off at first: %+v", c)
	}
	if err := m.SetOn(ctx, true); err != nil {
		t.Fatal(err)
	}
	if c := m.Load(ctx); !c.On || c.Private == "" || c.Endpoint != "203.0.113.7" {
		t.Fatalf("switched on: %+v", c)
	}
	d, err := m.AddDevice(ctx, "엄마 폰")
	if err != nil {
		t.Fatal(err)
	}
	conf, _ := m.Conf(ctx, d.ID)
	if !strings.Contains(conf, fmt.Sprintf("Endpoint = 203.0.113.7:%d\n", port)) || !strings.Contains(conf, "AllowedIPs = 10.30.0.1/32") {
		t.Errorf("settings: %s", conf)
	}
	for _, bad := range []string{"a b", "http://x", "home.example:99999", ":40000"} {
		if m.SetEndpoint(ctx, bad) == nil {
			t.Errorf("%q taken as an address", bad)
		}
	}
	if err := m.SetEndpoint(ctx, "home.example.net:40000"); err != nil {
		t.Fatal(err)
	}
	if conf, _ := m.Conf(ctx, d.ID); !strings.Contains(conf, "Endpoint = home.example.net:40000\n") {
		t.Errorf("the router's own port: %s", conf)
	}
	if err := m.RemoveDevice(ctx, d.ID); err != nil || len(m.Load(ctx).Devices) != 0 {
		t.Errorf("remove: %v", err)
	}
	if err := m.SetOn(ctx, false); err != nil || m.Load(ctx).On {
		t.Errorf("switch off: %v", err)
	}
}
