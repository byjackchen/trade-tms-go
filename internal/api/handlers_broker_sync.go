package api

// handlers_broker_sync.go is the broker-SYNC surface (DIRECTION 2: broker -> TMS).
// It is READ-ONLY at the broker (only Trd_Get* reads; it places NO orders) and is
// safe in ALL session modes incl signal. TMS no longer offers an order ticket — the
// operator places orders at the broker directly; this surface only pulls the
// externally-placed state back into TMS under the EXTERNAL book and reconciles it vs
// the strategy books.
//
//	POST /api/v1/trade/sync   — pull + reflect + reconcile (READ-ONLY at the broker)
//	GET  /api/v1/trade/status — desk-availability probe (503 when no session)

import (
	"context"
	"errors"
	"net/http"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
)

// BrokerSync is the broker-sync surface the API drives (satisfied by
// *livetrade.BrokerSyncController). Optional: when nil the /api/v1/trade/sync and
// /trade/status endpoints return 503 (no broker-connected session). It exposes ONLY
// the READ-ONLY sync + a liveness probe — there is no order-entry path.
type BrokerSync interface {
	// SyncFromBroker is DIRECTION 2: pull the account's ACTUAL state from the broker
	// (READ-ONLY — places NO orders) and reflect it into TMS under the EXTERNAL book,
	// then reconcile vs the strategy books. Safe in ALL modes incl signal.
	SyncFromBroker(ctx context.Context, operator string) (livetrade.SyncReport, error)
	// IsLive reports whether the desk is bound to a real account (for messaging).
	IsLive() bool
}

// handleTradeSync serves POST /api/v1/trade/sync — DIRECTION 2 (broker -> TMS). It
// pulls the account's ACTUAL state from the broker (Trd_GetPositionList +
// Trd_GetOrderList + Trd_GetOrderFillList + Trd_GetFunds) and REFLECTS it into TMS
// under the EXTERNAL book, then reconciles vs the strategy books. It is READ-ONLY at
// the broker (places NO orders) and audited — safe in ALL modes incl signal. It is
// idempotent (re-syncing the same broker state reflects nothing). Returns the sync
// report (positions/orders/fills counts + reconciliation result). 503 when no
// broker-connected session is present.
func (s *Server) handleTradeSync(w http.ResponseWriter, r *http.Request) {
	if s.brokerSync == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"broker sync not connected (start a paper/live session to sync)")
		return
	}
	rep, err := s.brokerSync.SyncFromBroker(r.Context(), actorFromRequest(r))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidArgument) {
			writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
			return
		}
		internalError(w, s.log, "broker sync", err)
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

// handleTradeStatus serves GET /api/v1/trade/status — the broker-sync availability
// probe. It returns 503 when no broker-connected session is present and 200
// {connected:true, mode, live} when one is. The e2e skip-guard reads THIS (not the
// always-present GET /trade/account live-account reader, which returns 200 even with
// no session and so cannot distinguish a connected session from an absent one).
func (s *Server) handleTradeStatus(w http.ResponseWriter, _ *http.Request) {
	if s.brokerSync == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"broker sync not connected (start a paper/live session)")
		return
	}
	mode := "paper"
	if s.brokerSync.IsLive() {
		mode = "live"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"mode":      mode,
		"live":      s.brokerSync.IsLive(),
	})
}
