package api

// handlers_manual_trade.go is the MANUAL (operator-driven) trade-mutation surface
// — the ONLY write-to-the-broker path in the HTTP API. The strategy-driven trade
// surface stays out of the API (orders come from strategies + the flatten command);
// this adds a tightly-gated desk for discretionary operator orders:
//
//	POST /api/v1/trade/order                  — place (live needs confirm_token)
//	POST /api/v1/trade/order/{coid}/cancel    — cancel a working order
//	POST /api/v1/trade/position/{symbol}/close — close a symbol (confirm-gated live)
//	GET  /api/v1/trade/account                 — reuse the live account view
//
// SAFETY: a LIVE place/close without the per-order confirm phrase returns 412
// (confirmation_required) and NO order is placed; a risk-gate violation without an
// override returns 422 (risk_violation). The desk itself enforces the 4-factor
// live activation (via its live-bound executor) + the per-order confirm + the risk
// gate; this layer is the thin transport that maps the desk's sentinel errors to
// precise status codes and audits via the desk.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	moo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
)

// ManualTrader is the operator-driven trade desk the API drives (satisfied by
// *livetrade.ManualController). Optional: when nil the /api/v1/trade/* mutation
// endpoints return 503 (no manual session connected). The desk owns every safety
// gate (4-factor live activation, per-order confirm, risk gate, audit); this
// interface is the narrow transport seam.
type ManualTrader interface {
	// PlaceManualOrder places an operator order. It returns the sentinel errors
	// livetrade.Err{ConfirmationRequired,TradePasswordRequired,RiskViolation} the
	// handler maps to 412/422; domain.ErrInvalidArgument maps to 400.
	PlaceManualOrder(ctx context.Context, req livetrade.ManualOrderRequest) (livetrade.ManualOrderResult, error)
	// CancelManualOrder cancels a working order by client-order-id (idempotent).
	CancelManualOrder(ctx context.Context, operator, clientOrderID string) error
	// CloseManualPosition closes a symbol's manual position (qty<=0 = full close).
	// idempotencyKey makes the close client-order-id caller-deterministic (a retry /
	// double-click dedupes); empty derives an idempotent key from (symbol, net).
	CloseManualPosition(ctx context.Context, operator, symbol string, qty domain.Qty, confirm string, idempotencyKey string) (livetrade.ManualOrderResult, error)
	// SyncFromBroker is DIRECTION 2: pull the account's ACTUAL state from the broker
	// (READ-ONLY — places NO orders) and reflect it into TMS under the MANUAL book,
	// then reconcile vs the strategy books. Safe in ALL modes incl signal.
	SyncFromBroker(ctx context.Context, operator string) (livetrade.SyncReport, error)
	// IsLive reports whether the desk is bound to a real account (for messaging).
	IsLive() bool
}

// placeOrderBody is the POST /api/v1/trade/order request.
type placeOrderBody struct {
	IdempotencyKey string  `json:"idempotency_key"`
	Symbol         string  `json:"symbol"`
	Side           string  `json:"side"` // BUY | SELL
	Qty            int64   `json:"qty"`
	Type           string  `json:"type,omitempty"` // MARKET (default) | LIMIT
	LimitPrice     float64 `json:"limit_price,omitempty"`
	Override       bool    `json:"override,omitempty"`
	// ConfirmToken is the per-order gate: the live confirmation phrase for a LIVE
	// desk, or the trade password for a PAPER desk.
	ConfirmToken string `json:"confirm_token,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// closePositionBody is the POST /api/v1/trade/position/{symbol}/close request.
type closePositionBody struct {
	Qty          int64  `json:"qty,omitempty"` // 0 / omitted => full close
	ConfirmToken string `json:"confirm_token,omitempty"`
	// IdempotencyKey makes the close client-order-id caller-deterministic so a
	// double-click / client retry on a slow request never double-submits (oversell).
	// Optional: empty derives an idempotent key from (symbol, current net).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// handleTradeOrder serves POST /api/v1/trade/order (place a manual order).
//
// Status codes: 200 (placed, returns client_order_id) / 400 (bad request) / 412
// (confirmation_required — live order missing/wrong confirm) / 422 (risk_violation
// — gate rejected without override) / 503 (no manual desk connected).
func (s *Server) handleTradeOrder(w http.ResponseWriter, r *http.Request) {
	if s.manual == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"manual trade desk not connected (start a paper/live manual session)")
		return
	}
	var body placeOrderBody
	// Decode tolerantly (unknown fields ignored): the desk is bound paper/live at
	// CONNECT, so request-level routing hints (e.g. mode/live) are NOT honored — the
	// desk's binding alone determines the account. Tolerating extra fields keeps this
	// path consistent with the live-node manual listener (finding 8) and forward-
	// compatible; the safety gate is the desk's confirm/risk check, not strict JSON.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid order body: "+err.Error())
		return
	}
	side := domain.OrderSide(strings.ToUpper(strings.TrimSpace(body.Side)))
	if !side.IsValid() {
		writeError(w, http.StatusBadRequest, CodeValidation, "side must be BUY or SELL")
		return
	}
	otype := domain.OrderType(strings.ToUpper(strings.TrimSpace(body.Type)))
	if otype == "" {
		otype = domain.OrderTypeMarket
	}
	if otype != domain.OrderTypeMarket && otype != domain.OrderTypeLimit {
		writeError(w, http.StatusBadRequest, CodeValidation, "type must be MARKET or LIMIT")
		return
	}
	var limitPx domain.Price
	if otype == domain.OrderTypeLimit {
		px, err := domain.PriceFromFloat64(body.LimitPrice)
		if err != nil || px <= 0 {
			writeError(w, http.StatusBadRequest, CodeValidation, "LIMIT order requires a positive limit_price")
			return
		}
		limitPx = px
	}

	res, err := s.manual.PlaceManualOrder(r.Context(), livetrade.ManualOrderRequest{
		Operator:       actorFromRequest(r),
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
	if s.writeManualErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_order_id": res.ClientOrderID,
		"submitted":       res.Submitted,
		"status":          "submitted",
	})
}

// handleTradeCancel serves POST /api/v1/trade/order/{coid}/cancel.
func (s *Server) handleTradeCancel(w http.ResponseWriter, r *http.Request) {
	if s.manual == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manual trade desk not connected")
		return
	}
	coid := strings.TrimSpace(chi.URLParam(r, "coid"))
	if coid == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "missing client_order_id")
		return
	}
	if err := s.manual.CancelManualOrder(r.Context(), actorFromRequest(r), coid); err != nil {
		if errors.Is(err, domain.ErrInvalidArgument) {
			writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
			return
		}
		if errors.Is(err, moo.ErrUnsupported) {
			// A wire client that cannot cancel: surface as 501 (not implemented) so
			// the operator is NOT told the order was cancelled.
			writeError(w, http.StatusNotImplemented, "cancel_unsupported", err.Error())
			return
		}
		internalError(w, s.log, "manual cancel", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client_order_id": coid, "status": "cancel_requested"})
}

// handleTradeClose serves POST /api/v1/trade/position/{symbol}/close.
func (s *Server) handleTradeClose(w http.ResponseWriter, r *http.Request) {
	if s.manual == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manual trade desk not connected")
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "symbol")))
	if symbol == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "missing symbol")
		return
	}
	var body closePositionBody
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_body", "invalid close body: "+err.Error())
			return
		}
	}
	res, err := s.manual.CloseManualPosition(r.Context(), actorFromRequest(r), symbol, domain.Qty(body.Qty), body.ConfirmToken, strings.TrimSpace(body.IdempotencyKey))
	if s.writeManualErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_order_id": res.ClientOrderID,
		"submitted":       res.Submitted,
		"symbol":          symbol,
		"status":          "close_submitted",
	})
}

// handleTradeSync serves POST /api/v1/trade/sync — DIRECTION 2 (broker -> TMS). It
// pulls the account's ACTUAL state from the broker (Trd_GetPositionList +
// Trd_GetOrderList + Trd_GetOrderFillList + Trd_GetFunds) and REFLECTS it into TMS
// under the MANUAL book, then reconciles vs the strategy books. It is READ-ONLY at
// the broker (places NO orders) and audited — safe in ALL modes incl signal. It is
// idempotent (re-syncing the same broker state reflects nothing). Returns the sync
// report (positions/orders/fills counts + reconciliation result). 503 when no manual
// desk is connected.
func (s *Server) handleTradeSync(w http.ResponseWriter, r *http.Request) {
	if s.manual == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"manual trade desk not connected (start a paper/live manual session to sync)")
		return
	}
	rep, err := s.manual.SyncFromBroker(r.Context(), actorFromRequest(r))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidArgument) {
			writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
			return
		}
		internalError(w, s.log, "manual sync", err)
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
			"matched":    len(rep.Reconciliation.Matched),
			"mismatches": len(rep.Reconciliation.Mismatches),
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// handleTradeStatus serves GET /api/v1/trade/status — the desk availability probe
// on the ACTUAL mutation surface. It returns 503 when no desk is connected and 200
// {connected:true, mode, live} when one is. The e2e skip-guard reads THIS (not the
// always-present GET /trade/account live-account reader, which returns 200 even with
// no desk and so cannot distinguish a connected desk from an absent one — finding 1).
func (s *Server) handleTradeStatus(w http.ResponseWriter, _ *http.Request) {
	if s.manual == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"manual trade desk not connected (start a paper/live manual session)")
		return
	}
	mode := "paper"
	if s.manual.IsLive() {
		mode = "live"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"mode":      mode,
		"live":      s.manual.IsLive(),
	})
}

// writeManualErr maps the desk's sentinel errors to status codes. It returns true
// when it wrote an error response (the caller should stop).
func (s *Server) writeManualErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, livetrade.ErrConfirmationRequired):
		writeError(w, http.StatusPreconditionFailed, "confirmation_required",
			"this live (real-money) order requires the exact per-order confirm_token")
		return true
	case errors.Is(err, livetrade.ErrTradePasswordRequired):
		writeError(w, http.StatusPreconditionFailed, "confirmation_required",
			"this paper order requires the trade password as confirm_token")
		return true
	case errors.Is(err, livetrade.ErrRiskViolation):
		writeError(w, http.StatusUnprocessableEntity, "risk_violation", err.Error())
		return true
	case errors.Is(err, moo.ErrOrderRejected):
		// A BROKER/VENUE business rejection (insufficient buying power, market closed,
		// unknown symbol): an EXPECTED operator outcome, not an internal fault. Surface
		// a clean 422 with the venue's reason — never a 500 with a leaked protocol
		// string (finding 4). No order was placed.
		writeError(w, http.StatusUnprocessableEntity, "order_rejected", err.Error())
		return true
	case errors.Is(err, domain.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return true
	default:
		internalError(w, s.log, "manual order", err)
		return true
	}
}
