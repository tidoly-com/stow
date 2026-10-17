package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"

	"github.com/skip2/go-qrcode"
	"github.com/tidoly-com/stow/internal/lan"
	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/vpn"
)

// vpnState is what the connection settings show of WireGuard; nil when this server has none.
type vpnState struct {
	On       bool
	Endpoint string
	Port     int
	Home     string // this PC's address at home: where the router forwards the port to
	Server   string // the server's address inside the tunnel, for the Immich app
	Devices  []vpn.Device
}

func (w *Web) vpnState(ctx context.Context) *vpnState {
	if w.VPN == nil {
		return nil
	}
	c := w.VPN.Load(ctx)
	home := ""
	if u, err := url.Parse(w.Address); err == nil {
		home = u.Hostname()
	}
	return &vpnState{On: c.On, Endpoint: c.Endpoint, Port: w.VPN.VPN.Port, Home: home, Devices: c.Devices,
		Server: "http://" + net.JoinHostPort(vpn.ServerIP, fmt.Sprint(w.VPN.VPN.Port))}
}

// adminVPN switches WireGuard, sets the address outside, and adds or removes devices — at home only: the
// devices' keys are in what it shows.
func (w *Web) adminVPN(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.VPN == nil {
		http.NotFound(rw, r)
		return
	}
	if !lan.From(r) {
		w.adminPage(rw, r, user, "VPN 은 집 안에서 설정합니다 — 이 PC 와 같은 와이파이에서 열어 주세요.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	var err error
	switch r.URL.Path {
	case "/admin/vpn/switch":
		err = w.VPN.SetOn(ctx, r.FormValue("on") != "")
	case "/admin/vpn/endpoint":
		if err = w.VPN.SetEndpoint(ctx, r.FormValue("endpoint")); err != nil {
			w.adminPage(rw, r, user, "바깥 주소는 IP 나 이름(예: myhome.example.com)으로, 공유기가 다른 포트를 쓰면 뒤에 :포트 를 붙여 적어 주세요.", http.StatusBadRequest)
			return
		}
	case "/admin/vpn/devices":
		_, err = w.VPN.AddDevice(ctx, r.FormValue("name"))
	case "/admin/vpn/devices/remove":
		err = w.VPN.RemoveDevice(ctx, r.FormValue("id"))
	}
	if err != nil {
		log.Printf("web: vpn: %v", err)
		w.adminPage(rw, r, user, "VPN 을 바꾸지 못했습니다: "+err.Error(), http.StatusBadRequest)
		return
	}
	to := back(r)
	if to == "/admin" {
		to = "/admin#vpn"
	}
	http.Redirect(rw, r, to, http.StatusSeeOther)
}

// vpnQR is a device's settings as a QR code, for its WireGuard app to take — at home only.
func (w *Web) vpnQR(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.VPN == nil || !lan.From(r) {
		http.NotFound(rw, r)
		return
	}
	conf, err := w.VPN.Conf(r.Context(), r.FormValue("id"))
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	png, err := qrcode.Encode(conf, qrcode.Medium, 400)
	if err != nil {
		w.fail(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "image/png")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Write(png)
}

// adminUpdate starts installing the newer release — at home: it quits and reopens the app on this PC.
func (w *Web) adminUpdate(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.Update == nil {
		http.NotFound(rw, r)
		return
	}
	if !lan.From(r) {
		w.adminPage(rw, r, user, "업데이트는 집 안에서 합니다 — 이 PC 와 같은 와이파이에서 열어 주세요.", http.StatusForbidden)
		return
	}
	if err := w.Update.Install(r.Context()); err != nil {
		log.Printf("web: update: %v", err)
		w.adminPage(rw, r, user, "업데이트를 시작하지 못했습니다. 잠시 뒤에 다시 해 주세요.", http.StatusBadGateway)
		return
	}
	http.Redirect(rw, r, "/admin/update", http.StatusSeeOther)
}

// updating shows an update on its way: the page asks updateState until a newer version answers.
func (w *Web) updating(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.Update == nil {
		http.NotFound(rw, r)
		return
	}
	if w.Update.State() == "" {
		http.Redirect(rw, r, "/admin", http.StatusSeeOther)
		return
	}
	w.render(rw, "updating", map[string]any{"Title": "업데이트", "From": w.Update.Current, "To": w.Update.Newer()})
}

// updateState is this program's version and how its update is doing, for the page waiting on it.
func (w *Web) updateState(rw http.ResponseWriter, r *http.Request, user store.User) {
	if !user.IsAdmin || w.Update == nil {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(rw).Encode(map[string]string{"version": w.Update.Current, "state": w.Update.State()})
}
