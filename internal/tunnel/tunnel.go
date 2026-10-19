// Package tunnel keeps this server reachable from outside through tidoly's tunnel (stow issue #4): frp's
// client, run inside the program, holds a connection to tidoly's frps, which hands it the requests sent to
// <slug>.stow.tidoly.app.
//
// The frps asks tidoly-home, for every login and every proxy, whether the key the client carries belongs
// to that slug; anyone may run this program, so the key is what keeps a name its owner's. The key is only
// ever sent to the real frps: the client checks the server's certificate against the roots carried here
// (Let's Encrypt's), never trusting whatever answers at the address.
package tunnel

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

//go:embed roots.pem
var roots []byte

// Tunnel is the connection to tidoly's frps, for one slug at a time.
type Tunnel struct {
	Server    string // frps, host:port, given at start (-tunnel-server) over the one tidoly names; "" takes tidoly's
	LocalPort int    // where this server listens, on this machine
	TLSPort   int    // where it takes https with its own certificate, on 127.0.0.1; 0 if it does not
	Dir       string // where the roots file is written for frp to read
	Roots     []byte // the certificates frps's must chain to, PEM; nil for the ones carried here
	// Waiting asks tidoly where this server is in the line for a tunnel, when the frps says it waits for a
	// place: its place in the line, and when the next place opens. ok is false when it is not in the line,
	// or tidoly cannot be asked. nil: the line is told without them.
	Waiting func(ctx context.Context) (position int, opensAt time.Time, ok bool)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	state  string
}

// Set starts the tunnel for a slug and its key, to the frps tidoly named for it (host:port), or — with an
// empty slug — closes it. With https, the name's https goes, unopened, to TLSPort, where this server ends
// it with its own certificate; its http goes to LocalPort either way.
func (t *Tunnel) Set(slug, key, server string, https bool) {
	t.mu.Lock()
	cancel, done := t.cancel, t.done
	t.cancel, t.done = nil, nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if slug == "" {
		t.setState("")
		return
	}
	if t.Server != "" {
		server = t.Server
	}
	if server == "" {
		t.setState("터널 준비 중입니다 — tidoly 가 터널 서버를 정하면 저절로 열립니다")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	t.mu.Lock()
	t.cancel, t.done = cancel, done
	t.mu.Unlock()
	go func() {
		defer close(done)
		t.keep(ctx, slug, key, server, https && t.TLSPort > 0)
	}()
}

// Close ends the tunnel.
func (t *Tunnel) Close() { t.Set("", "", "", false) }

const connecting = "연결하는 중"

// Connecting says whether the tunnel is on its way up.
func (t *Tunnel) Connecting() bool { return t.State() == connecting }

// State says how the tunnel is doing, in words for the admin page: "" while it is up, or not wanted.
func (t *Tunnel) State() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *Tunnel) setState(s string) {
	t.mu.Lock()
	t.state = s
	t.mu.Unlock()
}

// keep runs the client, and runs it again a while after it stops, until told to end.
func (t *Tunnel) keep(ctx context.Context, slug, key, server string, https bool) {
	wait := 5 * time.Second
	for {
		t.setState(connecting)
		err := t.run(ctx, slug, key, server, https)
		if ctx.Err() != nil {
			return
		}
		log.Printf("tunnel: %v — again in %s", err, wait)
		t.setState(problem(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
}

// waitsForAPlace says whether the frps turned the name down because the week's new tunnels are taken: the
// address, key and certificate are this server's already, and frpc, trying again every 30 s, gets in by
// itself once its turn comes.
func waitsForAPlace(err string) bool { return strings.Contains(err, "waiting") }

const waiting = "터널 자리를 기다리는 중입니다 — 차례가 오면 저절로 연결되고 메일로 알려 드립니다"

// inLine puts the place in the line in words; the place is asked for again every so often, as it moves.
func inLine(position int, opensAt time.Time) string {
	return fmt.Sprintf("대기 %d번째 · 다음 자리는 %s 에 열립니다 · 차례가 오면 저절로 연결되고 메일로 알려 드립니다",
		position, opensAt.Local().Format("1월 2일 15:04"))
}

// problem puts what went wrong in words for the owner.
func problem(err error) string {
	text := err.Error()
	switch {
	case waitsForAPlace(text):
		return waiting
	case strings.Contains(text, "not allowed"):
		return "tidoly 가 이 서버의 열쇠를 받지 않습니다 — 다른 PC 가 같은 주소로 연결했거나 tidoly 가 이 주소를 멈췄습니다"
	default:
		return "터널 서버에 닿지 않습니다 — 다시 시도하는 중"
	}
}

// run holds one client until it fails or ctx ends.
func (t *Tunnel) run(ctx context.Context, slug, key, server string, https bool) error {
	host, port, err := net.SplitHostPort(server)
	if err != nil { // a name alone: frp's own port
		host, port = server, "7000"
	}
	ca := filepath.Join(t.Dir, "tunnel-roots.pem")
	trusted := roots
	if t.Roots != nil {
		trusted = t.Roots
	}
	if err := os.WriteFile(ca, trusted, 0o644); err != nil {
		return err
	}
	common := &v1.ClientCommonConfig{ServerAddr: host, Metadatas: map[string]string{"stowkey": key}}
	if common.ServerPort, err = strconv.Atoi(port); err != nil {
		return err
	}
	common.Transport.TLS.TrustedCaFile = ca // without it frp would take any certificate — and hand the key to anyone
	common.Transport.TLS.ServerName = host
	// frp turns heartbeats off when connections are multiplexed; without them home does not see that this
	// server is up, nor cut a key that was ended at once
	common.Transport.HeartbeatInterval, common.Transport.HeartbeatTimeout = 30, 90
	common.Log.To = "console"
	common.Log.Level = "warn"
	if err := common.Complete(); err != nil {
		return err
	}
	proxy := &v1.HTTPProxyConfig{
		ProxyBaseConfig: v1.ProxyBaseConfig{Name: slug, Type: "http", ProxyBackend: v1.ProxyBackend{LocalIP: "127.0.0.1", LocalPort: t.LocalPort}},
		DomainConfig:    v1.DomainConfig{SubDomain: slug},
	}
	proxy.Complete()
	proxies := []v1.ProxyConfigurer{proxy}
	names := []string{slug}
	if https {
		secure := &v1.HTTPSProxyConfig{
			ProxyBaseConfig: v1.ProxyBaseConfig{Name: slug + "-https", Type: "https", ProxyBackend: v1.ProxyBackend{LocalIP: "127.0.0.1", LocalPort: t.TLSPort}},
			DomainConfig:    v1.DomainConfig{SubDomain: slug},
		}
		secure.Complete()
		proxies = append(proxies, secure)
		names = append(names, slug+"-https")
	}
	configs := source.NewConfigSource()
	if err := configs.ReplaceAll(proxies, nil); err != nil {
		return err
	}
	svc, err := client.NewService(client.ServiceOptions{Common: common, ConfigSourceAggregator: source.NewAggregator(configs)})
	if err != nil {
		return err
	}
	failed := make(chan error, 1)
	go func() { failed <- svc.Run(ctx) }()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var asked time.Time // when the place in the line was last asked for: as often as frpc tries again
	for {
		select {
		case err := <-failed:
			if err == nil {
				err = errors.New("the tunnel closed")
			}
			return err
		case <-ctx.Done():
			svc.Close()
			<-failed
			return ctx.Err()
		case <-tick.C:
			// logged in; whether frps took the name is the proxies' state
			up := 0
			for _, name := range names {
				if status, ok := svc.StatusExporter().GetProxyStatus(name); ok {
					switch status.Phase {
					case "running":
						up++
					case "start error", "check failed":
						if !waitsForAPlace(status.Err) {
							t.setState(problem(errors.New(status.Err)))
							break
						}
						if time.Since(asked) < 30*time.Second {
							break
						}
						asked = time.Now()
						t.setState(waiting)
						if t.Waiting != nil {
							if position, opensAt, ok := t.Waiting(ctx); ok {
								t.setState(inLine(position, opensAt))
							}
						}
					}
				}
			}
			if up == len(names) {
				t.setState("")
			}
		}
	}
}
