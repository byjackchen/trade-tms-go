package api

// ws.go implements GET /api/v1/ws: a fan-out hub that pushes job-queue and
// dataset-sync events (bridged from Redis pub/sub) to every connected
// client. Frame envelope (docs/api.md §WebSocket):
//
//	{"type": "hello"|"job"|"sync", "ts": "<RFC3339 UTC>", "payload": {...}}
//
// "job" payloads are jobs.Event objects from channel "tms:jobs:events";
// "sync" payloads come from channel "tms:data:sync" (reserved for the
// dataset-sync engine; same envelope contract). Delivery is best-effort by
// design — the durable job state lives in PostgreSQL; clients reconcile
// via GET /api/v1/jobs after reconnecting.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// SyncEventsChannel is the Redis pub/sub channel for dataset-sync events,
// bridged to WS frames of type "sync". (Job events use
// jobs.DefaultEventsChannel, bridged as type "job".)
const SyncEventsChannel = "tms:data:sync"

// WS envelope types.
const (
	WSTypeHello = "hello"
	WSTypeJob   = "job"
	WSTypeSync  = "sync"
)

const (
	// wsSendBuffer is the per-client outbound queue; a client that falls
	// this far behind is dropped (slow-consumer policy, documented).
	wsSendBuffer = 256
	// wsWriteTimeout bounds one frame write.
	wsWriteTimeout = 10 * time.Second
)

// Envelope is the WS frame shape.
type Envelope struct {
	Type    string          `json:"type"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// wsClient is one connected browser/CLI client.
type wsClient struct {
	send chan []byte
	// drop closes once the hub has evicted the client (slow consumer or
	// hub shutdown); the connection goroutine then closes the socket.
	drop     chan struct{}
	dropOnce sync.Once
	reason   string
}

func (c *wsClient) evict(reason string) {
	c.dropOnce.Do(func() {
		c.reason = reason
		close(c.drop)
	})
}

// Hub fans envelopes out to connected WS clients. Safe for concurrent use.
type Hub struct {
	log            zerolog.Logger
	originPatterns []string

	mu      sync.Mutex
	clients map[*wsClient]struct{}
	closed  bool
}

// NewHub builds a hub. corsOrigins (full origins like
// "http://localhost:13000") become the WebSocket Origin allowlist.
func NewHub(log zerolog.Logger, corsOrigins []string) *Hub {
	patterns := make([]string, 0, len(corsOrigins))
	for _, o := range corsOrigins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			patterns = append(patterns, u.Host)
		}
	}
	return &Hub{
		log:            log.With().Str("component", "ws-hub").Logger(),
		originPatterns: patterns,
		clients:        make(map[*wsClient]struct{}),
	}
}

// Clients reports the current connection count (metrics/tests).
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Broadcast wraps payload in the envelope and queues it to every client.
// Slow clients are evicted rather than blocking the bridge.
func (h *Hub) Broadcast(typ string, payload json.RawMessage) {
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	frame, err := json.Marshal(Envelope{Type: typ, TS: time.Now().UTC(), Payload: payload})
	if err != nil { // payload is raw JSON; cannot fail in practice
		h.log.Error().Err(err).Str("type", typ).Msg("marshal ws envelope")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- frame:
		default:
			delete(h.clients, c)
			c.evict("slow consumer: outbound queue full")
		}
	}
}

// Close evicts every client (server shutdown). Subsequent registrations
// are refused.
func (h *Hub) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	for c := range h.clients {
		delete(h.clients, c)
		c.evict("server shutting down")
	}
	h.mu.Unlock()
	_ = ctx
	return nil
}

// register adds a client; false when the hub is already closed.
func (h *Hub) register(c *wsClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

func (h *Hub) unregister(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

// handleWS upgrades the request (auth already enforced by requireAuth —
// browsers pass the token as ?token=) and streams envelopes until the
// client disconnects or the hub closes.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.hub.originPatterns,
	})
	if err != nil {
		// Accept already wrote the HTTP error response.
		s.log.Warn().Err(err).Str("remote", r.RemoteAddr).Msg("ws accept failed")
		return
	}

	client := &wsClient{
		send: make(chan []byte, wsSendBuffer),
		drop: make(chan struct{}),
	}
	if !s.hub.register(client) {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	defer s.hub.unregister(client)
	s.log.Info().Str("remote", r.RemoteAddr).Int("clients", s.hub.Clients()).Msg("ws client connected")

	ctx := r.Context()

	// Reader: clients send nothing meaningful; the read loop surfaces
	// disconnects (and services control frames).
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	// Hello frame: confirms the subscription contract to the client.
	hello, _ := json.Marshal(map[string]any{"channels": []string{WSTypeJob, WSTypeSync}})
	if err := writeFrame(ctx, conn, mustEnvelope(WSTypeHello, hello)); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "hello write failed")
		return
	}

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "request context canceled")
			return
		case <-readDone:
			s.log.Info().Str("remote", r.RemoteAddr).Msg("ws client disconnected")
			return
		case <-client.drop:
			s.log.Warn().Str("remote", r.RemoteAddr).Str("reason", client.reason).Msg("ws client evicted")
			code := websocket.StatusGoingAway
			if client.reason != "server shutting down" {
				code = websocket.StatusPolicyViolation
			}
			_ = conn.Close(code, client.reason)
			return
		case frame := <-client.send:
			if err := writeFrame(ctx, conn, frame); err != nil {
				s.log.Info().Err(err).Str("remote", r.RemoteAddr).Msg("ws write failed; dropping client")
				_ = conn.Close(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}
}

func mustEnvelope(typ string, payload json.RawMessage) []byte {
	b, _ := json.Marshal(Envelope{Type: typ, TS: time.Now().UTC(), Payload: payload})
	return b
}

func writeFrame(ctx context.Context, conn *websocket.Conn, frame []byte) error {
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, frame)
}

// ---------------------------------------------------------------------------
// Redis -> Hub bridge
// ---------------------------------------------------------------------------

// RunEventBridge subscribes to the job-events and sync-events channels and
// fans messages into the hub until ctx is canceled. Redis outages are
// retried with a 1 s backoff; the bridge never takes the API down (events
// are best-effort by contract — PostgreSQL holds the durable state).
func RunEventBridge(ctx context.Context, client *redis.Client, hub *Hub, log zerolog.Logger) {
	blog := log.With().Str("component", "ws-bridge").Logger()
	for {
		err := bridgeOnce(ctx, client, hub, blog)
		if ctx.Err() != nil {
			blog.Info().Msg("event bridge stopped")
			return
		}
		blog.Warn().Err(err).Msg("event bridge interrupted; retrying in 1s")
		select {
		case <-ctx.Done():
			blog.Info().Msg("event bridge stopped")
			return
		case <-time.After(time.Second):
		}
	}
}

func bridgeOnce(ctx context.Context, client *redis.Client, hub *Hub, log zerolog.Logger) error {
	pubsub := client.Subscribe(ctx, jobs.DefaultEventsChannel, SyncEventsChannel)
	defer func() { _ = pubsub.Close() }()

	// Fail fast when the subscription itself cannot be established.
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	log.Info().
		Str("jobs_channel", jobs.DefaultEventsChannel).
		Str("sync_channel", SyncEventsChannel).
		Msg("event bridge subscribed")

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return errors.New("api: pubsub channel closed")
			}
			typ := WSTypeJob
			if msg.Channel == SyncEventsChannel {
				typ = WSTypeSync
			}
			payload := json.RawMessage(msg.Payload)
			if !json.Valid(payload) {
				// One bad publish must not sever the stream (spec
				// api-ws-redis.md §4.1 spirit): skip with a warning.
				log.Warn().Str("channel", msg.Channel).Msg("skipping non-JSON event payload")
				continue
			}
			hub.Broadcast(typ, payload)
		}
	}
}
