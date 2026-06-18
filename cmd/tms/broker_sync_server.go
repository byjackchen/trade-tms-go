package main

// broker_sync_server.go serves the READ-ONLY broker-SYNC surface (DIRECTION 2:
// broker -> TMS) from the trade-run node — the process that holds the broker
// connection. There is no separate HTTP listener and no reverse proxy: the sync
// routes are FOLDED onto the trade node's own health server (the same listener
// that serves /healthz on TMS_WORKER_HEALTH_ADDR / host 18090), bearer-guarded by
// TMS_API_TOKEN. They delegate to *livetrade.BrokerSyncController.
//
// TMS no longer offers an order ticket: the operator places orders at the broker
// directly; this surface only pulls the externally-placed state into TMS under the
// EXTERNAL book and reconciles it. It is READ-ONLY at the broker (only Trd_Get*
// reads; it places NO orders) — safe in ALL session modes incl signal.
//
//	POST /api/v1/trade/sync    — pull + reflect + reconcile (READ-ONLY at the broker)
//	GET  /api/v1/trade/status  — sync-availability probe (503 until connected)
//	GET  /api/v1/trade/account — the EXTERNAL book's account snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// brokerSyncHandler serves the bearer-guarded broker-sync routes off the trade
// node's health server. It reads the connected controller live from the node, so
// the routes return 503 until the desk has connected (see connectBrokerSyncDesk).
type brokerSyncHandler struct {
	node  *runner.Live
	log   zerolog.Logger
	token string
}

// registerBrokerSyncRoutes mounts the READ-ONLY sync surface onto an existing mux
// (the trade node's health mux). The routes are bearer-guarded by TMS_API_TOKEN;
// an empty token disables them (a signal-only node with no API token never serves
// the sync surface — but the desk only ever connects in paper/live anyway).
func registerBrokerSyncRoutes(mux *http.ServeMux, node *runner.Live, token string, log zerolog.Logger) {
	if strings.TrimSpace(token) == "" {
		log.Warn().Msg("broker SYNC surface disabled: TMS_API_TOKEN not set (cannot bearer-guard /api/v1/trade/*)")
		return
	}
	h := &brokerSyncHandler{node: node, log: log.With().Str("component", "broker-sync").Logger(), token: token}
	// DIRECTION 2 (broker -> TMS): READ-ONLY pull + reflect + reconcile.
	mux.HandleFunc("POST /api/v1/trade/sync", h.bearer(h.handleSync))
	// Sync-availability probe (the e2e skip-guard reads this): 503 until connected,
	// 200 {connected,mode,live} once it is.
	mux.HandleFunc("GET /api/v1/trade/status", h.bearer(h.handleStatus))
	mux.HandleFunc("GET /api/v1/trade/account", h.bearer(h.handleAccount))
}

// connectBrokerSyncDesk connects the broker-sync controller in the background once
// the moomoo client is ready (the client may not be connected when the node starts),
// retrying on the transient "client not connected yet" error. A hard failure is
// logged once and NOT retried. It runs only for paper/live (signal has no broker
// account to bind); the caller gates on mode.
func connectBrokerSyncDesk(ctx context.Context, node *runner.Live, mode string, log zerolog.Logger) {
	slog := log.With().Str("component", "broker-sync").Logger()
	for {
		if ctx.Err() != nil {
			return
		}
		_, err := node.ConnectBrokerSync(ctx, mode)
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "not connected yet") {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		slog.Error().Err(err).Msg("broker sync connect failed (endpoints stay 503)")
		return
	}
}

func (h *brokerSyncHandler) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtleConstEq(got, h.token) {
			next(w, r)
			return
		}
		writeJSONErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
	}
}

func (h *brokerSyncHandler) desk() *livetrade.BrokerSyncController { return h.node.BrokerSync() }

// handleSync serves POST /api/v1/trade/sync — DIRECTION 2 (broker -> TMS). It pulls
// the account's ACTUAL state from the broker READ-ONLY (no order placed) + reflects
// it into TMS under the EXTERNAL book + reconciles. Safe in ALL modes incl signal.
func (h *brokerSyncHandler) handleSync(w http.ResponseWriter, r *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "broker sync not connected")
		return
	}
	rep, err := desk.SyncFromBroker(r.Context(), "sync-api@"+r.RemoteAddr)
	if writeSyncErr(w, err) {
		return
	}
	body := map[string]any{
		"status":             "synced",
		"positions_observed": rep.PositionsObserved,
		"orders_observed":    rep.OrdersObserved,
		"fills_observed":     rep.FillsObserved,
		"reflected":          rep.Reflected,
		"read_only":          true,
	}
	if rep.HasReconciliation {
		body["reconciliation"] = map[string]any{
			"has_issues": rep.Reconciliation.HasIssues(),
			"summary":    rep.Reconciliation.Summary(),
		}
	}
	writeJSON2(w, http.StatusOK, body)
}

// handleStatus serves GET /api/v1/trade/status — the sync-availability probe (503
// until connected; 200 once it is).
func (h *brokerSyncHandler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "broker sync not connected")
		return
	}
	writeJSON2(w, http.StatusOK, map[string]any{
		"connected": true,
		"mode":      desk.Mode(),
		"live":      desk.IsLive(),
	})
}

// handleAccount serves GET /api/v1/trade/account — the bound mode + a snapshot of
// the EXTERNAL book's account (cash / equity / day P&L). 503 until connected.
func (h *brokerSyncHandler) handleAccount(w http.ResponseWriter, _ *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "broker sync not connected")
		return
	}
	body := map[string]any{"mode": desk.Mode(), "live": desk.IsLive(), "connected": true}
	if snap, ok := desk.AccountSnapshot(); ok {
		body["cash"] = snap.Cash
		body["equity"] = snap.Equity
		body["day_pnl_usd"] = snap.DayPnLUSD
		body["open_positions"] = snap.OpenPositions
	}
	writeJSON2(w, http.StatusOK, body)
}

// writeSyncErr maps the sync sentinels to status codes (400/500).
func writeSyncErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, domain.ErrInvalidArgument):
		writeJSONErr(w, http.StatusBadRequest, "validation", err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
	return true
}

func writeJSON2(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON2(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// subtleConstEq is a constant-time token comparison.
func subtleConstEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
