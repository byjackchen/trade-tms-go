package runner

// live_persist.go is the PG-backed durability layer for paper/live trading (P6
// decision 2/4/5/6): it implements the executor's order/fill/position persistence
// + risk-event sink, the gate's risk-event recorder, the reconciler's report
// sink, the trade session's strategy-state store, and the post-timestamp live
// health/position/account Redis publisher. PG is the system-of-record (decision
// 5); Redis is the hot cockpit mirror (best-effort).
//
// All writes are idempotent on their natural key (client-order-id / trade-id /
// (session, strategy, symbol)) so a duplicate broker push or a crash-recovery
// replay never double-writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
	"github.com/byjackchen/trade-tms-go/internal/publish"
)

// LivePersist persists paper/live trading state to PG + mirrors to Redis.
type LivePersist struct {
	pool      *pgxpool.Pool
	publisher *publish.Publisher
	sessionID int64
	accountID string // the bound tms.accounts.id; stamped on every order/fill/position/recon row
	traderID  string
	venue     string // instrument_id venue suffix (e.g. "MOOMOO")
	log       zerolog.Logger
}

// NewLivePersist builds the durability layer for a session. accountID is the
// resolved tms.accounts.id this session is bound to; it is stamped on every
// order / fill / position / reconciliation row so persistence is account-aware.
func NewLivePersist(pool *pgxpool.Pool, pub *publish.Publisher, sessionID int64, accountID, traderID, venue string, log zerolog.Logger) *LivePersist {
	if venue == "" {
		venue = "MOOMOO"
	}
	return &LivePersist{
		pool:      pool,
		publisher: pub,
		sessionID: sessionID,
		accountID: accountID,
		traderID:  traderID,
		venue:     venue,
		log:       log.With().Str("component", "live-persist").Logger(),
	}
}

// UpsertAccount ensures the bound account's row exists in tms.accounts so the FK
// from sessions/orders/positions/fills/reconciliation_reports is satisfiable. It
// is idempotent on the account id (INSERT ... ON CONFLICT DO UPDATE refreshes the
// label + updated_at). MUST be called before the session row is created.
func (p *LivePersist) UpsertAccount(ctx context.Context, a domain.Account) error {
	if p.pool == nil {
		return nil
	}
	if err := a.Validate(); err != nil {
		return fmt.Errorf("upsert account: %w", err)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.accounts (id, venue, env, broker_acc_id, label)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET
		    label      = EXCLUDED.label,
		    updated_at = now()`,
		a.ID, a.Venue, string(a.Env), int64(a.BrokerAccID), a.Label)
	if err != nil {
		return fmt.Errorf("upsert account %s: %w", a.ID, err)
	}
	return nil
}

func (p *LivePersist) instrumentID(symbol string) string { return symbol + "." + p.venue }

// --- executor Persistence (live.orders / live.fills / live.positions) ---

// UpsertOrder writes/updates a tms.orders row keyed by client-order-id. The
// status is mapped to the schema's enum; SUBMITTED/ACCEPTED/etc. pass through.
func (p *LivePersist) UpsertOrder(ctx context.Context, o domain.Order) error {
	if p.pool == nil {
		return nil
	}
	// filled_qty/avg_fill_px ride on the snapshot so a FILLED status arrives with
	// filled_qty=qty in the SAME write — satisfying CHECK (status<>'FILLED' OR
	// filled_qty=qty). Never regress filled_qty on update (GREATEST): a late
	// duplicate or out-of-order push must not walk a FILLED row backwards.
	//
	// order_type + limit_px are written from the order (NOT hardcoded 'MARKET'): the
	// manual desk fully supports LIMIT orders (validated, sent to the venue with the
	// limit price), and the durable record + blotter MUST faithfully reflect the
	// operator's order type + limit price on this audited, real-money-capable surface
	// (finding 2). The schema CHECK ('LIMIT' requires limit_px NOT NULL) is satisfied
	// because the executor only sets Type=LIMIT alongside a positive LimitPrice; a
	// MARKET order carries no limit_px (NULL via nullPx on a nil/zero LimitPrice).
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.orders
		    (session_id, account_id, client_order_id, venue_order_id, strategy_id, symbol, instrument_id,
		     side, order_type, qty, limit_px, tif, status, filled_qty, avg_fill_px, reason, ts_submitted, ts_last_event)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'GTC',$12,$13,$14,$15,$16,$16)
		ON CONFLICT (client_order_id) DO UPDATE SET
		    venue_order_id = COALESCE(EXCLUDED.venue_order_id, tms.orders.venue_order_id),
		    status         = EXCLUDED.status,
		    filled_qty     = GREATEST(EXCLUDED.filled_qty, tms.orders.filled_qty),
		    avg_fill_px    = COALESCE(EXCLUDED.avg_fill_px, tms.orders.avg_fill_px),
		    reason         = COALESCE(NULLIF(EXCLUDED.reason, ''), tms.orders.reason),
		    ts_last_event  = EXCLUDED.ts_last_event`,
		p.sessionID, p.accountIDParam(), o.ClientOrderID, nullStr(o.VenueOrderID), o.StrategyID, o.Symbol,
		p.instrumentID(o.Symbol), string(o.Side), orderTypeOr(o.Type), int64(o.Qty),
		limitPxParam(o.Type, o.LimitPrice), string(o.Status),
		int64(o.FilledQty), nullPx(o.AvgFillPx), nullStr(o.Reason), o.TS.UTC())
	if err != nil {
		return fmt.Errorf("upsert order %s: %w", o.ClientOrderID, err)
	}
	if p.publisher != nil {
		_ = p.publisher.PublishOrder(ctx, o, 0, 0, o.TS.UTC().UnixNano())
	}
	return nil
}

// InsertFill writes a tms.fills row keyed by (order_id, venue_trade_id),
// idempotent: a duplicate trade-id is a no-op (ON CONFLICT DO NOTHING).
func (p *LivePersist) InsertFill(ctx context.Context, f domain.Fill) error {
	if p.pool == nil {
		return nil
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.fills (order_id, account_id, venue_trade_id, qty, px, fee_usd, ts)
		SELECT o.id, o.account_id, $2, $3, $4, $5, $6
		  FROM tms.orders o
		 WHERE o.client_order_id = $1
		ON CONFLICT (order_id, venue_trade_id) DO NOTHING`,
		f.ClientOrderID, f.TradeID, int64(f.Qty), int64(f.Price), int64(f.Commission), f.TS.UTC())
	if err != nil {
		return fmt.Errorf("insert fill %s: %w", f.TradeID, err)
	}
	// Roll up the order's filled_qty + avg_fill_px from its fills.
	_, _ = p.pool.Exec(ctx, `
		UPDATE tms.orders o SET
		    filled_qty = sub.fqty,
		    avg_fill_px = sub.avgpx,
		    status = CASE WHEN sub.fqty >= o.qty THEN 'FILLED'
		                  WHEN sub.fqty > 0 THEN 'PARTIALLY_FILLED'
		                  ELSE o.status END
		FROM (
		    SELECT order_id, sum(qty) AS fqty,
		           CASE WHEN sum(qty) > 0 THEN sum(qty*px)/sum(qty) ELSE NULL END AS avgpx
		      FROM tms.fills WHERE order_id = (SELECT id FROM tms.orders WHERE client_order_id=$1)
		     GROUP BY order_id) sub
		WHERE o.id = sub.order_id`, f.ClientOrderID)
	if p.publisher != nil {
		_ = p.publisher.PublishFill(ctx, f)
	}
	return nil
}

// UpsertPosition writes/updates a tms.positions row keyed by (session, position_id).
func (p *LivePersist) UpsertPosition(ctx context.Context, pos domain.Position) error {
	if p.pool == nil {
		return nil
	}
	positionID := pos.StrategyID + "|" + pos.Symbol
	status := "OPEN"
	var closedAt any
	if pos.SignedQty == 0 {
		status = "CLOSED"
		closedAt = pos.UpdatedAt.UTC()
	}
	var avgEntry any
	if pos.AvgPx > 0 && pos.SignedQty != 0 {
		avgEntry = int64(pos.AvgPx)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.positions
		    (session_id, account_id, position_id, strategy_id, symbol, instrument_id,
		     signed_qty, avg_entry_px, realized_pnl_usd, status, opened_at, closed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (session_id, position_id) DO UPDATE SET
		    account_id       = EXCLUDED.account_id,
		    signed_qty       = EXCLUDED.signed_qty,
		    avg_entry_px     = EXCLUDED.avg_entry_px,
		    realized_pnl_usd = EXCLUDED.realized_pnl_usd,
		    status           = EXCLUDED.status,
		    closed_at        = EXCLUDED.closed_at`,
		p.sessionID, p.accountIDParam(), positionID, pos.StrategyID, pos.Symbol, p.instrumentID(pos.Symbol),
		int64(pos.SignedQty), avgEntry, int64(pos.RealizedPnL), status, pos.UpdatedAt.UTC(), closedAt)
	if err != nil {
		return fmt.Errorf("upsert position %s: %w", positionID, err)
	}
	return nil
}

// RecordRiskEvent records an executor-level safety event (-> tms.risk_events).
func (p *LivePersist) RecordRiskEvent(ctx context.Context, strategyID, symbol, rule, detail string) error {
	return p.insertRiskEvent(ctx, false, rule, strategyID, symbol, domain.SideFlat, 0, 0, detail)
}

// RecordManualAction appends a manual-desk action to tms.audit_log (the
// livetrade.AuditSink seam): operator, action (place/cancel/close), symbol, side,
// qty, override?, live?, ts. Append-only (never updated/deleted). EVERY manual
// action audits — this is a hard safety requirement (the desk can place real
// orders), so an audit-write failure is returned to the caller (the desk logs it)
// rather than silently dropped.
func (p *LivePersist) RecordManualAction(ctx context.Context, a livetrade.ManualAuditRecord) error {
	if p.pool == nil {
		return nil
	}
	details := map[string]any{
		"action":          a.Action,
		"symbol":          a.Symbol,
		"side":            a.Side,
		"qty":             a.Qty,
		"order_type":      a.OrderType,
		"client_order_id": a.ClientOrderID,
		"override":        a.Override,
		"live":            a.Live,
		"session_id":      p.sessionID,
		"trader_id":       p.traderID,
	}
	if a.RiskRule != "" {
		details["risk_rule_overridden"] = a.RiskRule
	}
	detBytes, _ := json.Marshal(details)
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO tms.audit_log (actor, action, entity, entity_id, details, ts)
		 VALUES ($1, $2, 'manual_trade', $3, $4::jsonb, $5)`,
		actorOr(a.Operator), "trade.manual."+a.Action, nullStr(a.ClientOrderID),
		string(detBytes), a.TS.UTC()); err != nil {
		return fmt.Errorf("record manual action %s: %w", a.Action, err)
	}
	return nil
}

// actorOr defaults an empty operator to a non-empty sentinel (the audit_log actor
// column CHECKs actor <> ”).
func actorOr(operator string) string {
	if operator == "" {
		return "manual-desk:unknown"
	}
	return operator
}

// StrategyForOrder re-keys a restored broker order to its originating strategy
// id during crash recovery (decision 6) by reading the durable order row written
// at submit (tms.orders, keyed by client-order-id). It is scoped to THIS session
// so a restored in-flight order attributes to the strategy that actually placed
// it. ok=false (nil error) means no row exists for that client-order-id.
func (p *LivePersist) StrategyForOrder(ctx context.Context, clientOrderID string) (string, bool, error) {
	if p.pool == nil {
		return "", false, nil
	}
	var strategyID string
	err := p.pool.QueryRow(ctx,
		`SELECT strategy_id FROM tms.orders WHERE session_id=$1 AND client_order_id=$2`,
		p.sessionID, clientOrderID).Scan(&strategyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve strategy for order %s: %w", clientOrderID, err)
	}
	return strategyID, strategyID != "", nil
}

// --- gate RiskRecorder (live.risk_events: pre-submit gate decisions) ---

// RecordGateDecision records a pre-submit gate decision (-> tms.risk_events).
func (p *LivePersist) RecordGateDecision(ctx context.Context, d livetrade.GateDecision) error {
	return p.insertRiskEvent(ctx, d.Approved, d.RuleName, d.StrategyID, d.Symbol, d.Side, d.Qty, d.Price, d.Reason)
}

func (p *LivePersist) insertRiskEvent(ctx context.Context, approved bool, rule, strategyID, symbol string, side domain.SignalSide, qty domain.Qty, price domain.Price, reason string) error {
	if p.pool == nil {
		return nil
	}
	if rule == "" {
		rule = "safety.unspecified"
	}
	sideStr := string(side)
	if sideStr == "" {
		sideStr = string(domain.SideFlat)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.risk_events (session_id, rule_name, approved, strategy_id, symbol, side, qty, price, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		p.sessionID, rule, approved, strategyID, symbol, sideStr, int64(qty), int64(price), reason)
	if err != nil {
		return fmt.Errorf("insert risk event %s: %w", rule, err)
	}
	return nil
}

// --- reconciler ReportSink (tms.reconciliation_reports) ---

// SaveReconciliation persists a reconciliation report (-> tms.reconciliation_reports).
func (p *LivePersist) SaveReconciliation(ctx context.Context, r portfolio.ReconciliationReport, tolerance int64) error {
	if p.pool == nil {
		return nil
	}
	mismatches := make([]map[string]any, 0, len(r.Mismatches))
	for _, m := range r.Mismatches {
		mismatches = append(mismatches, map[string]any{
			"symbol":             m.Symbol,
			"strategy_books_sum": m.StrategyBooksSum,
			"broker_net":         m.BrokerNet,
			"diff":               m.Diff,
		})
	}
	mb, _ := json.Marshal(mismatches)
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.reconciliation_reports
		    (session_id, account_id, ts, tolerance_shares, matched, mismatches,
		     symbols_only_in_strategies, symbols_only_at_broker)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`,
		p.sessionID, p.accountIDParam(), r.TS.UTC(), tolerance, textArray(r.Matched), string(mb),
		textArray(r.SymbolsOnlyInStrategies), textArray(r.SymbolsOnlyAtBroker))
	if err != nil {
		return fmt.Errorf("save reconciliation report: %w", err)
	}
	return nil
}

// --- trade-session StateStore (tms.strategy_state) ---

// SaveState upserts strategyID's SG state (-> tms.strategy_state), bumping the
// generation counter on each change.
func (p *LivePersist) SaveState(ctx context.Context, strategyID string, state []byte) error {
	if p.pool == nil || len(state) == 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tms.strategy_state (trader_id, strategy_id, session_id, state, generation)
		VALUES ($1,$2,$3,$4::jsonb,0)
		ON CONFLICT (trader_id, strategy_id) DO UPDATE SET
		    state      = EXCLUDED.state,
		    session_id = EXCLUDED.session_id,
		    generation = tms.strategy_state.generation + 1`,
		p.traderID, strategyID, p.sessionID, string(state))
	if err != nil {
		return fmt.Errorf("save strategy state %s: %w", strategyID, err)
	}
	return nil
}

// LoadState loads strategyID's last persisted SG state (-> tms.strategy_state).
func (p *LivePersist) LoadState(ctx context.Context, strategyID string) ([]byte, bool, error) {
	if p.pool == nil {
		return nil, false, nil
	}
	var state string
	err := p.pool.QueryRow(ctx,
		`SELECT state::text FROM tms.strategy_state WHERE trader_id=$1 AND strategy_id=$2`,
		p.traderID, strategyID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load strategy state %s: %w", strategyID, err)
	}
	return []byte(state), true, nil
}

// --- HealthSink (post-timestamp live snapshot -> Redis cockpit) ---

// EmitLiveHealth publishes the live health + position book + account snapshot to
// Redis (best-effort). The daily-loss-halt flag rides on the health envelope.
func (p *LivePersist) EmitLiveHealth(ctx context.Context, snap livetrade.LiveSnapshot) error {
	if p.publisher == nil {
		return nil
	}
	tsNS := snap.AsOf.UTC().UnixNano()
	_ = p.publisher.PublishPortfolioHealth(ctx, publish.PortfolioHealthEnvelope{
		DayPnL:           snap.Health.DayPnLFloat(),
		DayPnLPct:        snap.Health.DayPnLPctFloat(),
		DailyLossHalt:    snap.DailyLossHalted,
		HaltHeadroomPct:  snap.Health.HaltHeadroomPctFloat(),
		ConcentrationPct: snap.Health.ConcentrationPctFloat(),
		TSEvent:          tsNS,
	})
	positions := make([]publish.LivePosition, 0, len(snap.Positions))
	for _, pos := range snap.Positions {
		positions = append(positions, publish.LivePosition{
			StrategyID:  pos.StrategyID,
			Symbol:      pos.Symbol,
			SignedQty:   int64(pos.SignedQty),
			AvgPx:       pos.AvgPx.Float64(),
			RealizedPnL: pos.RealizedPnL.Float64(),
		})
	}
	_ = p.publisher.PublishLivePositions(ctx, positions, tsNS)
	return nil
}

// PublishAccount mirrors the broker funds + day P&L to Redis (called by the live
// node on a cadence). Best-effort.
func (p *LivePersist) PublishAccount(ctx context.Context, env publish.AccountEnvelope) {
	if p.publisher == nil {
		return
	}
	_ = p.publisher.PublishAccount(ctx, env)
}

// --- small helpers ---

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// accountIDParam maps the bound account id to the account_id column value (the
// FK), or SQL NULL when unset (the column is nullable through phase 3).
func (p *LivePersist) accountIDParam() any { return nullStr(p.accountID) }

// nullPx maps a non-positive price to SQL NULL (the avg_fill_px column is
// `NULL OR > 0`): a not-yet-filled order has no average fill price.
func nullPx(px domain.Price) any {
	if px <= 0 {
		return nil
	}
	return int64(px)
}

// orderTypeOr maps the order's type to the schema enum, defaulting an empty type
// to 'MARKET' (a defensive default for a snapshot that omitted it). The schema
// CHECK restricts order_type to MARKET/LIMIT/STOP_MARKET/STOP_LIMIT.
func orderTypeOr(t domain.OrderType) string {
	if t == "" {
		return string(domain.OrderTypeMarket)
	}
	return string(t)
}

// limitPxParam returns the limit_px column value (USD fixed-point 1e-4) for an
// order: the positive limit price for a LIMIT/STOP_LIMIT order, else SQL NULL. The
// schema CHECK requires limit_px NOT NULL for LIMIT/STOP_LIMIT and (limit_px > 0),
// so a LIMIT order with a missing/non-positive price would (correctly) fail the
// write rather than persist a malformed record — but the executor never produces
// one (it sets Type=LIMIT only alongside a positive LimitPrice).
func limitPxParam(t domain.OrderType, px *domain.Price) any {
	if t != domain.OrderTypeLimit && t != domain.OrderTypeStopLimit {
		return nil
	}
	if px == nil || *px <= 0 {
		return nil
	}
	return int64(*px)
}

func textArray(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// compile-time checks: LivePersist satisfies every durability seam.
var (
	_ moexec.Persistence      = (*LivePersist)(nil)
	_ moexec.RiskEventSink    = (*LivePersist)(nil)
	_ moexec.StrategyResolver = (*LivePersist)(nil)
	_ livetrade.RiskRecorder  = (*LivePersist)(nil)
	_ livetrade.AuditSink     = (*LivePersist)(nil)
	_ livetrade.ReportSink    = (*LivePersist)(nil)
	_ livetrade.StateStore    = (*LivePersist)(nil)
	_ livetrade.HealthSink    = (*LivePersist)(nil)
)
