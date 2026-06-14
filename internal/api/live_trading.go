package api

// live_trading.go adds the paper/live trading read surface (P6 task 6): orders,
// fills, positions, account/buying-power + day-PnL, and the latest reconciliation
// report. All reads are from PG (the durable system-of-record, decision 5); the
// cockpit follows the Redis streams live and reconstructs from these on connect.
// READ-ONLY: the trading mutation surface stays out of the HTTP API (commands
// are the audited side channel).

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// LiveTradingReader is the paper/live trading read surface (PG-backed; satisfied
// by *LiveStore). It extends the signal-mode LiveReader.
type LiveTradingReader interface {
	// RecentOrders returns up to limit newest orders, optionally filtered by symbol.
	RecentOrders(ctx context.Context, symbol string, limit int) ([]LiveOrder, error)
	// RecentFills returns up to limit newest fills, optionally filtered by symbol.
	RecentFills(ctx context.Context, symbol string, limit int) ([]LiveFill, error)
	// OpenPositions returns the live position book (non-flat positions).
	OpenPositions(ctx context.Context) ([]LiveTradePosition, error)
	// SessionRealizedPnL returns Σ realized PnL over the FULL position book
	// (open AND closed). Day P/L must include realized gains/losses from
	// positions closed intraday (e.g. a rebalance dropping a sector), which
	// OpenPositions excludes.
	SessionRealizedPnL(ctx context.Context) (float64, error)
	// LatestReconciliation returns the newest reconciliation report, or nil.
	LatestReconciliation(ctx context.Context) (*LiveReconciliation, error)
}

// LiveOrder is the wire shape of one order.
type LiveOrder struct {
	ClientOrderID string    `json:"client_order_id"`
	VenueOrderID  string    `json:"venue_order_id,omitempty"`
	StrategyID    string    `json:"strategy_id"`
	Symbol        string    `json:"symbol"`
	Side          string    `json:"side"`
	Qty           int64     `json:"qty"`
	FilledQty     int64     `json:"filled_qty"`
	AvgFillPx     float64   `json:"avg_fill_px"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	TS            time.Time `json:"ts"`
}

// LiveFill is the wire shape of one execution.
type LiveFill struct {
	TradeID    string    `json:"trade_id"`
	Symbol     string    `json:"symbol"`
	Qty        int64     `json:"qty"`
	Price      float64   `json:"price"`
	Commission float64   `json:"commission"`
	TS         time.Time `json:"ts"`
}

// LiveTradePosition is the wire shape of one open position.
type LiveTradePosition struct {
	StrategyID  string  `json:"strategy_id"`
	Symbol      string  `json:"symbol"`
	SignedQty   int64   `json:"signed_qty"`
	AvgEntryPx  float64 `json:"avg_entry_px"`
	RealizedPnL float64 `json:"realized_pnl"`
	Status      string  `json:"status"`
}

// LiveAccount is the wire shape of the account/buying-power + day-PnL snapshot.
type LiveAccount struct {
	TotalAssets    float64   `json:"total_assets"`
	Cash           float64   `json:"cash"`
	AvailableFunds float64   `json:"available_funds"` // buying power
	MarketValue    float64   `json:"market_value"`
	DayPnL         float64   `json:"day_pnl"`
	TS             time.Time `json:"ts"`
}

// LiveReconciliation is the wire shape of the latest reconciliation report.
type LiveReconciliation struct {
	TS                      time.Time       `json:"ts"`
	HasIssues               bool            `json:"has_issues"`
	ToleranceShares         int64           `json:"tolerance_shares"`
	Matched                 []string        `json:"matched"`
	Mismatches              []ReconMismatch `json:"mismatches"`
	SymbolsOnlyInStrategies []string        `json:"symbols_only_in_strategies"`
	SymbolsOnlyAtBroker     []string        `json:"symbols_only_at_broker"`
}

// ReconMismatch is one symbol drift.
type ReconMismatch struct {
	Symbol           string `json:"symbol"`
	StrategyBooksSum int64  `json:"strategy_books_sum"`
	BrokerNet        int64  `json:"broker_net"`
	Diff             int64  `json:"diff"`
}

// handleLiveOrders serves GET /api/v1/live/orders?symbol=&limit=.
func (s *Server) handleLiveOrders(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.liveTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live trading reader not configured")
		return
	}
	symbol := queryStr(r, "symbol")
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	out, err := reader.RecentOrders(r.Context(), symbol, limit)
	if err != nil {
		internalError(w, s.log, "live orders", err)
		return
	}
	if out == nil {
		out = []LiveOrder{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// handleLiveFills serves GET /api/v1/live/fills?symbol=&limit=.
func (s *Server) handleLiveFills(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.liveTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live trading reader not configured")
		return
	}
	symbol := queryStr(r, "symbol")
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	out, err := reader.RecentFills(r.Context(), symbol, limit)
	if err != nil {
		internalError(w, s.log, "live fills", err)
		return
	}
	if out == nil {
		out = []LiveFill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"fills": out})
}

// handleLivePositions serves GET /api/v1/live/positions.
func (s *Server) handleLivePositions(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.liveTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live trading reader not configured")
		return
	}
	out, err := reader.OpenPositions(r.Context())
	if err != nil {
		internalError(w, s.log, "live positions", err)
		return
	}
	if out == nil {
		out = []LiveTradePosition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"positions": out})
}

// handleLiveAccount serves GET /api/v1/live/account: account/buying-power +
// day-PnL, derived from the position book + the session's starting NAV.
func (s *Server) handleLiveAccount(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.liveTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live trading reader not configured")
		return
	}
	positions, err := reader.OpenPositions(r.Context())
	if err != nil {
		internalError(w, s.log, "live account", err)
		return
	}
	// Day P&L = Σ realized over the FULL book (open + intraday-closed), so a
	// rebalance that closes a position still books its realized P&L. Market value
	// is over open positions only (closed positions have no mark); the cockpit
	// follows the Redis account stream for live marks.
	dayPnL, err := reader.SessionRealizedPnL(r.Context())
	if err != nil {
		internalError(w, s.log, "live account", err)
		return
	}
	var marketValue float64
	for _, p := range positions {
		marketValue += float64(p.SignedQty) * p.AvgEntryPx
	}
	writeJSON(w, http.StatusOK, LiveAccount{
		MarketValue: marketValue,
		DayPnL:      dayPnL,
		TS:          time.Now().UTC(),
	})
}

// handleLiveReconciliation serves GET /api/v1/live/reconciliation: the latest
// reconciliation report (broker vs strategy books).
func (s *Server) handleLiveReconciliation(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.liveTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live trading reader not configured")
		return
	}
	rep, err := reader.LatestReconciliation(r.Context())
	if err != nil {
		internalError(w, s.log, "live reconciliation", err)
		return
	}
	if rep == nil {
		writeJSON(w, http.StatusOK, map[string]any{"reconciliation": nil})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// liveTrading returns the trading reader (the LiveStore, when it implements the
// trading surface). Signal-mode-only deployments still expose the signal reads.
func (s *Server) liveTrading() (LiveTradingReader, bool) {
	if s.live == nil {
		return nil, false
	}
	tr, ok := s.live.(LiveTradingReader)
	return tr, ok
}

func queryStr(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}
