// Package nearby lets the tidoly stow servers in the house be found: a server says it is there over
// multicast DNS (Bonjour), and a PC that only looks at photos — the app set up to show another PC's —
// lists them to choose from.
package nearby

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

const service = "_tidoly-stow._tcp"

// Server is one found in the house.
type Server struct {
	Name    string // what the server calls itself: its computer's name
	Address string // http://<address on the network>:<port>
}

// Announce says over the home network that a server is here, under name, until stop is called.
func Announce(name string, port int) (stop func(), err error) {
	s, err := zeroconf.Register(name, service, "local.", port, []string{"v=1"}, nil)
	if err != nil {
		return nil, err
	}
	return s.Shutdown, nil
}

// Find listens for the servers in the house for a while and returns them by name.
func Find(ctx context.Context, wait time.Duration) ([]Server, error) {
	r, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	var (
		mu    sync.Mutex
		found = map[string]Server{}
		done  = make(chan struct{})
	)
	go func() {
		defer close(done)
		for e := range entries {
			if ip := pick(e.AddrIPv4); ip != nil {
				s := Server{Name: unescape(e.Instance), Address: fmt.Sprintf("http://%s:%d", ip, e.Port)}
				mu.Lock()
				found[s.Address] = s
				mu.Unlock()
			}
		}
	}()
	if err := r.Browse(ctx, service, "local.", entries); err != nil {
		return nil, err
	}
	<-ctx.Done()
	<-done // the resolver closes entries once ctx is done
	out := make([]Server, 0, len(found))
	for _, s := range found {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pick takes the address a phone or a PC in the house would use: a private one before any other.
func pick(ips []net.IP) net.IP {
	for _, ip := range ips {
		if ip.IsPrivate() {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return nil
}

// unescape undoes DNS-SD's escaping of an instance name ("tidoly\ stow" → "tidoly stow").
func unescape(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '\\' && i+1 < len(name) {
			i++
		}
		b.WriteByte(name[i])
	}
	return b.String()
}
