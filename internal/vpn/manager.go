package vpn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings is where the config is kept: the server's settings table.
type Settings interface {
	Setting(ctx context.Context, name string) string
	SetSetting(ctx context.Context, name, value string) error
}

const setting = "vpn"

// Manager keeps the config and WireGuard in step.
type Manager struct {
	VPN      *VPN
	Settings Settings
	// Outside finds the house's address outside, when switching on without one; nil leaves it to the owner
	Outside func(ctx context.Context) (string, error)

	mu sync.Mutex
}

// Load is the config kept; off and empty when there is none.
func (m *Manager) Load(ctx context.Context) Config {
	var c Config
	if raw := m.Settings.Setting(ctx, setting); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// Start brings WireGuard up as it was left.
func (m *Manager) Start(ctx context.Context) error { return m.VPN.Apply(m.Load(ctx)) }

func (m *Manager) change(ctx context.Context, f func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.Load(ctx)
	if err := f(&c); err != nil {
		return err
	}
	raw, _ := json.Marshal(c)
	if err := m.Settings.SetSetting(ctx, setting, string(raw)); err != nil {
		return err
	}
	return m.VPN.Apply(c)
}

// SetOn switches WireGuard on or off. On the first time it makes this server's key, and looks for the
// house's address outside if none is set.
func (m *Manager) SetOn(ctx context.Context, on bool) error {
	return m.change(ctx, func(c *Config) error {
		c.On = on
		if on && c.Private == "" {
			priv, _, err := NewKey()
			if err != nil {
				return err
			}
			c.Private = priv
		}
		if on && c.Endpoint == "" && m.Outside != nil {
			if addr, err := m.Outside(ctx); err == nil {
				c.Endpoint = addr
			}
		}
		return nil
	})
}

// SetEndpoint sets the house's address outside: an IP, or a name (DDNS) that follows a changing one — with
// ":port" when the router forwards another port outside to this one. This server's port does not change.
func (m *Manager) SetEndpoint(ctx context.Context, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	host := endpoint
	if h, p, err := net.SplitHostPort(endpoint); err == nil {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("not a port")
		}
		host = h
	}
	if endpoint != "" && (host == "" || strings.ContainsAny(host, " /:@") || len(host) > 253) {
		return errors.New("not an address")
	}
	return m.change(ctx, func(c *Config) error { c.Endpoint = endpoint; return nil })
}

// AddDevice lets one more phone or computer in, under a name the owner knows it by.
func (m *Manager) AddDevice(ctx context.Context, name string) (Device, error) {
	var d Device
	err := m.change(ctx, func(c *Config) error {
		ip, err := NextIP(c.Devices)
		if err != nil {
			return err
		}
		priv, pub, err := NewKey()
		if err != nil {
			return err
		}
		id := make([]byte, 8)
		rand.Read(id)
		name = strings.TrimSpace(name)
		if name == "" {
			name = "기기 " + strings.TrimPrefix(ip, "10.30.0.")
		}
		if len([]rune(name)) > 60 {
			name = string([]rune(name)[:60])
		}
		d = Device{ID: hex.EncodeToString(id), Name: name, IP: ip, Public: pub, Private: priv, Added: time.Now().UTC().Format(time.RFC3339)}
		c.Devices = append(c.Devices, d)
		return nil
	})
	return d, err
}

// RemoveDevice shuts a phone or computer out: its key is no longer taken.
func (m *Manager) RemoveDevice(ctx context.Context, id string) error {
	return m.change(ctx, func(c *Config) error {
		for i, d := range c.Devices {
			if d.ID == id {
				c.Devices = append(c.Devices[:i], c.Devices[i+1:]...)
				return nil
			}
		}
		return errors.New("no such device")
	})
}

// Conf is one device's settings for its WireGuard app.
func (m *Manager) Conf(ctx context.Context, id string) (string, error) {
	c := m.Load(ctx)
	for _, d := range c.Devices {
		if d.ID == id {
			return PhoneConf(c, d, m.VPN.Port), nil
		}
	}
	return "", errors.New("no such device")
}

// PublicAddress asks a public echo service for the address this house has outside. It is asked only when
// WireGuard is switched on with no address set; the owner can set a name (DDNS) instead.
func PublicAddress(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64))
	ip := net.ParseIP(strings.TrimSpace(string(b)))
	if ip == nil {
		return "", errors.New("no address in the answer")
	}
	return ip.String(), nil
}
