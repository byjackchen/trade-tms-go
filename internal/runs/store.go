package runs

// store.go is the PostgreSQL persistence for backtest runs — the DB source of
// truth (locked decision 4). It writes tms.runs / tms.run_metrics /
// tms.equity_curves / tms.trades transactionally and reads them back for the
// HTTP API. Money columns are BIGINT fixed-point at 1e-4 USD (domain.Money raw
// units); sharpe/calmar/max_drawdown are DOUBLE PRECISION float64 (metrics are
// defined at float64 — hyperopt spec §1).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
)

// ErrRunNotFound is returned when a run id / run_ts is unknown.
var ErrRunNotFound = errors.New("runs: run not found")

// Store persists and reads backtest runs.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// PersistInput is a completed backtest ready to persist. The portfolio metrics
// plus per-strategy metrics, the portfolio + per-strategy equity curves and the
// extracted trades are all written under one run id, in one transaction.
type PersistInput struct {
	RunTS           string
	Kind            string
	Status          string // "COMPLETE" (terminal) or "FAIL"
	StartDate       calendar.Date
	EndDate         calendar.Date
	StartingBalance domain.Money
	FinalBalance    domain.Money
	TotalPnL        domain.Money
	Strategies      []string
	Config          json.RawMessage // run params (JSONB)

	PortfolioMetrics metrics.BacktestMetrics
	StrategyMetrics  map[string]metrics.BacktestMetrics

	PortfolioEquity []EquityPoint
	StrategyEquity  map[string][]EquityPoint

	Trades []Trade

	// Orders is the engine's submitted-order list, stored in meta.orders for
	// GET /api/v1/backtests/{id}/orders. Optional.
	Orders []domain.Order
}

// Persist writes the run and all its children, returning the new run id. It is
// idempotent on RunTS: a re-run with the same RunTS replaces the prior rows
// (DELETE+INSERT under the transaction), so a retried job converges instead of
// duplicating. The whole write is one transaction (all-or-nothing).
func (s *Store) Persist(ctx context.Context, in PersistInput) (int64, error) {
	if in.RunTS == "" {
		return 0, fmt.Errorf("runs: persist: empty run_ts")
	}
	status := in.Status
	if status == "" {
		status = "COMPLETE"
	}
	kind := in.Kind
	if kind == "" {
		kind = "multi-strategy"
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	meta, err := buildMeta(in.Orders)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("runs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency: drop any prior run with this ts (cascades to children).
	if _, err := tx.Exec(ctx, `DELETE FROM tms.runs WHERE run_ts = $1`, in.RunTS); err != nil {
		return 0, fmt.Errorf("runs: clearing prior run %s: %w", in.RunTS, err)
	}

	var runID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO tms.runs
		    (run_ts, kind, status, start_date, end_date,
		     starting_balance_usd, final_balance_usd, total_pnl_usd,
		     strategies, config, meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id`,
		in.RunTS, kind, status,
		dateVal(in.StartDate), dateVal(in.EndDate),
		int64(in.StartingBalance), int64(in.FinalBalance), int64(in.TotalPnL),
		in.Strategies, cfg, meta,
	).Scan(&runID)
	if err != nil {
		return 0, fmt.Errorf("runs: inserting run %s: %w", in.RunTS, err)
	}

	// Metrics: portfolio scope plus one row per strategy.
	if err := insertMetrics(ctx, tx, runID, "portfolio", in.PortfolioMetrics); err != nil {
		return 0, err
	}
	for _, sid := range sortedMetricKeys(in.StrategyMetrics) {
		if err := insertMetrics(ctx, tx, runID, sid, in.StrategyMetrics[sid]); err != nil {
			return 0, err
		}
	}

	// Equity curves: portfolio + per strategy.
	if err := insertEquity(ctx, tx, runID, "portfolio", in.PortfolioEquity); err != nil {
		return 0, err
	}
	for _, sid := range sortedEquityKeys(in.StrategyEquity) {
		if err := insertEquity(ctx, tx, runID, sid, in.StrategyEquity[sid]); err != nil {
			return 0, err
		}
	}

	// Trades.
	for _, tr := range in.Trades {
		if err := insertTrade(ctx, tx, runID, tr); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("runs: commit: %w", err)
	}
	return runID, nil
}

func insertMetrics(ctx context.Context, tx pgx.Tx, runID int64, scope string, m metrics.BacktestMetrics) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tms.run_metrics
		    (run_id, scope, final_balance_usd, total_pnl_usd,
		     sharpe, calmar, max_drawdown_pct,
		     num_orders, num_filled_orders, num_rejected_orders, num_positions)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		runID, scope,
		moneyRawFromFloat(m.FinalBalanceUSD), moneyRawFromFloat(m.TotalPnLUSD),
		m.Sharpe, m.Calmar, m.MaxDrawdownPct,
		m.NumOrders, m.NumFilledOrders, m.NumRejectedOrders, m.NumPositions,
	)
	if err != nil {
		return fmt.Errorf("runs: inserting metrics scope=%s: %w", scope, err)
	}
	return nil
}

func insertEquity(ctx context.Context, tx pgx.Tx, runID int64, scope string, pts []EquityPoint) error {
	for _, p := range pts {
		_, err := tx.Exec(ctx, `
			INSERT INTO tms.equity_curves (run_id, scope, ts, balance_usd)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (run_id, scope, ts) DO UPDATE SET balance_usd = EXCLUDED.balance_usd`,
			runID, scope, p.TS.UTC(), int64(p.BalanceUSD))
		if err != nil {
			return fmt.Errorf("runs: inserting equity scope=%s ts=%s: %w", scope, p.TS, err)
		}
	}
	return nil
}

func insertTrade(ctx context.Context, tx pgx.Tx, runID int64, tr Trade) error {
	var exitTS *time.Time
	if tr.ExitTS != nil {
		u := tr.ExitTS.UTC()
		exitTS = &u
	}
	var exitPx *int64
	if tr.ExitPx != nil {
		v := int64(*tr.ExitPx)
		exitPx = &v
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO tms.trades
		    (run_id, strategy_id, symbol, side, qty,
		     entry_ts, exit_ts, entry_px, exit_px, realized_pnl_usd)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		runID, tr.StrategyID, tr.Symbol, tr.Side, int64(tr.Qty),
		tr.EntryTS.UTC(), exitTS, int64(tr.EntryPx), exitPx, int64(tr.RealizedPnL),
	)
	if err != nil {
		return fmt.Errorf("runs: inserting trade %s/%s: %w", tr.StrategyID, tr.Symbol, err)
	}
	return nil
}

// buildMeta marshals the run's meta JSONB. Orders are stored under
// meta.orders for GET /api/v1/backtests/{id}/orders (the DB has no dedicated
// orders table; orders are submission-order opaque blobs, api spec §7.2).
func buildMeta(orders []domain.Order) ([]byte, error) {
	if orders == nil {
		orders = []domain.Order{}
	}
	meta := map[string]any{"orders": orders}
	b, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("runs: marshaling meta: %w", err)
	}
	return b, nil
}

// dateVal converts a calendar.Date to a time.Time at UTC midnight for the DATE
// column (pgx maps time.Time -> date by truncating).
func dateVal(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// moneyRawFromFloat converts a float USD value (metrics use float64) into the
// 1e-4 fixed-point raw units the money columns store, via the domain bridge.
func moneyRawFromFloat(f float64) int64 {
	m, err := domain.MoneyFromFloat64(f)
	if err != nil {
		// Non-finite or out-of-range: store 0 rather than failing the whole
		// persist (metrics are float64 and never produce these in practice;
		// the run row still carries the authoritative fixed-point balances).
		return 0
	}
	return int64(m)
}
