package runs

// read.go is the query side of the runs store: list/detail/equity/trades/orders
// readers backing the HTTP API (GET /api/v1/backtests*). All money is returned
// as domain.Money (1e-4 fixed point); the API layer renders it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
)

// RunSummary is one row of the runs list (newest-first by run_ts).
type RunSummary struct {
	ID              int64
	RunTS           string
	Kind            string
	Status          string
	StartDate       string // YYYY-MM-DD
	EndDate         string // YYYY-MM-DD
	StartingBalance domain.Money
	FinalBalance    *domain.Money // nil until COMPLETE
	TotalPnL        *domain.Money
	Strategies      []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// RunDetail is a run plus its portfolio + per-strategy metrics and the run
// config.
type RunDetail struct {
	RunSummary
	Config           json.RawMessage
	PortfolioMetrics *metrics.BacktestMetrics
	StrategyMetrics  map[string]metrics.BacktestMetrics
}

// EquitySample is one persisted equity-curve point.
type EquitySample struct {
	Scope      string
	TS         time.Time
	BalanceUSD domain.Money
}

// TradeRow is one persisted trade.
type TradeRow struct {
	ID          int64
	StrategyID  string
	Symbol      string
	Side        string
	Qty         domain.Qty
	EntryTS     time.Time
	ExitTS      *time.Time
	EntryPx     domain.Price
	ExitPx      *domain.Price
	RealizedPnL *domain.Money
}

// ListFilter narrows the runs list.
type ListFilter struct {
	Kind   string // "" = any
	Status string // "" = any
	Limit  int    // <=0 -> default 50; capped at 500
}

// List returns runs newest-first by run_ts (api spec §3.14 ordering).
func (s *Store) List(ctx context.Context, f ListFilter) ([]RunSummary, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_ts, kind, status, start_date, end_date,
		       starting_balance_usd, final_balance_usd, total_pnl_usd,
		       strategies, created_at, updated_at
		  FROM tms.runs
		 WHERE ($1 = '' OR kind = $1)
		   AND ($2 = '' OR status = $2)
		 ORDER BY run_ts DESC
		 LIMIT $3`,
		f.Kind, f.Status, limit)
	if err != nil {
		return nil, fmt.Errorf("runs: list: %w", err)
	}
	defer rows.Close()

	var out []RunSummary
	for rows.Next() {
		sum, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: list scan: %w", err)
	}
	return out, nil
}

// Get returns a run detail by id, including metrics, or ErrRunNotFound.
func (s *Store) Get(ctx context.Context, id int64) (*RunDetail, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, run_ts, kind, status, start_date, end_date,
		       starting_balance_usd, final_balance_usd, total_pnl_usd,
		       strategies, created_at, updated_at, config
		  FROM tms.runs WHERE id = $1`, id)
	var d RunDetail
	var start, end time.Time
	var finalBal, totalPnL *int64
	var startBal int64
	err := row.Scan(
		&d.ID, &d.RunTS, &d.Kind, &d.Status, &start, &end,
		&startBal, &finalBal, &totalPnL,
		&d.Strategies, &d.CreatedAt, &d.UpdatedAt, &d.Config,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runs: get %d: %w", id, err)
	}
	d.StartDate = ymd(start)
	d.EndDate = ymd(end)
	d.StartingBalance = domain.Money(startBal)
	d.FinalBalance = moneyPtr(finalBal)
	d.TotalPnL = moneyPtr(totalPnL)

	mrows, err := s.pool.Query(ctx, `
		SELECT scope, final_balance_usd, total_pnl_usd, sharpe, calmar, max_drawdown_pct,
		       num_orders, num_filled_orders, num_rejected_orders, num_positions
		  FROM tms.run_metrics WHERE run_id = $1 ORDER BY scope`, id)
	if err != nil {
		return nil, fmt.Errorf("runs: get metrics %d: %w", id, err)
	}
	defer mrows.Close()
	d.StrategyMetrics = make(map[string]metrics.BacktestMetrics)
	for mrows.Next() {
		var scope string
		var finalRaw, pnlRaw int64
		var m metrics.BacktestMetrics
		if err := mrows.Scan(&scope, &finalRaw, &pnlRaw, &m.Sharpe, &m.Calmar, &m.MaxDrawdownPct,
			&m.NumOrders, &m.NumFilledOrders, &m.NumRejectedOrders, &m.NumPositions); err != nil {
			return nil, fmt.Errorf("runs: scan metrics %d: %w", id, err)
		}
		m.FinalBalanceUSD = domain.Money(finalRaw).Float64()
		m.TotalPnLUSD = domain.Money(pnlRaw).Float64()
		if scope == "portfolio" {
			pm := m
			d.PortfolioMetrics = &pm
		} else {
			d.StrategyMetrics[scope] = m
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("runs: metrics scan %d: %w", id, err)
	}
	return &d, nil
}

// Equity returns equity-curve samples for a run, ascending by ts. scope "" =
// portfolio; any other value selects that strategy's curve. Returns
// ErrRunNotFound when the run id is unknown.
func (s *Store) Equity(ctx context.Context, id int64, scope string) ([]EquitySample, error) {
	if err := s.assertRunExists(ctx, id); err != nil {
		return nil, err
	}
	if scope == "" {
		scope = "portfolio"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT scope, ts, balance_usd
		  FROM tms.equity_curves
		 WHERE run_id = $1 AND scope = $2
		 ORDER BY ts ASC`, id, scope)
	if err != nil {
		return nil, fmt.Errorf("runs: equity %d/%s: %w", id, scope, err)
	}
	defer rows.Close()
	var out []EquitySample
	for rows.Next() {
		var e EquitySample
		var raw int64
		if err := rows.Scan(&e.Scope, &e.TS, &raw); err != nil {
			return nil, fmt.Errorf("runs: scan equity %d: %w", id, err)
		}
		e.BalanceUSD = domain.Money(raw)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: equity scan %d: %w", id, err)
	}
	return out, nil
}

// Trades returns the run's trades ordered by (strategy_id, symbol, entry_ts).
func (s *Store) Trades(ctx context.Context, id int64) ([]TradeRow, error) {
	if err := s.assertRunExists(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, strategy_id, symbol, side, qty, entry_ts, exit_ts,
		       entry_px, exit_px, realized_pnl_usd
		  FROM tms.trades
		 WHERE run_id = $1
		 ORDER BY strategy_id, symbol, entry_ts, id`, id)
	if err != nil {
		return nil, fmt.Errorf("runs: trades %d: %w", id, err)
	}
	defer rows.Close()
	var out []TradeRow
	for rows.Next() {
		var t TradeRow
		var qty int64
		var entryPx int64
		var exitPx *int64
		var pnl *int64
		if err := rows.Scan(&t.ID, &t.StrategyID, &t.Symbol, &t.Side, &qty,
			&t.EntryTS, &t.ExitTS, &entryPx, &exitPx, &pnl); err != nil {
			return nil, fmt.Errorf("runs: scan trade %d: %w", id, err)
		}
		t.Qty = domain.Qty(qty)
		t.EntryPx = domain.Price(entryPx)
		if exitPx != nil {
			p := domain.Price(*exitPx)
			t.ExitPx = &p
		}
		if pnl != nil {
			m := domain.Money(*pnl)
			t.RealizedPnL = &m
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runs: trades scan %d: %w", id, err)
	}
	return out, nil
}

// Orders returns the run's submitted orders as a JSON array, read from the
// run's meta.orders JSONB (the engine's submitted-order list, recorded at
// persist time). Returns an empty array when none were stored.
func (s *Store) Orders(ctx context.Context, id int64) (json.RawMessage, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(meta->'orders', '[]'::jsonb) FROM tms.runs WHERE id = $1`, id)
	var raw json.RawMessage
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("runs: orders %d: %w", id, err)
	}
	return raw, nil
}

// assertRunExists returns ErrRunNotFound when id is unknown.
func (s *Store) assertRunExists(ctx context.Context, id int64) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tms.runs WHERE id = $1)`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("runs: exists %d: %w", id, err)
	}
	if !exists {
		return ErrRunNotFound
	}
	return nil
}

func scanSummary(rows pgx.Row) (RunSummary, error) {
	var s RunSummary
	var start, end time.Time
	var startBal int64
	var finalBal, totalPnL *int64
	if err := rows.Scan(&s.ID, &s.RunTS, &s.Kind, &s.Status, &start, &end,
		&startBal, &finalBal, &totalPnL, &s.Strategies, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return RunSummary{}, fmt.Errorf("runs: scan summary: %w", err)
	}
	s.StartDate = ymd(start)
	s.EndDate = ymd(end)
	s.StartingBalance = domain.Money(startBal)
	s.FinalBalance = moneyPtr(finalBal)
	s.TotalPnL = moneyPtr(totalPnL)
	return s, nil
}

func moneyPtr(v *int64) *domain.Money {
	if v == nil {
		return nil
	}
	m := domain.Money(*v)
	return &m
}

func ymd(t time.Time) string { return t.UTC().Format("2006-01-02") }
