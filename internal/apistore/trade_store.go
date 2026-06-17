package apistore

// trade_store.go is the PG-backed api.TradeReader: it reads the live cockpit
// snapshots from the durable tms.* tables (decision 5: PG is truth). The
// PortfolioHealth snapshot is reconstructed from the latest health stream
// mirror; since signal mode persists no health table, the read derives the
// informational snapshot from the latest session config (flat book) — the same
// values the live publisher emits.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/api"
)

// TradeStore reads live cockpit state from PostgreSQL.
type TradeStore struct {
	pool *pgxpool.Pool
}

// NewTradeStore builds a TradeStore over a pool.
func NewTradeStore(pool *pgxpool.Pool) *TradeStore { return &TradeStore{pool: pool} }

// LatestSession returns the most recent session with its active halt (if any).
func (s *TradeStore) LatestSession(ctx context.Context) (*api.TradeSession, error) {
	var (
		sess       api.TradeSession
		ended      *time.Time
		cfgText    string
		accountEnv *string
	)
	// The 2D model (docs/concept-alignment.md §1.3): exec_policy on the session +
	// the bound account's env (NULL when the session carries no account_id).
	err := s.pool.QueryRow(ctx, `
		SELECT s.id, s.trader_id, s.exec_policy, a.env, s.status, s.started_at, s.ended_at, s.config::text
		  FROM tms.sessions s
		  LEFT JOIN tms.accounts a ON a.id = s.account_id
		 ORDER BY s.started_at DESC, s.id DESC
		 LIMIT 1`).Scan(&sess.ID, &sess.TraderID, &sess.ExecPolicy, &accountEnv, &sess.Status,
		&sess.StartedAt, &ended, &cfgText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.EndedAt = ended
	sess.Config = json.RawMessage(cfgText)
	if accountEnv != nil {
		sess.AccountEnv = *accountEnv
	}

	// Active halt (cleared_at IS NULL), most recent.
	var halt api.TradeHalt
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
func (s *TradeStore) RecentIntents(ctx context.Context, strategyID string, limit int) ([]api.TradeIntent, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		rows pgx.Rows
		err  error
	)
	if strategyID == "" {
		rows, err = s.pool.Query(ctx, `
			SELECT strategy_id, symbol, state, strength, generation, signal::text, ts, ts_event_ns
			  FROM tms.signals
			 ORDER BY ts DESC, id DESC
			 LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT strategy_id, symbol, state, strength, generation, signal::text, ts, ts_event_ns
			  FROM tms.signals
			 WHERE strategy_id = $1
			 ORDER BY ts DESC, id DESC
			 LIMIT $2`, strategyID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []api.TradeIntent
	for rows.Next() {
		var it api.TradeIntent
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
func (s *TradeStore) LatestHealth(ctx context.Context) (*api.TradeHealth, error) {
	sess, err := s.LatestSession(ctx)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}
	// Flat-book signal-mode health: zeros + the session start as the as-of ts.
	return &api.TradeHealth{
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
func (s *TradeStore) Watchlist(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH frontier AS (SELECT max(ts) AS f FROM tms.signals)
		SELECT DISTINCT symbol
		  FROM tms.signals, frontier
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
func (s *TradeStore) LatestIntentsBySymbol(ctx context.Context, limit int) ([]api.TradeIntent, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.pool.Query(ctx, `
		WITH frontier AS (SELECT max(ts) AS f FROM tms.signals),
		latest AS (
			SELECT DISTINCT ON (symbol)
			       strategy_id, symbol, state, strength, generation, signal::text AS itext, ts, ts_event_ns
			  FROM tms.signals, frontier
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

	var out []api.TradeIntent
	for rows.Next() {
		var it api.TradeIntent
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

// --- accounts (P5 step A) ---

// ListAccounts returns the registered trading accounts (the tms.accounts
// registry), ordered by env then id, for the UI account selector / per-account
// filter. One account per node; the UI aggregates a multi-account view.
func (s *TradeStore) ListAccounts(ctx context.Context) ([]api.TradeAccountInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, venue, env, broker_acc_id, label
		  FROM tms.accounts
		 ORDER BY env, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.TradeAccountInfo
	for rows.Next() {
		var a api.TradeAccountInfo
		if err := rows.Scan(&a.ID, &a.Venue, &a.Env, &a.BrokerAccID, &a.Label); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- paper/live trading reads (P6 task 6) ---

// fixed4 converts a 1e-4 fixed-point BIGINT to a float64 USD value.
func fixed4(v int64) float64 { return float64(v) / 10000.0 }

// itoa renders a positional-parameter index ($N) for dynamically built WHERE
// clauses.
func itoa(n int) string { return strconv.Itoa(n) }

// RecentOrders returns up to limit newest orders, optionally filtered by symbol.
func (s *TradeStore) RecentOrders(ctx context.Context, symbol string, limit int) ([]api.TradeOrder, error) {
	return s.RecentOrdersFor(ctx, symbol, "", limit)
}

// RecentOrdersFor is RecentOrders with an optional account filter. accountID ""
// means no filter (all accounts); account_id is nullable so unattributed rows
// only show when no account is selected.
func (s *TradeStore) RecentOrdersFor(ctx context.Context, symbol, accountID string, limit int) ([]api.TradeOrder, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT client_order_id, COALESCE(venue_order_id,''), strategy_id, symbol, side,
	             qty, filled_qty, COALESCE(avg_fill_px,0), status, COALESCE(reason,''),
	             COALESCE(ts_last_event, created_at)
	        FROM tms.orders`
	var (
		conds []string
		args  []any
	)
	if symbol != "" {
		args = append(args, symbol)
		conds = append(conds, "symbol=$"+itoa(len(args)))
	}
	if accountID != "" {
		args = append(args, accountID)
		conds = append(conds, "account_id=$"+itoa(len(args)))
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	q += " ORDER BY created_at DESC, id DESC LIMIT $" + itoa(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.TradeOrder
	for rows.Next() {
		var o api.TradeOrder
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
func (s *TradeStore) RecentFills(ctx context.Context, symbol string, limit int) ([]api.TradeFill, error) {
	return s.RecentFillsFor(ctx, symbol, "", limit)
}

// RecentFillsFor is RecentFills with an optional account filter (see
// RecentOrdersFor). It filters on the fill's own account_id.
func (s *TradeStore) RecentFillsFor(ctx context.Context, symbol, accountID string, limit int) ([]api.TradeFill, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT f.venue_trade_id, o.symbol, f.qty, f.px, f.fee_usd, f.ts
	        FROM tms.fills f JOIN tms.orders o ON o.id = f.order_id`
	var (
		conds []string
		args  []any
	)
	if symbol != "" {
		args = append(args, symbol)
		conds = append(conds, "o.symbol=$"+itoa(len(args)))
	}
	if accountID != "" {
		args = append(args, accountID)
		conds = append(conds, "f.account_id=$"+itoa(len(args)))
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	q += " ORDER BY f.ts DESC, f.id DESC LIMIT $" + itoa(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.TradeFill
	for rows.Next() {
		var f api.TradeFill
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
func (s *TradeStore) OpenPositions(ctx context.Context) ([]api.TradePosition, error) {
	return s.OpenPositionsFor(ctx, "")
}

// OpenPositionsFor is OpenPositions with an optional account filter (see
// RecentOrdersFor).
func (s *TradeStore) OpenPositionsFor(ctx context.Context, accountID string) ([]api.TradePosition, error) {
	q := `
		SELECT strategy_id, symbol, signed_qty, COALESCE(avg_entry_px,0), realized_pnl_usd, status
		  FROM tms.positions
		 WHERE status = 'OPEN' AND signed_qty <> 0`
	var args []any
	if accountID != "" {
		args = append(args, accountID)
		q += " AND account_id=$" + itoa(len(args))
	}
	q += " ORDER BY strategy_id, symbol"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.TradePosition
	for rows.Next() {
		var p api.TradePosition
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
func (s *TradeStore) SessionRealizedPnL(ctx context.Context) (float64, error) {
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
func (s *TradeStore) LatestReconciliation(ctx context.Context) (*api.TradeReconciliation, error) {
	var (
		rep        api.TradeReconciliation
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
	_ api.TradeReader        = (*TradeStore)(nil)
	_ api.TradeTradingReader = (*TradeStore)(nil)
)
