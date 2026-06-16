package apistore

// live_store.go is the PG-backed api.LiveReader: it reads the live cockpit
// snapshots from the durable tms.* tables (decision 5: PG is truth). The
// PortfolioHealth snapshot is reconstructed from the latest health stream
// mirror; since signal mode persists no health table, the read derives the
// informational snapshot from the latest session config (flat book) — the same
// values the live publisher emits.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/api"
)

// LiveStore reads live cockpit state from PostgreSQL.
type LiveStore struct {
	pool *pgxpool.Pool
}

// NewLiveStore builds a LiveStore over a pool.
func NewLiveStore(pool *pgxpool.Pool) *LiveStore { return &LiveStore{pool: pool} }

// LatestSession returns the most recent session with its active halt (if any).
func (s *LiveStore) LatestSession(ctx context.Context) (*api.LiveSession, error) {
	var (
		sess    api.LiveSession
		ended   *time.Time
		cfgText string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, trader_id, mode, status, started_at, ended_at, config::text
		  FROM tms.sessions
		 ORDER BY started_at DESC, id DESC
		 LIMIT 1`).Scan(&sess.ID, &sess.TraderID, &sess.Mode, &sess.Status,
		&sess.StartedAt, &ended, &cfgText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.EndedAt = ended
	sess.Config = json.RawMessage(cfgText)

	// Active halt (cleared_at IS NULL), most recent.
	var halt api.LiveHalt
	herr := s.pool.QueryRow(ctx, `
		SELECT kind, reason, triggered_at
		  FROM tms.halts
		 WHERE cleared_at IS NULL
		 ORDER BY triggered_at DESC
		 LIMIT 1`).Scan(&halt.Kind, &halt.Reason, &halt.TriggeredAt)
	if herr == nil {
		sess.Halt = &halt
	} else if !errors.Is(herr, pgx.ErrNoRows) {
		return nil, herr
	}
	return &sess, nil
}

// RecentIntents returns up to limit newest signal intents, optionally filtered
// by strategy_id.
func (s *LiveStore) RecentIntents(ctx context.Context, strategyID string, limit int) ([]api.LiveIntent, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		rows pgx.Rows
		err  error
	)
	if strategyID == "" {
		rows, err = s.pool.Query(ctx, `
			SELECT strategy_id, symbol, state, strength, generation, intent::text, ts, ts_event_ns
			  FROM tms.signal_intents
			 ORDER BY ts DESC, id DESC
			 LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT strategy_id, symbol, state, strength, generation, intent::text, ts, ts_event_ns
			  FROM tms.signal_intents
			 WHERE strategy_id = $1
			 ORDER BY ts DESC, id DESC
			 LIMIT $2`, strategyID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []api.LiveIntent
	for rows.Next() {
		var it api.LiveIntent
		var intentText string
		if err := rows.Scan(&it.StrategyID, &it.Symbol, &it.State, &it.Strength,
			&it.Generation, &intentText, &it.TS, &it.TSEventNS); err != nil {
			return nil, err
		}
		it.Intent = json.RawMessage(intentText)
		out = append(out, it)
	}
	return out, rows.Err()
}

// LatestHealth returns the newest informational health snapshot. Signal mode
// holds no positions, so the snapshot is the flat-book starting NAV (day P&L 0,
// no halt — decision 6); it is derived deterministically (no health table), so
// the cockpit health panel always has a value while a session is running.
func (s *LiveStore) LatestHealth(ctx context.Context) (*api.LiveHealth, error) {
	sess, err := s.LatestSession(ctx)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	// Flat-book signal-mode health: zeros + the session start as the as-of ts.
	return &api.LiveHealth{
		DayPnL:           0,
		DayPnLPct:        0,
		DailyLossHalt:    false,
		HaltHeadroomPct:  0,
		ConcentrationPct: 0,
		TS:               sess.StartedAt,
	}, nil
}

// Watchlist returns the distinct symbols the most recent signal batch emitted
// intents for (its tracked universe). Empty when no intents exist yet.
//
// The freshness window is anchored to the DATA FRONTIER (the newest intent ts),
// NOT wall-clock now(): the latest batch of signals always shows even when the
// data is a few days behind the wall clock (weekend / holiday / data-vendor lag
// / clock skew). Anchoring on now() would silently empty the watchlist whenever
// the freshest data is older than the cutoff — the same class of bug fixed for
// the sync freshness logic (frontier-driven, not last-operation-time-driven).
func (s *LiveStore) Watchlist(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH frontier AS (SELECT max(ts) AS f FROM tms.signal_intents)
		SELECT DISTINCT symbol
		  FROM tms.signal_intents, frontier
		 WHERE ts >= frontier.f - interval '2 days'
		 ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

// LatestIntentsBySymbol returns the LATEST intent per symbol within the data
// frontier window (the newest signal batch — anchored to max(ts), not wall-clock;
// see Watchlist), ranked ACTIONABLE-FIRST: states that call for operator attention
// (forming / hold / buy / sell — anything but no_setup/flat) sort ahead of the
// idle no_setup tail, then by strength desc, then symbol. It caps at limit rows.
//
// This powers the watchlist's per-symbol state column for the WHOLE tracked
// universe in ONE query, so every rendered row shows its current signal and the
// handful of actionable names float to the top of a multi-thousand-symbol
// universe — instead of a separate newest-N intents poll that, when the batch
// stamps thousands of same-ts intents, never reliably contains the actionable few.
func (s *LiveStore) LatestIntentsBySymbol(ctx context.Context, limit int) ([]api.LiveIntent, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.pool.Query(ctx, `
		WITH frontier AS (SELECT max(ts) AS f FROM tms.signal_intents),
		latest AS (
			SELECT DISTINCT ON (symbol)
			       strategy_id, symbol, state, strength, generation, intent::text AS itext, ts, ts_event_ns
			  FROM tms.signal_intents, frontier
			 WHERE ts >= frontier.f - interval '2 days'
			 ORDER BY symbol, ts DESC, id DESC
		)
		SELECT strategy_id, symbol, state, strength, generation, itext, ts, ts_event_ns
		  FROM latest
		 ORDER BY (state NOT IN ('no_setup','flat')) DESC, strength DESC NULLS LAST, symbol
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []api.LiveIntent
	for rows.Next() {
		var it api.LiveIntent
		var intentText string
		if err := rows.Scan(&it.StrategyID, &it.Symbol, &it.State, &it.Strength,
			&it.Generation, &intentText, &it.TS, &it.TSEventNS); err != nil {
			return nil, err
		}
		it.Intent = json.RawMessage(intentText)
		out = append(out, it)
	}
	return out, rows.Err()
}

// --- paper/live trading reads (P6 task 6) ---

// fixed4 converts a 1e-4 fixed-point BIGINT to a float64 USD value.
func fixed4(v int64) float64 { return float64(v) / 10000.0 }

// RecentOrders returns up to limit newest orders, optionally filtered by symbol.
func (s *LiveStore) RecentOrders(ctx context.Context, symbol string, limit int) ([]api.LiveOrder, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT client_order_id, COALESCE(venue_order_id,''), strategy_id, symbol, side,
	             qty, filled_qty, COALESCE(avg_fill_px,0), status, COALESCE(reason,''),
	             COALESCE(ts_last_event, created_at)
	        FROM tms.orders`
	var rows pgx.Rows
	var err error
	if symbol == "" {
		rows, err = s.pool.Query(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, q+` WHERE symbol=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, symbol, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.LiveOrder
	for rows.Next() {
		var o api.LiveOrder
		var avgPx, _qty, _filled int64
		if err := rows.Scan(&o.ClientOrderID, &o.VenueOrderID, &o.StrategyID, &o.Symbol, &o.Side,
			&_qty, &_filled, &avgPx, &o.Status, &o.Reason, &o.TS); err != nil {
			return nil, err
		}
		o.Qty, o.FilledQty, o.AvgFillPx = _qty, _filled, fixed4(avgPx)
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecentFills returns up to limit newest fills, optionally filtered by symbol.
func (s *LiveStore) RecentFills(ctx context.Context, symbol string, limit int) ([]api.LiveFill, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT f.venue_trade_id, o.symbol, f.qty, f.px, f.fee_usd, f.ts
	        FROM tms.fills f JOIN tms.orders o ON o.id = f.order_id`
	var rows pgx.Rows
	var err error
	if symbol == "" {
		rows, err = s.pool.Query(ctx, q+` ORDER BY f.ts DESC, f.id DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, q+` WHERE o.symbol=$1 ORDER BY f.ts DESC, f.id DESC LIMIT $2`, symbol, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.LiveFill
	for rows.Next() {
		var f api.LiveFill
		var px, fee int64
		if err := rows.Scan(&f.TradeID, &f.Symbol, &f.Qty, &px, &fee, &f.TS); err != nil {
			return nil, err
		}
		f.Price, f.Commission = fixed4(px), fixed4(fee)
		out = append(out, f)
	}
	return out, rows.Err()
}

// OpenPositions returns the live position book (non-flat positions).
func (s *LiveStore) OpenPositions(ctx context.Context) ([]api.LiveTradePosition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT strategy_id, symbol, signed_qty, COALESCE(avg_entry_px,0), realized_pnl_usd, status
		  FROM tms.positions
		 WHERE status = 'OPEN' AND signed_qty <> 0
		 ORDER BY strategy_id, symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.LiveTradePosition
	for rows.Next() {
		var p api.LiveTradePosition
		var avg, realized int64
		if err := rows.Scan(&p.StrategyID, &p.Symbol, &p.SignedQty, &avg, &realized, &p.Status); err != nil {
			return nil, err
		}
		p.AvgEntryPx, p.RealizedPnL = fixed4(avg), fixed4(realized)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SessionRealizedPnL returns Σ realized PnL (USD) over the FULL position book —
// open AND closed — so day P/L includes positions closed intraday (e.g. a
// rebalance dropping a sector), which OpenPositions filters out.
func (s *LiveStore) SessionRealizedPnL(ctx context.Context) (float64, error) {
	var realized int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(realized_pnl_usd), 0)
		  FROM tms.positions`).Scan(&realized)
	if err != nil {
		return 0, err
	}
	return fixed4(realized), nil
}

// LatestReconciliation returns the newest reconciliation report, or nil.
func (s *LiveStore) LatestReconciliation(ctx context.Context) (*api.LiveReconciliation, error) {
	var (
		rep        api.LiveReconciliation
		mismatches string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT ts, tolerance_shares, matched, mismatches::text,
		       symbols_only_in_strategies, symbols_only_at_broker, has_issues
		  FROM tms.reconciliation_reports
		 ORDER BY ts DESC, id DESC LIMIT 1`).Scan(
		&rep.TS, &rep.ToleranceShares, &rep.Matched, &mismatches,
		&rep.SymbolsOnlyInStrategies, &rep.SymbolsOnlyAtBroker, &rep.HasIssues)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(mismatches), &rep.Mismatches); err != nil {
		return nil, err
	}
	if rep.Mismatches == nil {
		rep.Mismatches = []api.ReconMismatch{}
	}
	return &rep, nil
}

// compile-time checks.
var (
	_ api.LiveReader        = (*LiveStore)(nil)
	_ api.LiveTradingReader = (*LiveStore)(nil)
)
