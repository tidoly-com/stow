package lan

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestHost(t *testing.T) {
	for host, want := range map[string]bool{
		"192.168.0.20:30891":        true,
		"192.168.0.10":              true,
		"10.0.0.5:30891":            true, // WireGuard and other private networks
		"172.20.1.1:30891":          true,
		"127.0.0.1:30891":           true,
		"localhost:30891":           true,
		"LOCALHOST":                 true,
		"[::1]:30891":               true,
		"[fe80::1]:30891":           true,
		"[fd12:3456::1]:30891":      true,
		"macbook.local:30891":       true,
		"macbook.local.":            true,
		"demo.stow.tidoly.app":      false, // through the tunnel
		"demo.stow.tidoly.app:443":  false,
		"photos.example.com:30891":  false, // someone's own domain, forwarded by their router
		"203.0.113.7:30891":         false, // a public address
		"[2001:db8::1]:30891":       false,
		"192.168.0.10.nip.io:30891": false,
		"local":                     false,
		"evil-local:30891":          false,
		"":                          false,
	} {
		if got := Host(host); got != want {
			t.Errorf("Host(%q) = %v, want %v", host, got, want)
		}
	}
}

// Over TLS a request is from outside, even with a home Host: https comes only through the tunnel.
func TestTLSIsFromOutside(t *testing.T) {
	r := httptest.NewRequest("GET", "/login", nil)
	r.Host = "127.0.0.1"
	if !From(r) {
		t.Error("plain http to 127.0.0.1 is from home")
	}
	r.TLS = &tls.ConnectionState{ServerName: "demo.stow.tidoly.app"}
	if From(r) {
		t.Error("https with Host 127.0.0.1 passed for home")
	}
}
