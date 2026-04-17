package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/brianbuquoi/overlord/internal/broker"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     checkOrigin,
}

// wsLimits controls the resource envelope every WebSocket connection
// operates within. The audit flagged the prior shape — unbounded
// client count, no read deadline, no ping/pong — as a denial-of-
// service path available to any authenticated tenant. Tuning these
// values is a trade-off between monitoring-tool friendliness and
// attacker cost; the defaults below chose values generous enough for
// typical dashboards (~hundreds of clients) and aggressive enough to
// close dead connections inside a minute.
const (
	// maxWSClients caps the total number of concurrent WebSocket
	// clients the hub registers. Connections arriving past this
	// point are refused at registration with a 503-style close
	// frame so legitimate clients see a clear signal rather than a
	// hung socket.
	maxWSClients = 512

	// wsPongWait is the maximum time the server waits between
	// client pong frames before considering the connection dead
	// and closing it. Paired with wsPingPeriod.
	wsPongWait = 60 * time.Second

	// wsPingPeriod is how often the server sends a ping frame. Must
	// be strictly less than wsPongWait so a single missed pong has a
	// chance to refresh the read deadline before the deadline
	// expires.
	wsPingPeriod = (wsPongWait * 9) / 10

	// wsWriteWait bounds every write (data, ping, close) so a slow
	// or wedged client cannot stall the writePump goroutine.
	wsWriteWait = 10 * time.Second
)

// checkOrigin validates the Origin header against the request Host to prevent
// cross-site WebSocket hijacking (CSWSH). Only same-origin connections are
// allowed. If no Origin header is present (non-browser clients), the request
// is allowed.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients (curl, CLI tools) don't send Origin
	}
	// Parse origin to extract host.
	// Origin format: scheme "://" host [ ":" port ]
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// wsClient represents a single WebSocket connection with optional filters.
type wsClient struct {
	conn       *websocket.Conn
	taskID     string // filter: only events for this task
	pipelineID string // filter: only events for this pipeline
	send       chan []byte
	hub        *wsHub
	closeOnce  sync.Once
}

// wsHub manages all active WebSocket clients and fans out events from the
// broker's EventBus.
type wsHub struct {
	mu        sync.RWMutex
	clients   map[*wsClient]struct{}
	sub       *broker.Subscription
	logger    *slog.Logger
	done      chan struct{}
	closeOnce sync.Once
}

func newWSHub(bus *broker.EventBus, logger *slog.Logger) *wsHub {
	h := &wsHub{
		clients: make(map[*wsClient]struct{}),
		sub:     bus.Subscribe(256),
		logger:  logger,
		done:    make(chan struct{}),
	}
	go h.run()
	return h
}

// run reads events from the EventBus subscription and fans them out to
// matching clients.
func (h *wsHub) run() {
	defer h.closeOnce.Do(func() { close(h.done) })
	for ev := range h.sub.C {
		data, err := json.Marshal(ev)
		if err != nil {
			h.logger.Error("marshal event failed", "error", err)
			continue
		}

		h.mu.RLock()
		for c := range h.clients {
			if !c.matches(ev) {
				continue
			}
			select {
			case c.send <- data:
			default:
				// Client too slow — close it.
				go c.close()
			}
		}
		h.mu.RUnlock()
	}
}

// stopped reports whether the hub's event loop has exited.
func (h *wsHub) stopped() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// register adds a new client. Returns false when the hub is shut
// down or at the max-clients cap; callers MUST check the return
// value and reject the connection rather than leak the socket.
// A successful registration counts against maxWSClients until the
// caller's writePump / readPump closes and unregister runs.
func (h *wsHub) register(c *wsClient) bool {
	if h.stopped() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxWSClients {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

// unregister removes a client. Safe to call after shutdown.
func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// shutdown sends close frames to all clients and waits for the event loop
// to drain.
func (h *wsHub) shutdown() {
	h.sub.Unsubscribe()
	// Unsubscribe closes the subscription channel, causing run() to exit
	// its range loop and close h.done via closeOnce.
	<-h.done

	h.mu.Lock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		c.close()
	}
}

// matches returns true if the event passes the client's filters.
func (c *wsClient) matches(ev broker.TaskEvent) bool {
	if c.taskID != "" && ev.TaskID != c.taskID {
		return false
	}
	if c.pipelineID != "" && ev.PipelineID != c.pipelineID {
		return false
	}
	return true
}

// close sends a close frame and cleans up the client.
func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		c.hub.unregister(c)
		c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "server shutting down"),
			time.Now().Add(time.Second),
		)
		close(c.send)
		c.conn.Close()
	})
}

// writePump pumps messages from the send channel to the WebSocket
// connection. Every write (data and ping) is bounded by wsWriteWait
// so a wedged client cannot stall this goroutine indefinitely. A
// periodic ping keeps the connection warm and tickles the peer's
// stack so stale TCP connections are detected within wsPongWait.
func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				// Hub closed the channel — send a graceful close
				// frame and bail. WriteControl handles the deadline
				// itself so SetWriteDeadline is unnecessary here.
				_ = c.conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stream closed"),
					time.Now().Add(wsWriteWait),
				)
				return
			}
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump reads from the WebSocket to detect client disconnect and
// refresh the read deadline on every pong. A client that stops
// responding to pings has its read deadline expire within wsPongWait
// and this goroutine returns, triggering close().
func (c *wsClient) readPump() {
	defer c.close()
	c.conn.SetReadLimit(512)
	// Prime the read deadline so a stalled first-message client is
	// still cleaned up within the pong window.
	_ = c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// handleStream upgrades to WebSocket and registers a client.
//
// Refuses the connection when the hub is at the maxWSClients cap —
// sending a CloseTryAgainLater frame so well-behaved clients back
// off and retry rather than reconnecting in a tight loop. Every
// accepted connection operates under the ping/pong deadline regime
// in writePump / readPump so stale TCP sessions are detected and
// cleaned up within wsPongWait.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("websocket upgrade failed", "error", err)
		return
	}

	q := r.URL.Query()
	c := &wsClient{
		conn:       conn,
		taskID:     q.Get("task_id"),
		pipelineID: q.Get("pipeline_id"),
		send:       make(chan []byte, 64),
		hub:        s.hub,
	}

	if !s.hub.register(c) {
		s.logger.Warn("refusing websocket connection: at max clients or hub stopped",
			"max_clients", maxWSClients,
		)
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many clients"),
			time.Now().Add(wsWriteWait),
		)
		_ = conn.Close()
		return
	}
	go c.writePump()
	go c.readPump()
}
