package api

// ws_test.go covers the WebSocket endpoint and its fan-out hub: the auth gate
// on /api/v1/ws, the hello frame contract, live job/sync broadcast delivery,
// and the slow-consumer eviction policy. It drives a real httptest server so
// the coder/websocket client performs a genuine upgrade handshake.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// wsTestServer spins up a real HTTP server bound to the loopback so the
// WebSocket client can dial it. The Origin allowlist is set to the server's
// own origin (added after Start, since the URL is only known then).
func wsTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	srv, err := NewServer(Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		CORSOrigins: []string{testOrigin},
		Jobs:        newStubJobQueue(),
		Data:        &stubDataStore{},
		Universe:    &stubUniverseReader{},
		Runs:        &stubRunsReader{},
		Calendar:    cal,
		PingPG:      pingOK,
		PingRedis:   pingOK,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	// The hub allowlists by Origin host; permit the test server's host so the
	// dial (which sets Origin to the loopback host) is accepted.
	srv.hub.originPatterns = append(srv.hub.originPatterns, strings.TrimPrefix(hs.URL, "http://"))
	return hs, srv
}

func wsURL(httpURL, token string) string {
	u := strings.Replace(httpURL, "http://", "ws://", 1)
	return u + "/api/v1/ws?token=" + token
}

func TestWS_RequiresAuth(t *testing.T) {
	hs, _ := wsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// No token -> requireAuth rejects before the upgrade.
	_, resp, err := websocket.Dial(ctx, strings.Replace(hs.URL, "http", "ws", 1)+"/api/v1/ws", nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWS_HelloAndBroadcast(t *testing.T) {
	hs, srv := wsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(hs.URL, testToken), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// First frame is the hello envelope advertising the channels.
	hello := readEnvelope(t, ctx, conn)
	assert.Equal(t, WSTypeHello, hello.Type)
	assert.False(t, hello.TS.IsZero())

	// Wait for the connection goroutine to register the client, then publish.
	requireEventually(t, func() bool { return srv.Hub().Clients() == 1 })

	srv.Hub().Broadcast(WSTypeJob, json.RawMessage(`{"job_id":42,"event":"progress"}`))
	jobFrame := readEnvelope(t, ctx, conn)
	assert.Equal(t, WSTypeJob, jobFrame.Type)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(jobFrame.Payload, &payload))
	assert.Equal(t, float64(42), payload["job_id"])

	srv.Hub().Broadcast(WSTypeSync, json.RawMessage(`{"dataset":"SEP"}`))
	syncFrame := readEnvelope(t, ctx, conn)
	assert.Equal(t, WSTypeSync, syncFrame.Type)
}

func TestWS_HubCloseEvictsClients(t *testing.T) {
	hs, srv := wsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(hs.URL, testToken), nil)
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	_ = readEnvelope(t, ctx, conn) // hello

	requireEventually(t, func() bool { return srv.Hub().Clients() == 1 })
	require.NoError(t, srv.Hub().Close(ctx))

	// The server closes the socket; the next read fails.
	_, _, err = conn.Read(ctx)
	assert.Error(t, err)
	requireEventually(t, func() bool { return srv.Hub().Clients() == 0 })
}

func TestHub_BroadcastEvictsSlowConsumer(t *testing.T) {
	hub := NewHub(zerolog.Nop(), nil)
	slow := &wsClient{send: make(chan []byte, 1), drop: make(chan struct{})}
	require.True(t, hub.register(slow))
	assert.Equal(t, 1, hub.Clients())

	// Fill the buffer (1) then overflow: the second broadcast evicts.
	hub.Broadcast(WSTypeJob, json.RawMessage(`{"n":1}`))
	hub.Broadcast(WSTypeJob, json.RawMessage(`{"n":2}`))

	assert.Equal(t, 0, hub.Clients(), "slow consumer must be evicted")
	select {
	case <-slow.drop:
		assert.Contains(t, slow.reason, "slow consumer")
	default:
		t.Fatal("expected slow consumer to be evicted (drop channel closed)")
	}
}

func TestHub_BroadcastEmptyPayloadDefaultsToObject(t *testing.T) {
	hub := NewHub(zerolog.Nop(), nil)
	c := &wsClient{send: make(chan []byte, 1), drop: make(chan struct{})}
	require.True(t, hub.register(c))
	hub.Broadcast(WSTypeSync, nil)
	frame := <-c.send
	var env Envelope
	require.NoError(t, json.Unmarshal(frame, &env))
	assert.JSONEq(t, `{}`, string(env.Payload))
}

func TestHub_RegisterAfterCloseFails(t *testing.T) {
	hub := NewHub(zerolog.Nop(), nil)
	require.NoError(t, hub.Close(context.Background()))
	c := &wsClient{send: make(chan []byte, 1), drop: make(chan struct{})}
	assert.False(t, hub.register(c), "registration must fail once the hub is closed")
}

// readEnvelope reads one text frame and decodes the WS envelope.
func readEnvelope(t *testing.T, ctx context.Context, conn *websocket.Conn) Envelope {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	typ, data, err := conn.Read(rctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	var env Envelope
	require.NoError(t, json.Unmarshal(data, &env))
	return env
}

// requireEventually polls cond up to 2s.
func requireEventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
