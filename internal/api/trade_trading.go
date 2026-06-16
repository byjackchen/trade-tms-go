package api

// trade_trading.go adds the paper/live trading read surface (P6 task 6): orders,
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

// TradeTradingReader is the paper/live trading read surface (PG-backed; satisfied
// by *apistore.TradeStore). It extends the signal-mode TradeReader.
type TradeTradingReader interface {
	// RecentOrders returns up to limit newest orders, optionally filtered by symbol.
	RecentOrders(ctx context.Context, symbol string, limit int) ([]TradeOrder, error)
	// RecentOrdersFor is RecentOrders with an optional account filter (accountID
	// "" = all accounts).
	RecentOrdersFor(ctx context.Context, symbol, accountID string, limit int) ([]TradeOrder, error)
	// RecentFills returns up to limit newest fills, optionally filtered by symbol.
	RecentFills(ctx context.Context, symbol string, limit int) ([]TradeFill, error)
	// RecentFillsFor is RecentFills with an optional account filter (accountID
	// "" = all accounts).
	RecentFillsFor(ctx context.Context, symbol, accountID string, limit int) ([]TradeFill, error)
	// OpenPositions returns the live position book (non-flat positions).
	OpenPositions(ctx context.Context) ([]TradePosition, error)
	// OpenPositionsFor is OpenPositions with an optional account filter
	// (accountID "" = all accounts).
	OpenPositionsFor(ctx context.Context, accountID string) ([]TradePosition, error)
	// SessionRealizedPnL returns Σ realized PnL over the FULL position book
	// (open AND closed). Day P/L must include realized gains/losses from
	// positions closed intraday (e.g. a rebalance dropping a sector), which
	// OpenPositions excludes.
	SessionRealizedPnL(ctx context.Context) (float64, error)
	// LatestReconciliation returns the newest reconciliation report, or nil.
	LatestReconciliation(ctx context.Context) (*TradeReconciliation, error)
}

// TradeOrder is the wire shape of one order.
type TradeOrder struct {
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

// TradeFill is the wire shape of one execution.
type TradeFill struct {
	TradeID    string    `json:"trade_id"`
	Symbol     string    `json:"symbol"`
	Qty        int64     `json:"qty"`
	Price      float64   `json:"price"`
	Commission float64   `json:"commission"`
	TS         time.Time `json:"ts"`
}

// TradePosition is the wire shape of one open position.
type TradePosition struct {
	StrategyID  string  `json:"strategy_id"`
	Symbol      string  `json:"symbol"`
	SignedQty   int64   `json:"signed_qty"`
	AvgEntryPx  float64 `json:"avg_entry_px"`
	RealizedPnL float64 `json:"realized_pnl"`
	Status      string  `json:"status"`
}

// TradeAccount is the wire shape of the account/buying-power + day-PnL snapshot.
type TradeAccount struct {
	TotalAssets    float64   `json:"total_assets"`
	Cash           float64   `json:"cash"`
	AvailableFunds float64   `json:"available_funds"` // buying power
	MarketValue    float64   `json:"market_value"`
	DayPnL         float64   `json:"day_pnl"`
	TS             time.Time `json:"ts"`
}

// TradeReconciliation is the wire shape of the latest reconciliation report.
type TradeReconciliation struct {
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

// handleTradeOrders serves GET /api/v1/trade/orders?symbol=&limit=.
func (s *Server) handleTradeOrders(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.tradeTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade trading reader not configured")
		return
	}
	symbol := queryStr(r, "symbol")
	accountID := queryStr(r, "account_id")
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	out, err := reader.RecentOrdersFor(r.Context(), symbol, accountID, limit)
	if err != nil {
		internalError(w, s.log, "trade orders", err)
		return
	}
	if out == nil {
		out = []TradeOrder{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// handleTradeFills serves GET /api/v1/trade/fills?symbol=&limit=.
func (s *Server) handleTradeFills(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.tradeTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade trading reader not configured")
		return
	}
	symbol := queryStr(r, "symbol")
	accountID := queryStr(r, "account_id")
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	out, err := reader.RecentFillsFor(r.Context(), symbol, accountID, limit)
	if err != nil {
		internalError(w, s.log, "trade fills", err)
		return
	}
	if out == nil {
		out = []TradeFill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"fills": out})
}

// handleTradePositions serves GET /api/v1/trade/positions.
func (s *Server) handleTradePositions(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.tradeTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade trading reader not configured")
		return
	}
	accountID := queryStr(r, "account_id")
	out, err := reader.OpenPositionsFor(r.Context(), accountID)
	if err != nil {
		internalError(w, s.log, "trade positions", err)
		return
	}
	if out == nil {
		out = []TradePosition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"positions": out})
}

// handleTradeAccount serves GET /api/v1/trade/account: account/buying-power +
// day-PnL, derived from the position book + the session's starting NAV.
func (s *Server) handleTradeAccount(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.tradeTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade trading reader not configured")
		return
	}
	positions, err := reader.OpenPositions(r.Context())
	if err != nil {
		internalError(w, s.log, "trade account", err)
		return
	}
	// Day P&L = Σ realized over the FULL book (open + intraday-closed), so a
	// rebalance that closes a position still books its realized P&L. Market value
	// is over open positions only (closed positions have no mark); the cockpit
	// follows the Redis account stream for live marks.
	dayPnL, err := reader.SessionRealizedPnL(r.Context())
	if err != nil {
		internalError(w, s.log, "trade account", err)
		return
	}
	var marketValue float64
	for _, p := range positions {
		marketValue += float64(p.SignedQty) * p.AvgEntryPx
	}
	writeJSON(w, http.StatusOK, TradeAccount{
		MarketValue: marketValue,
		DayPnL:      dayPnL,
		TS:          time.Now().UTC(),
	})
}

// handleTradeReconciliation serves GET /api/v1/trade/reconciliation: the latest
// reconciliation report (broker vs strategy books).
func (s *Server) handleTradeReconciliation(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.tradeTrading()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade trading reader not configured")
		return
	}
	rep, err := reader.LatestReconciliation(r.Context())
	if err != nil {
		internalError(w, s.log, "trade reconciliation", err)
		return
	}
	if rep == nil {
		writeJSON(w, http.StatusOK, map[string]any{"reconciliation": nil})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// tradeTrading returns the trading reader (the TradeStore, when it implements the
// trading surface). Signal-mode-only deployments still expose the signal reads.
func (s *Server) tradeTrading() (TradeTradingReader, bool) {
	if s.trade == nil {
		return nil, false
	}
	tr, ok := s.trade.(TradeTradingReader)
	return tr, ok
}

func queryStr(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}
