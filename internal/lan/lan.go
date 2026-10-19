// Package lan tells a request made at home from one that came from outside.
//
// What can only be done at home: logging in with this server's own address and password, making the
// first account, and walking this machine's folders. From outside one comes through the tunnel and logs in
// with tidoly (stow issue #4).
//
// The test is the address the request was sent to (its Host), not where it came from: the tunnel's
// client, frpc, runs on this machine and hands requests over from 127.0.0.1, but a request that came
// through it was sent to the domain — the tunnel's server routes by that name and passes it on. At home
// one uses this machine's address on the network (an IP address), localhost, or its .local name.
// A Host made up at home to look like the domain only takes rights away.
//
// A request that came over TLS is from outside, whatever its Host: this server takes https only on the
// port the tunnel hands the domain's https to, and the tunnel's server picks the way by the TLS name alone,
// never looking at the Host inside — sent with "Host: 127.0.0.1", it passed for one made at home (2026-10-19).
package lan

import (
	"net"
	"net/http"
	"strings"
)

// From says whether the request was made at home.
func From(r *http.Request) bool { return r.TLS == nil && Host(r.Host) }

// Host says whether an address one reaches this server by is a home one.
func Host(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
