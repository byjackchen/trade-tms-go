package main

// manual_trade_server.go serves the operator-driven MANUAL trade-mutation surface
// (POST /api/v1/trade/order, .../cancel, .../close) from the live-node process —
// the process that holds the broker connection. It connects the manual desk
// (paper/live) once the moomoo client is ready, then serves bearer-guarded
// endpoints that delegate to *livetrade.ManualController. SAFETY: a live desk
// re-runs the full 4-factor activation at connect; per-order confirm + risk gate
// run inside the controller (412 confirmation_required / 422 risk_violation).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	moo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

type manualTradeServer struct {
	srv *http.Server
}

func manualShutdown(s *manualTradeServer) func(context.Context) error {
	return func(ctx context.Context) error {
		if s == nil {
			return nil
		}
		return s.srv.Shutdown(ctx)
	}
}

type manualTradeServerArgs struct {
	node    *runner.Live
	mode    string
	apiAddr string
	token   string
	log     zerolog.Logger
}

// startManualTradeServer binds the HTTP listener immediately (so the address is
// reserved + the process can be probed) and connects the desk in the background
// once the moomoo client is ready. Requests before the desk is connected return
// 503. A live desk that fails the 4-factor activation logs the refusal and the
// endpoint stays 503 — there is no degraded/partial real-money path.
func startManualTradeServer(ctx context.Context, a manualTradeServerArgs) (*manualTradeServer, error) {
	mode := strings.TrimSpace(a.mode)
	if mode != "paper" && mode != "live" {
		return nil, fmt.Errorf("--manual-mode %q invalid (want paper|live)", mode)
	}
	if a.token == "" {
		return nil, errors.New("manual trade api: TMS_API_TOKEN required to bearer-guard the trade endpoints")
	}
	log := a.log.With().Str("component", "manual-trade-api").Logger()

	ln, err := net.Listen("tcp", a.apiAddr)
	if err != nil {
		return nil, fmt.Errorf("manual trade api: listener on %s: %w", a.apiAddr, err)
	}

	h := &manualHandler{node: a.node, log: log, token: a.token}

	// Connect the desk in the background (the client may not be ready yet).
	go h.connectWithRetry(ctx, mode)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/trade/order", h.bearer(h.handlePlace))
	mux.HandleFunc("POST /api/v1/trade/order/{coid}/cancel", h.bearer(h.handleCancel))
	mux.HandleFunc("POST /api/v1/trade/position/{symbol}/close", h.bearer(h.handleClose))
	mux.HandleFunc("POST /api/v1/trade/sync", h.bearer(h.handleSync))
	// Desk availability probe on the ACTUAL mutation surface (the e2e skip-guard
	// reads this, NOT a generic account reader): 503 until the desk is connected,
	// 200 {connected,mode,live} once it is (finding 1). GET /trade/account mirrors
	// the desk's account view so the UI/e2e can read the manual book over one host.
	mux.HandleFunc("GET /api/v1/trade/status", h.bearer(h.handleStatus))
	mux.HandleFunc("GET /api/v1/trade/account", h.bearer(h.handleAccount))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("manual trade api stopped unexpectedly")
		}
	}()
	log.Warn().Str("addr", ln.Addr().String()).Str("mode", mode).
		Msg("MANUAL trade API listening (bearer-guarded)")
	return &manualTradeServer{srv: srv}, nil
}

type manualHandler struct {
	node  *runner.Live
	log   zerolog.Logger
	token string
}

// connectWithRetry connects the manual desk once the moomoo client is ready,
// retrying on the transient "client not connected yet" error. A hard activation
// failure (e.g. live gate not satisfied) is logged once and NOT retried (the gate
// is deterministic — retrying cannot help and must never silently succeed later).
func (h *manualHandler) connectWithRetry(ctx context.Context, mode string) {
	paperPassword := strings.TrimSpace(envOr("TMS_MOOMOO_TRADE_PASSWORD", ""))
	for {
		if ctx.Err() != nil {
			return
		}
		_, err := h.node.ConnectManualSession(ctx, mode, paperPassword)
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
		h.log.Error().Err(err).Msg("manual desk connect failed (endpoints stay 503 — no real-money path without activation)")
		return
	}
}

func (h *manualHandler) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtleConstEq(got, h.token) {
			next(w, r)
			return
		}
		writeJSONErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
	}
}

func (h *manualHandler) desk() *livetrade.ManualController { return h.node.ManualController() }

func (h *manualHandler) handlePlace(w http.ResponseWriter, r *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "manual desk not connected")
		return
	}
	var body struct {
		IdempotencyKey string  `json:"idempotency_key"`
		Symbol         string  `json:"symbol"`
		Side           string  `json:"side"`
		Qty            int64   `json:"qty"`
		Type           string  `json:"type"`
		LimitPrice     float64 `json:"limit_price"`
		Override       bool    `json:"override"`
		ConfirmToken   string  `json:"confirm_token"`
		Reason         string  `json:"reason"`
	}
	// Decode tolerantly (unknown fields ignored): the desk is bound paper/live at
	// CONNECT, so request-level routing hints (e.g. mode/live) are deliberately NOT
	// honored — the desk's binding alone determines the account, and a paper desk
	// refuses anything that is not its trade password regardless of such hints. This
	// matches the chi handler (finding 8: same tolerant decode on both surfaces).
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	side := domain.OrderSide(strings.ToUpper(strings.TrimSpace(body.Side)))
	if !side.IsValid() {
		writeJSONErr(w, http.StatusBadRequest, "validation", "side must be BUY or SELL")
		return
	}
	otype := domain.OrderType(strings.ToUpper(strings.TrimSpace(body.Type)))
	if otype == "" {
		otype = domain.OrderTypeMarket
	}
	var limitPx domain.Price
	if otype == domain.OrderTypeLimit {
		px, perr := domain.PriceFromFloat64(body.LimitPrice)
		if perr != nil {
			writeJSONErr(w, http.StatusBadRequest, "validation", "invalid limit_price")
			return
		}
		limitPx = px
	}
	res, err := desk.PlaceManualOrder(r.Context(), livetrade.ManualOrderRequest{
		Operator:       "manual-api@" + r.RemoteAddr,
		IdempotencyKey: strings.TrimSpace(body.IdempotencyKey),
		Symbol:         strings.ToUpper(strings.TrimSpace(body.Symbol)),
		Side:           side,
		Qty:            domain.Qty(body.Qty),
		Type:           otype,
		LimitPrice:     limitPx,
		Override:       body.Override,
		Confirm:        body.ConfirmToken,
		Reason:         body.Reason,
	})
	if writeManualErr(w, err) {
		return
	}
	writeJSON2(w, http.StatusOK, map[string]any{
		"client_order_id": res.ClientOrderID, "submitted": res.Submitted, "status": "submitted",
	})
}

func (h *manualHandler) handleCancel(w http.ResponseWriter, r *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "manual desk not connected")
		return
	}
	coid := strings.TrimSpace(r.PathValue("coid"))
	if err := desk.CancelManualOrder(r.Context(), "manual-api@"+r.RemoteAddr, coid); err != nil {
		if errors.Is(err, moo.ErrUnsupported) {
			writeJSONErr(w, http.StatusNotImplemented, "cancel_unsupported", err.Error())
			return
		}
		if writeManualErr(w, err) {
			return
		}
	}
	writeJSON2(w, http.StatusOK, map[string]any{"client_order_id": coid, "status": "cancel_requested"})
}

func (h *manualHandler) handleClose(w http.ResponseWriter, r *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "manual desk not connected")
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.PathValue("symbol")))
	var body struct {
		Qty            int64  `json:"qty"`
		ConfirmToken   string `json:"confirm_token"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	res, err := desk.CloseManualPosition(r.Context(), "manual-api@"+r.RemoteAddr, symbol, domain.Qty(body.Qty), body.ConfirmToken, strings.TrimSpace(body.IdempotencyKey))
	if writeManualErr(w, err) {
		return
	}
	writeJSON2(w, http.StatusOK, map[string]any{
		"client_order_id": res.ClientOrderID, "submitted": res.Submitted, "symbol": symbol, "status": "close_submitted",
	})
}

// handleSync serves POST /api/v1/trade/sync — DIRECTION 2 (broker -> TMS). It pulls
// the account's ACTUAL state from the broker READ-ONLY (no order placed) + reflects
// it into TMS under the MANUAL book + reconciles. Safe in ALL modes incl signal.
func (h *manualHandler) handleSync(w http.ResponseWriter, r *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "manual desk not connected")
		return
	}
	rep, err := desk.SyncFromBroker(r.Context(), "manual-api@"+r.RemoteAddr)
	if writeManualErr(w, err) {
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

// handleStatus serves GET /api/v1/trade/status — the desk availability probe on
// the ACTUAL mutation surface (503 until the desk connects; 200 once it does). The
// e2e skip-guard reads THIS, not a generic account reader that is present even with
// no desk (finding 1).
func (h *manualHandler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "manual desk not connected")
		return
	}
	writeJSON2(w, http.StatusOK, map[string]any{
		"connected": true,
		"mode":      desk.Mode(),
		"live":      desk.IsLive(),
	})
}

// handleAccount serves GET /api/v1/trade/account — the desk's bound mode + a
// snapshot of the MANUAL book's account (cash / equity / day P&L). 503 until the
// desk connects. The UI + e2e read `mode` here to positively confirm a PAPER desk.
func (h *manualHandler) handleAccount(w http.ResponseWriter, _ *http.Request) {
	desk := h.desk()
	if desk == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "unavailable", "manual desk not connected")
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

// writeManualErr maps the desk sentinels to status codes (412/422/400/500).
func writeManualErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, livetrade.ErrConfirmationRequired):
		writeJSONErr(w, http.StatusPreconditionFailed, "confirmation_required", "live order requires the per-order confirm_token")
	case errors.Is(err, livetrade.ErrTradePasswordRequired):
		writeJSONErr(w, http.StatusPreconditionFailed, "confirmation_required", "paper order requires the trade password as confirm_token")
	case errors.Is(err, livetrade.ErrRiskViolation):
		writeJSONErr(w, http.StatusUnprocessableEntity, "risk_violation", err.Error())
	case errors.Is(err, moo.ErrOrderRejected):
		// A BROKER/VENUE business rejection (insufficient buying power, market closed,
		// unknown symbol) — an expected operator outcome, mapped to a clean 422 with
		// the venue's reason, never a 500 with a leaked protocol string (finding 4).
		writeJSONErr(w, http.StatusUnprocessableEntity, "order_rejected", err.Error())
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

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
