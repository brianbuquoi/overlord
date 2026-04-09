package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/orcastrator/orcastrator/internal/broker"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     checkOrigin,
}

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

// register adds a new client. It is a no-op if the hub is shut down.
func (h *wsHub) register(c *wsClient) {
	if h.stopped() {
		return
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
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

// writePump pumps messages from the send channel to the WebSocket connection.
func (c *wsClient) writePump() {
	defer c.close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

// readPump reads from the WebSocket to detect client disconnect.
func (c *wsClient) readPump() {
	defer c.close()
	c.conn.SetReadLimit(512)
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// handleStream upgrades to WebSocket and registers a client.
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

	s.hub.register(c)
	go c.writePump()
	go c.readPump()
}
