package immich

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tidoly-com/stow/internal/store"
)

// The Immich app marks a photo as backed up when the server tells it so over a socket.io connection;
// without that it keeps showing the upload button until its next full sync (measured 2026-10-13).
// This is the small part of socket.io the app needs: the websocket transport of Engine.IO v4, the
// default namespace, and events going out. Nothing the app sends is acted on.

const (
	pingInterval = 25 * time.Second
	pingTimeout  = 20 * time.Second
)

type hub struct {
	mu    sync.Mutex
	conns map[string]map[chan string]struct{} // user id -> outgoing queues of that person's devices
}

func (h *hub) join(userID string) chan string {
	out := make(chan string, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns == nil {
		h.conns = map[string]map[chan string]struct{}{}
	}
	if h.conns[userID] == nil {
		h.conns[userID] = map[chan string]struct{}{}
	}
	h.conns[userID][out] = struct{}{}
	return out
}

func (h *hub) leave(userID string, out chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns[userID], out)
}

// emit sends an event to every connected device of one person. A device that is not keeping up misses
// it and catches up at its next sync.
func (h *hub) emit(userID, event string, data any) {
	payload, err := json.Marshal([]any{event, data})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for out := range h.conns[userID] {
		select {
		case out <- "42" + string(payload):
		default:
		}
	}
}

func (s *Server) socket(w http.ResponseWriter, r *http.Request, sess session) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out := s.hub.join(sess.user.ID)
	defer s.hub.leave(sess.user.ID, out)

	open, _ := json.Marshal(map[string]any{"sid": store.NewID(), "upgrades": []string{},
		"pingInterval": pingInterval.Milliseconds(), "pingTimeout": pingTimeout.Milliseconds(), "maxPayload": 1_000_000})
	if conn.Write(ctx, websocket.MessageText, append([]byte("0"), open...)) != nil {
		return
	}

	pong := make(chan struct{}, 1)
	go func() { // reader: the namespace handshake and the answers to our pings
		defer cancel()
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch text := string(msg); {
			case text == "3":
				select {
				case pong <- struct{}{}:
				default:
				}
			case strings.HasPrefix(text, "40"):
				reply, _ := json.Marshal(map[string]any{"sid": store.NewID()})
				select {
				case out <- "40" + string(reply):
				default:
				}
			case strings.HasPrefix(text, "41"):
				return
			}
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	deadline := time.NewTimer(pingInterval + pingTimeout)
	defer deadline.Stop()
	for {
		var packet string
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return // the phone stopped answering
		case <-pong:
			deadline.Reset(pingInterval + pingTimeout)
			continue
		case <-ping.C:
			packet = "2"
		case packet = <-out:
		}
		if conn.Write(ctx, websocket.MessageText, []byte(packet)) != nil {
			return
		}
	}
}

// WebEvents follows one person's changes for the web pages, which have no socket.io: each is a line of
// JSON with the event's name and, for a photo just made ready, its month ("2006-01"), so that a page can
// fetch that month again. stop ends it.
func (s *Server) WebEvents(userID string) (events <-chan string, stop func()) {
	in := s.hub.join(userID)
	out := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case msg := <-in:
				var ev []json.RawMessage
				if !strings.HasPrefix(msg, "42") || json.Unmarshal([]byte(msg[2:]), &ev) != nil || len(ev) == 0 {
					continue
				}
				var name, month string
				json.Unmarshal(ev[0], &name)
				if name == "AssetUploadReadyV2" && len(ev) > 1 {
					var ready struct {
						Asset struct {
							LocalDateTime string `json:"localDateTime"`
						} `json:"asset"`
					}
					if json.Unmarshal(ev[1], &ready) == nil && len(ready.Asset.LocalDateTime) >= 7 {
						month = ready.Asset.LocalDateTime[:7]
					}
				}
				line, _ := json.Marshal(map[string]string{"event": name, "month": month})
				select {
				case out <- string(line):
				default: // a page not keeping up misses it; it is only a hint to fetch again
				}
			}
		}
	}()
	var once sync.Once
	return out, func() { once.Do(func() { close(done); s.hub.leave(userID, in) }) }
}
