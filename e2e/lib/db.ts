/**
 * Direct postgres access for ground-truth assertions and deterministic seeding.
 *
 * The DB-truth tests compare what the UI renders against numbers computed here
 * straight from `tms.*`, independently of the Go API. If the API mis-counts, the
 * UI will disagree with these queries and the test fails — that is the point.
 *
 * Counting conventions mirror docs/api.md `GET /api/v1/data/coverage`:
 *   - `rows`    = COUNT(*) of the table
 *   - `tickers` = COUNT(DISTINCT ticker)
 * The coverage endpoint reports exactly four tables, in this order:
 *   tickers, bars_daily, fundamentals_sf1, events.
 */

import pg from "pg";
import { PG } from "./env";

// `pg` is CommonJS; under Node's strict ESM resolver only the default import
// is reliable (named `{ Client }` fails to resolve). Destructure the value off
// the default export and alias the type.
const { Client } = pg;
type Client = pg.Client;

const config: pg.ClientConfig = {
  host: PG.host,
  port: PG.port,
  user: PG.user,
  password: PG.password,
  database: PG.database,
  // Keep connection attempts short; the fixture polls reachability separately.
  connectionTimeoutMillis: 5_000,
  statement_timeout: 30_000,
};

/** Run `fn` with a freshly-connected client, always closing it afterward. */
export async function withDb<T>(fn: (c: Client) => Promise<T>): Promise<T> {
  const client = new Client(config);
  await client.connect();
  try {
    return await fn(client);
  } finally {
    await client.end();
  }
}

/** A single coverage row's ground truth: total rows and distinct tickers. */
export type TableTruth = { rows: number; tickers: number };

/** COUNT(*) and COUNT(DISTINCT ticker) for a `tms.<table>` carrying a ticker col. */
export async function tableTruth(
  c: Client,
  table: string,
): Promise<TableTruth> {
  // `table` is a fixed allow-listed identifier (never user input), so the
  // string interpolation is safe; pg cannot parameterize identifiers anyway.
  const allow = new Set([
    "tickers",
    "bars_daily",
    "fundamentals_sf1",
    "events",
  ]);
  if (!allow.has(table)) {
    throw new Error(`tableTruth: refusing unknown table "${table}"`);
  }
  const { rows } = await c.query<{ rows: string; tickers: string }>(
    `SELECT COUNT(*)::text AS rows,
            COUNT(DISTINCT ticker)::text AS tickers
       FROM tms.${table}`,
  );
  return {
    rows: Number(rows[0].rows),
    tickers: Number(rows[0].tickers),
  };
}

/** Number of dataset_sync_runs rows (the sync-runs history the UI lists). */
export async function syncRunCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.dataset_sync_runs`,
  );
  return Number(rows[0].n);
}

/** Number of jobs rows (the recent-jobs panel the UI lists). */
export async function jobCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.jobs`,
  );
  return Number(rows[0].n);
}

// ---------------------------------------------------------------------------
// Ops ground truth (tms.jobs / tms.audit_log — migration 000006_ops). The Ops
// workspace renders the API's projection of these tables; the specs compare
// what the UI shows against these queries (the DB is the source of truth).
// ---------------------------------------------------------------------------

/** One ops.jobs row's identity — the same row the Ops queue table shows. */
export type OpsJobTruth = {
  id: number;
  kind: string;
  status: string;
  claimedBy: string | null;
  attempts: number;
  maxAttempts: number;
};

/** Up to `limit` jobs, newest-first (id DESC) — mirrors GET /api/v1/jobs's
 * ordering. Used to assert the Ops queue rows MATCH the DB. */
export async function recentJobs(
  c: Client,
  limit = 200,
): Promise<OpsJobTruth[]> {
  const { rows } = await c.query<{
    id: string;
    kind: string;
    status: string;
    claimed_by: string | null;
    attempts: string;
    max_attempts: string;
  }>(
    `SELECT id::text AS id, kind, status, claimed_by,
            attempts::text AS attempts, max_attempts::text AS max_attempts
       FROM tms.jobs
      ORDER BY id DESC
      LIMIT $1`,
    [limit],
  );
  return rows.map((r) => ({
    id: Number(r.id),
    kind: r.kind,
    status: r.status,
    claimedBy: r.claimed_by,
    attempts: Number(r.attempts),
    maxAttempts: Number(r.max_attempts),
  }));
}

/** The newest job's id, or null when the queue is empty. */
export async function latestJobId(c: Client): Promise<number | null> {
  const { rows } = await c.query<{ id: string }>(
    `SELECT id::text AS id FROM tms.jobs ORDER BY id DESC LIMIT 1`,
  );
  return rows.length ? Number(rows[0].id) : null;
}

/** Total audit_log rows (the Ops audit panel's lifetime entry count). */
export async function auditCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.audit_log`,
  );
  return Number(rows[0].n);
}

/** One audit_log row's identity, newest-first — mirrors GET /api/v1/audit. */
export type AuditTruth = {
  id: number;
  actor: string;
  action: string;
  entity: string | null;
  entityId: string | null;
};

/** Up to `limit` newest audit rows (id DESC) — used to assert the Ops audit
 * panel rows MATCH the DB (identity, not fabricated). */
export async function recentAudit(
  c: Client,
  limit = 100,
): Promise<AuditTruth[]> {
  const { rows } = await c.query<{
    id: string;
    actor: string;
    action: string;
    entity: string | null;
    entity_id: string | null;
  }>(
    `SELECT id::text AS id, actor, action, entity, entity_id
       FROM tms.audit_log
      ORDER BY id DESC
      LIMIT $1`,
    [limit],
  );
  return rows.map((r) => ({
    id: Number(r.id),
    actor: r.actor,
    action: r.action,
    entity: r.entity,
    entityId: r.entity_id,
  }));
}

/** True when no market data has been imported yet (drives seed-if-empty). */
export async function marketDataIsEmpty(c: Client): Promise<boolean> {
  const t = await tableTruth(c, "bars_daily");
  return t.rows === 0;
}

// ---------------------------------------------------------------------------
// Backtests ground truth (research.* === tms.runs / run_metrics / equity_curves
// / trades — P2 locked decision 4: the DB is the source of truth).
//
// Money is stored as BIGINT fixed-point at 1e-4 USD (dollars * 10000); the API
// renders it as float64 USD. These helpers return *USD floats* so a spec can
// compare them against the rendered metric cards / API payloads directly.
// ---------------------------------------------------------------------------

/** Number of finished (COMPLETE) backtest runs — gates "is there ground truth". */
export async function completeRunCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.runs WHERE status = 'COMPLETE'`,
  );
  return Number(rows[0].n);
}

/** The newest COMPLETE run's id + run_ts, or null when none exists. */
export async function latestCompleteRun(
  c: Client,
): Promise<{ id: number; runTs: string } | null> {
  const { rows } = await c.query<{ id: string; run_ts: string }>(
    `SELECT id::text AS id, run_ts
       FROM tms.runs
      WHERE status = 'COMPLETE'
      ORDER BY run_ts DESC
      LIMIT 1`,
  );
  return rows.length ? { id: Number(rows[0].id), runTs: rows[0].run_ts } : null;
}

/** Portfolio metrics for a run, in USD floats — the ground truth behind the
 * detail page's metric cards. `null` when the run has no metrics row. */
export type RunMetricsTruth = {
  finalBalanceUsd: number;
  totalPnlUsd: number;
  sharpe: number;
  calmar: number;
  maxDrawdownPct: number;
  numOrders: number;
  numFilledOrders: number;
  numRejectedOrders: number;
  numPositions: number;
};

export async function runMetricsTruth(
  c: Client,
  runId: number,
): Promise<RunMetricsTruth | null> {
  const { rows } = await c.query<{
    final_balance_usd: string;
    total_pnl_usd: string;
    sharpe: string;
    calmar: string;
    max_drawdown_pct: string;
    num_orders: string;
    num_filled_orders: string;
    num_rejected_orders: string;
    num_positions: string;
  }>(
    `SELECT final_balance_usd::text,
            total_pnl_usd::text,
            sharpe::text,
            calmar::text,
            max_drawdown_pct::text,
            num_orders::text,
            num_filled_orders::text,
            num_rejected_orders::text,
            num_positions::text
       FROM tms.run_metrics
      WHERE run_id = $1
        AND scope = 'portfolio'`,
    [runId],
  );
  if (!rows.length) return null;
  const r = rows[0];
  return {
    finalBalanceUsd: Number(r.final_balance_usd) / 10000,
    totalPnlUsd: Number(r.total_pnl_usd) / 10000,
    sharpe: Number(r.sharpe),
    calmar: Number(r.calmar),
    maxDrawdownPct: Number(r.max_drawdown_pct),
    numOrders: Number(r.num_orders),
    numFilledOrders: Number(r.num_filled_orders),
    numRejectedOrders: Number(r.num_rejected_orders),
    numPositions: Number(r.num_positions),
  };
}

/** Count of portfolio equity-curve points for a run (the chart's point count). */
export async function equityPointCount(
  c: Client,
  runId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.equity_curves
      WHERE run_id = $1
        AND scope = 'portfolio'`,
    [runId],
  );
  return Number(rows[0].n);
}

/** Count of round-trip trades for a run (the trades table's row count). */
export async function tradeCount(c: Client, runId: number): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.trades WHERE run_id = $1`,
    [runId],
  );
  return Number(rows[0].n);
}

// ---------------------------------------------------------------------------
// Strategies ground truth (tms.param_sets / tms.active_params — the DB
// counterpart of the Python reference's src/strategies/params/* docs, per
// migrations/000003_strategy.up.sql). The Strategies workspace lists the four
// shipped strategies with their *active* parameter document; "active" means the
// payload of the param_set pointed at by tms.active_params, OR — when no
// active_params row exists (the "No row = baseline" case, spec §8.4) — the
// strategy's resolved baseline. These helpers expose the active document
// exactly as the API/UI must render it so a spec can compare UI == ground
// truth without fabricating numbers.
//
// The four canonical strategy ids (internal/params/loader.go: Python package
// stems / baseline file names) are the only strategies that exist.
// ---------------------------------------------------------------------------

/** The four canonical strategy ids, in a stable display order. */
export const STRATEGY_IDS = [
  "sepa",
  "pairs",
  "sector_rotation",
  "intraday_breakout",
] as const;
export type StrategyId = (typeof STRATEGY_IDS)[number];

/** The "active" parameter document for one strategy: the param_sets.payload the
 * active_params pointer selects, plus its identity (id/version/source). `null`
 * when the strategy has no row in either table (a stack that has not seeded the
 * param documents at all — the spec should fall back to the API or skip). */
export type ActiveStrategyTruth = {
  strategy: string;
  /** tms.param_sets.id of the active document (null when only baseline exists). */
  paramSetId: number | null;
  version: number | null;
  source: string | null;
  /** Full params JSON document (strategy, schema_version, display, allocation,
   * metadata, parameters, constraints) — the `parameters` block drives the
   * detail page's param table. */
  payload: Record<string, unknown> | null;
};

/** Active param document for a strategy, resolving the active_params -> param_sets
 * pointer. Returns a row with null payload when the strategy has no stored
 * param_set at all (baseline-only — the UI may still render it from the API's
 * embedded baseline; specs treat a null payload as "use the API as truth"). */
export async function activeStrategy(
  c: Client,
  strategy: string,
): Promise<ActiveStrategyTruth> {
  // Prefer the explicit active_params promotion; otherwise the highest version
  // stored for the strategy (a seeded baseline param_set). Either way we read
  // the payload the run path would resolve from the DB.
  const { rows } = await c.query<{
    id: string | null;
    version: string | null;
    source: string | null;
    payload: Record<string, unknown> | null;
  }>(
    `SELECT ps.id::text       AS id,
            ps.version::text  AS version,
            ps.source         AS source,
            ps.payload        AS payload
       FROM tms.param_sets ps
       LEFT JOIN tms.active_params ap
              ON ap.strategy = ps.strategy
             AND ap.param_set_id = ps.id
      WHERE ps.strategy = $1
      ORDER BY (ap.param_set_id IS NOT NULL) DESC, ps.version DESC
      LIMIT 1`,
    [strategy],
  );
  if (!rows.length) {
    return {
      strategy,
      paramSetId: null,
      version: null,
      source: null,
      payload: null,
    };
  }
  const r = rows[0];
  return {
    strategy,
    paramSetId: r.id != null ? Number(r.id) : null,
    version: r.version != null ? Number(r.version) : null,
    source: r.source,
    payload: r.payload,
  };
}

/** Count of distinct strategies that have at least one stored param_set. Used
 * only to decide whether the DB carries param documents at all (vs. a stack
 * that serves strategies purely from the embedded baseline via the API). */
export async function storedStrategyCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(DISTINCT strategy)::text AS n FROM tms.param_sets`,
  );
  return Number(rows[0].n);
}

// ---------------------------------------------------------------------------
// Live cockpit ground truth (tms.sessions / tms.signal_intents / tms.halts —
// migrations/000005_live). The live read endpoints (docs/api.md "Live (P5)")
// project these tables; the cockpit renders the API's proxy of them. The DB is
// the durable truth (Redis is transport-only), so the cockpit specs compare
// what the UI renders / streams against these queries — no fabricated numbers.
//
// Money columns are BIGINT fixed-point 1e-4 USD; these helpers are count- and
// identity-oriented (intent counts, session mode/status, halt rows), so no
// money decoding is needed here.
// ---------------------------------------------------------------------------

/** The most recent trading session (any trader), or null when none has run.
 * Mirrors GET /api/v1/trade/session's "most recent session" selection.
 *
 * POST-RESTRUCTURE (migration 000016): the `mode` column was DROPPED. A session
 * now carries `exec_policy` (signal|auto) plus a bound account whose `env`
 * (sim|simulate|real) lives in tms.accounts (sessions.account_id). The legacy
 * "mode" label is DERIVED for existing consumers:
 *   - exec_policy = signal                      => "signal"
 *   - exec_policy = auto  AND env = real        => "live"
 *   - exec_policy = auto  AND env = sim|simulate => "paper"
 * `execPolicy` and `env` are also exposed for consumers reading the new shape. */
export type LiveSessionTruth = {
  id: number;
  traderId: string;
  /** Derived legacy label (see deriveSessionMode); kept so existing specs that
   * read `.mode` keep compiling/working against the new schema. */
  mode: "signal" | "paper" | "live";
  /** The session's raw execution policy (tms.sessions.exec_policy). */
  execPolicy: "signal" | "auto";
  /** The bound account's env (tms.accounts.env), or null when no account bound. */
  env: "sim" | "simulate" | "real" | null;
  status: "RUNNING" | "STOPPED" | "CRASHED";
};

/** Derive the legacy "mode" label from the post-restructure (exec_policy, env)
 * pair: signal => "signal"; auto+real => "live"; auto+(sim|simulate) => "paper".
 * Falls back to "signal" when exec_policy is signal regardless of env, and to
 * "paper" for an auto session whose env is unknown (the gate's sim default). */
function deriveSessionMode(
  execPolicy: string,
  env: string | null,
): LiveSessionTruth["mode"] {
  if (execPolicy === "signal") return "signal";
  // exec_policy = auto
  if (env === "real") return "live";
  return "paper"; // sim | simulate (or unknown auto) => paper book
}

export async function latestSession(
  c: Client,
): Promise<LiveSessionTruth | null> {
  const { rows } = await c.query<{
    id: string;
    trader_id: string;
    exec_policy: string;
    env: string | null;
    status: string;
  }>(
    `SELECT s.id::text   AS id,
            s.trader_id  AS trader_id,
            s.exec_policy AS exec_policy,
            a.env        AS env,
            s.status     AS status
       FROM tms.sessions s
       LEFT JOIN tms.accounts a ON a.id = s.account_id
      ORDER BY s.started_at DESC, s.id DESC
      LIMIT 1`,
  );
  if (!rows.length) return null;
  const r = rows[0];
  return {
    id: Number(r.id),
    traderId: r.trader_id,
    mode: deriveSessionMode(r.exec_policy, r.env),
    execPolicy: r.exec_policy as LiveSessionTruth["execPolicy"],
    env: r.env as LiveSessionTruth["env"],
    status: r.status as LiveSessionTruth["status"],
  };
}

/** Total streaming signal-intent rows (the append-only live path: as_of IS
 * NULL; EOD-replay rows carry as_of and are excluded — migration 000010). This
 * is the count the cockpit's live-intent stream grows, and the count that must
 * stop growing once a halt stops new-intent emission. */
export async function streamingIntentCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.signal_intents
      WHERE as_of IS NULL`,
  );
  return Number(rows[0].n);
}

/** A recent streaming intent row's identity, newest first — the same row the
 * cockpit's live-intent table shows at the top. `intent` ground truth is keyed
 * (strategy_id, symbol) newest-wins, matching the UI's dedupe (api spec §3.9). */
export type IntentRowTruth = {
  strategyId: string;
  symbol: string;
  state: string;
  strength: number;
  generation: number;
};

/** Up to `limit` newest streaming intents (as_of IS NULL), newest first — used
 * to assert the rows the cockpit shows MATCH the DB (identity, not fabricated). */
export async function recentStreamingIntents(
  c: Client,
  limit = 50,
): Promise<IntentRowTruth[]> {
  const { rows } = await c.query<{
    strategy_id: string;
    symbol: string;
    state: string;
    strength: string;
    generation: string;
  }>(
    `SELECT strategy_id, symbol, state,
            strength::text   AS strength,
            generation::text AS generation
       FROM tms.signal_intents
      WHERE as_of IS NULL
      ORDER BY ts DESC, id DESC
      LIMIT $1`,
    [limit],
  );
  return rows.map((r) => ({
    strategyId: r.strategy_id,
    symbol: r.symbol,
    state: r.state,
    strength: Number(r.strength),
    generation: Number(r.generation),
  }));
}

/** The set of distinct symbols the recent sessions emitted intents for — the
 * tracked universe behind GET /api/v1/watchlist. */
export async function watchlistSymbols(c: Client): Promise<string[]> {
  const { rows } = await c.query<{ symbol: string }>(
    `SELECT DISTINCT symbol FROM tms.signal_intents ORDER BY symbol ASC`,
  );
  return rows.map((r) => r.symbol);
}

/** Count of currently-active (uncleared) halts. The kill-switch / halt command
 * the cockpit fires writes a tms.halts row; an active halt has cleared_at NULL. */
export async function activeHaltCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.halts WHERE cleared_at IS NULL`,
  );
  return Number(rows[0].n);
}

/** Total halt rows ever (active or cleared) — used to assert the halt command
 * wrote exactly one new row regardless of any later auto-clear. */
export async function haltRowCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.halts`,
  );
  return Number(rows[0].n);
}

// ---------------------------------------------------------------------------
// Paper/live TRADING ground truth (tms.orders / fills / positions /
// risk_events / reconciliation_reports — migration 000005_live; P6 trading
// surface). The cockpit's paper-trading panels (blotter / positions / account
// day-P&L / reconciliation) render the API's proxy of these tables; the API
// reads come straight from PG (the durable system-of-record, decision 5). The
// specs compare what the UI renders against these queries — the DB is the
// truth, never a fabricated number.
//
// Money columns are BIGINT fixed-point 1e-4 USD (stored = dollars * 10000); the
// API renders them as float64 USD. The *Usd helpers below decode to USD floats
// so a spec can compare them against the rendered cards / API payloads directly.
//
// "Active session" here means the newest session (latestSession) — every order/
// position/risk-event/halt row is scoped to a session, and the cockpit reads
// the most-recent session's books. A spec that needs a paper session gates on
// latestSession().mode === 'paper' && status === 'RUNNING'.
// ---------------------------------------------------------------------------

/** Total order rows for a session (the blotter's lifetime order count). */
export async function orderCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.orders WHERE session_id = $1`,
    [sessionId],
  );
  return Number(rows[0].n);
}

/** One order's ground truth, decoded — the same row the blotter shows. */
export type OrderTruth = {
  clientOrderId: string;
  venueOrderId: string | null;
  strategyId: string;
  symbol: string;
  side: "BUY" | "SELL";
  qty: number;
  filledQty: number;
  avgFillPxUsd: number | null;
  status: string;
  reason: string | null;
};

/** Up to `limit` newest orders for a session, newest first — the blotter rows.
 * Decodes avg_fill_px from fixed-point 1e-4 to a USD float. */
export async function recentOrders(
  c: Client,
  sessionId: number,
  limit = 200,
): Promise<OrderTruth[]> {
  const { rows } = await c.query<{
    client_order_id: string;
    venue_order_id: string | null;
    strategy_id: string;
    symbol: string;
    side: string;
    qty: string;
    filled_qty: string;
    avg_fill_px: string | null;
    status: string;
    reason: string | null;
  }>(
    `SELECT client_order_id, venue_order_id, strategy_id, symbol, side,
            qty::text AS qty, filled_qty::text AS filled_qty,
            avg_fill_px::text AS avg_fill_px, status, reason
       FROM tms.orders
      WHERE session_id = $1
      ORDER BY created_at DESC, id DESC
      LIMIT $2`,
    [sessionId, limit],
  );
  return rows.map((r) => ({
    clientOrderId: r.client_order_id,
    venueOrderId: r.venue_order_id,
    strategyId: r.strategy_id,
    symbol: r.symbol,
    side: r.side as OrderTruth["side"],
    qty: Number(r.qty),
    filledQty: Number(r.filled_qty),
    avgFillPxUsd: r.avg_fill_px != null ? Number(r.avg_fill_px) / 10000 : null,
    status: r.status,
    reason: r.reason,
  }));
}

/** Count of FILLED orders for a session (the blotter's filled tally — the
 * paper-trade success signal: a strategy order reached terminal FILLED). */
export async function filledOrderCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.orders
      WHERE session_id = $1 AND status = 'FILLED'`,
    [sessionId],
  );
  return Number(rows[0].n);
}

/** Count of fills (executions) for a session — joins fills to orders so it is
 * session-scoped (fills reference order_id, not session_id directly). */
export async function fillCount(c: Client, sessionId: number): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.fills f
       JOIN tms.orders o ON o.id = f.order_id
      WHERE o.session_id = $1`,
    [sessionId],
  );
  return Number(rows[0].n);
}

/** One open-position row's ground truth, decoded. */
export type PositionTruth = {
  strategyId: string;
  symbol: string;
  signedQty: number;
  avgEntryPxUsd: number | null;
  realizedPnlUsd: number;
  status: "OPEN" | "CLOSED";
};

/** The OPEN (non-flat) position book for a session — the positions-panel rows.
 * Mirrors GET /api/v1/trade/positions (status OPEN; signed_qty <> 0). */
export async function openPositions(
  c: Client,
  sessionId: number,
): Promise<PositionTruth[]> {
  const { rows } = await c.query<{
    strategy_id: string;
    symbol: string;
    signed_qty: string;
    avg_entry_px: string | null;
    realized_pnl_usd: string;
    status: string;
  }>(
    `SELECT strategy_id, symbol,
            signed_qty::text AS signed_qty,
            avg_entry_px::text AS avg_entry_px,
            realized_pnl_usd::text AS realized_pnl_usd,
            status
       FROM tms.positions
      WHERE session_id = $1
        AND status = 'OPEN'
        AND signed_qty <> 0
      ORDER BY symbol ASC`,
    [sessionId],
  );
  return rows.map((r) => ({
    strategyId: r.strategy_id,
    symbol: r.symbol,
    signedQty: Number(r.signed_qty),
    avgEntryPxUsd:
      r.avg_entry_px != null ? Number(r.avg_entry_px) / 10000 : null,
    realizedPnlUsd: Number(r.realized_pnl_usd) / 10000,
    status: r.status as PositionTruth["status"],
  }));
}

/** Count of OPEN (non-flat) positions for a session — the positions-panel count
 * that must drop to ZERO after a FLATTEN. */
export async function openPositionCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.positions
      WHERE session_id = $1 AND status = 'OPEN' AND signed_qty <> 0`,
    [sessionId],
  );
  return Number(rows[0].n);
}

/** Day P&L for a session in USD — Σ realized_pnl over the position book, the
 * same derivation GET /api/v1/trade/account uses (handleTradeAccount). The account
 * panel's "day P/L" card renders this number. */
export async function sessionDayPnlUsd(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ pnl: string | null }>(
    `SELECT (SUM(realized_pnl_usd))::text AS pnl
       FROM tms.positions
      WHERE session_id = $1`,
    [sessionId],
  );
  const raw = rows[0]?.pnl;
  return raw != null ? Number(raw) / 10000 : 0;
}

/** Total risk-event rows for a session (every gate decision worth auditing). */
export async function riskEventCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.risk_events WHERE session_id = $1`,
    [sessionId],
  );
  return Number(rows[0].n);
}

/** Count of REJECTED (approved=false) risk events for a session, optionally
 * filtered to a specific rule_name. The portfolio-gate spec asserts a rejection
 * row appears when an over-budget / over-concentration order is gated. The
 * reference rule ids: allocator.budget_exceeded, risk.max_single_name,
 * risk.concentration, risk.daily_loss_halt (portfolio-risk.md §2.4/§3.2). */
export async function rejectedRiskEventCount(
  c: Client,
  sessionId: number,
  rule?: string,
): Promise<number> {
  const params: Array<number | string> = [sessionId];
  let sql = `SELECT COUNT(*)::text AS n
               FROM tms.risk_events
              WHERE session_id = $1 AND approved = false`;
  if (rule) {
    params.push(rule);
    sql += ` AND rule_name = $2`;
  }
  const { rows } = await c.query<{ n: string }>(sql, params);
  return Number(rows[0].n);
}

/** One rejected risk-event's identity — used to prove the gated order was NOT
 * executed (it has a rejection row; no FILLED order for that client order). */
export type RiskEventTruth = {
  ruleName: string;
  approved: boolean;
  strategyId: string;
  symbol: string;
  side: "LONG" | "SHORT" | "FLAT";
  reason: string;
};

/** Up to `limit` newest rejected risk events for a session, newest first. */
export async function recentRejectedRiskEvents(
  c: Client,
  sessionId: number,
  limit = 50,
): Promise<RiskEventTruth[]> {
  const { rows } = await c.query<{
    rule_name: string;
    approved: boolean;
    strategy_id: string;
    symbol: string;
    side: string;
    reason: string;
  }>(
    `SELECT rule_name, approved, strategy_id, symbol, side, reason
       FROM tms.risk_events
      WHERE session_id = $1 AND approved = false
      ORDER BY ts DESC, id DESC
      LIMIT $2`,
    [sessionId, limit],
  );
  return rows.map((r) => ({
    ruleName: r.rule_name,
    approved: r.approved,
    strategyId: r.strategy_id,
    symbol: r.symbol,
    side: r.side as RiskEventTruth["side"],
    reason: r.reason,
  }));
}

/** Whether a session has any ACTIVE daily_loss halt (the daily-loss-halt spec's
 * durable proof: day P&L below -threshold halts the node, kind = daily_loss). */
export async function activeDailyLossHalt(
  c: Client,
  sessionId: number,
): Promise<boolean> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.halts
      WHERE session_id = $1 AND kind = 'daily_loss' AND cleared_at IS NULL`,
    [sessionId],
  );
  return Number(rows[0].n) > 0;
}

/** The latest reconciliation report (any session), or null — the reconciliation
 * panel's source. Mirrors GET /api/v1/trade/reconciliation: the four lists +
 * mismatches + has_issues (portfolio-risk.md §6). */
export type ReconciliationTruth = {
  hasIssues: boolean;
  toleranceShares: number;
  matched: string[];
  mismatches: Array<{
    symbol: string;
    strategyBooksSum: number;
    brokerNet: number;
    diff: number;
  }>;
  symbolsOnlyInStrategies: string[];
  symbolsOnlyAtBroker: string[];
};

export async function latestReconciliation(
  c: Client,
): Promise<ReconciliationTruth | null> {
  const { rows } = await c.query<{
    has_issues: boolean;
    tolerance_shares: string;
    matched: string[];
    mismatches: Array<{
      symbol: string;
      strategy_books_sum: number;
      broker_net: number;
      diff: number;
    }>;
    symbols_only_in_strategies: string[];
    symbols_only_at_broker: string[];
  }>(
    `SELECT has_issues, tolerance_shares::text AS tolerance_shares,
            matched, mismatches,
            symbols_only_in_strategies, symbols_only_at_broker
       FROM tms.reconciliation_reports
      ORDER BY ts DESC, id DESC
      LIMIT 1`,
  );
  if (!rows.length) return null;
  const r = rows[0];
  return {
    hasIssues: r.has_issues,
    toleranceShares: Number(r.tolerance_shares),
    matched: r.matched ?? [],
    mismatches: (r.mismatches ?? []).map((m) => ({
      symbol: m.symbol,
      strategyBooksSum: Number(m.strategy_books_sum),
      brokerNet: Number(m.broker_net),
      diff: Number(m.diff),
    })),
    symbolsOnlyInStrategies: r.symbols_only_in_strategies ?? [],
    symbolsOnlyAtBroker: r.symbols_only_at_broker ?? [],
  };
}

/** Count of reconciliation reports — gates "has a reconcile run at all". */
export async function reconciliationReportCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n FROM tms.reconciliation_reports`,
  );
  return Number(rows[0].n);
}

// ---------------------------------------------------------------------------
// MANUAL trading desk ground truth (P6, operator-driven — docs/api.md "Manual
// trading desk"). Manual place / cancel / close orders are persisted to the SAME
// durable tables as the auto book (tms.orders / fills / positions / risk_events)
// but attributed to the `MANUAL` pseudo-strategy (strategy_id = 'MANUAL') so
// reconciliation + per-strategy accounting stay clean. Every manual action also
// writes a tms.audit_log row (operator, action, symbol, side, qty, override?,
// live?, ts). These helpers read MANUAL-scoped truth so the desk specs compare
// what the UI renders / the API returns against the DB — never a fabricated row.
//
// Money columns are BIGINT fixed-point 1e-4 USD; the *Usd helpers decode to USD.
// ---------------------------------------------------------------------------

/** The pseudo-strategy id every manual order/position is booked under. */
export const MANUAL_STRATEGY_ID = "MANUAL";

/** Up to `limit` newest MANUAL orders for a session, newest first — the manual
 * blotter's rows (strategy_id = 'MANUAL'). Reuses OrderTruth (decoded). */
export async function recentManualOrders(
  c: Client,
  sessionId: number,
  limit = 200,
): Promise<OrderTruth[]> {
  const { rows } = await c.query<{
    client_order_id: string;
    venue_order_id: string | null;
    strategy_id: string;
    symbol: string;
    side: string;
    qty: string;
    filled_qty: string;
    avg_fill_px: string | null;
    status: string;
    reason: string | null;
  }>(
    `SELECT client_order_id, venue_order_id, strategy_id, symbol, side,
            qty::text AS qty, filled_qty::text AS filled_qty,
            avg_fill_px::text AS avg_fill_px, status, reason
       FROM tms.orders
      WHERE session_id = $1 AND strategy_id = $2
      ORDER BY created_at DESC, id DESC
      LIMIT $3`,
    [sessionId, MANUAL_STRATEGY_ID, limit],
  );
  return rows.map((r) => ({
    clientOrderId: r.client_order_id,
    venueOrderId: r.venue_order_id,
    strategyId: r.strategy_id,
    symbol: r.symbol,
    side: r.side as OrderTruth["side"],
    qty: Number(r.qty),
    filledQty: Number(r.filled_qty),
    avgFillPxUsd: r.avg_fill_px != null ? Number(r.avg_fill_px) / 10000 : null,
    status: r.status,
    reason: r.reason,
  }));
}

/** A single MANUAL order by its client-order-id (within a session), decoded.
 * Used to follow a just-placed manual order to terminal state by its idempotent
 * client-order-id. Null when not yet persisted. */
export async function manualOrderByClientId(
  c: Client,
  sessionId: number,
  clientOrderId: string,
): Promise<OrderTruth | null> {
  const { rows } = await c.query<{
    client_order_id: string;
    venue_order_id: string | null;
    strategy_id: string;
    symbol: string;
    side: string;
    qty: string;
    filled_qty: string;
    avg_fill_px: string | null;
    status: string;
    reason: string | null;
  }>(
    `SELECT client_order_id, venue_order_id, strategy_id, symbol, side,
            qty::text AS qty, filled_qty::text AS filled_qty,
            avg_fill_px::text AS avg_fill_px, status, reason
       FROM tms.orders
      WHERE session_id = $1 AND strategy_id = $2 AND client_order_id = $3
      LIMIT 1`,
    [sessionId, MANUAL_STRATEGY_ID, clientOrderId],
  );
  if (!rows.length) return null;
  const r = rows[0];
  return {
    clientOrderId: r.client_order_id,
    venueOrderId: r.venue_order_id,
    strategyId: r.strategy_id,
    symbol: r.symbol,
    side: r.side as OrderTruth["side"],
    qty: Number(r.qty),
    filledQty: Number(r.filled_qty),
    avgFillPxUsd: r.avg_fill_px != null ? Number(r.avg_fill_px) / 10000 : null,
    status: r.status,
    reason: r.reason,
  };
}

/** The OPEN (non-flat) MANUAL position book for a session — the manual desk's
 * positions rows (strategy_id = 'MANUAL'). */
export async function openManualPositions(
  c: Client,
  sessionId: number,
): Promise<PositionTruth[]> {
  const { rows } = await c.query<{
    strategy_id: string;
    symbol: string;
    signed_qty: string;
    avg_entry_px: string | null;
    realized_pnl_usd: string;
    status: string;
  }>(
    `SELECT strategy_id, symbol,
            signed_qty::text AS signed_qty,
            avg_entry_px::text AS avg_entry_px,
            realized_pnl_usd::text AS realized_pnl_usd,
            status
       FROM tms.positions
      WHERE session_id = $1 AND strategy_id = $2
        AND status = 'OPEN' AND signed_qty <> 0
      ORDER BY symbol ASC`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return rows.map((r) => ({
    strategyId: r.strategy_id,
    symbol: r.symbol,
    signedQty: Number(r.signed_qty),
    avgEntryPxUsd:
      r.avg_entry_px != null ? Number(r.avg_entry_px) / 10000 : null,
    realizedPnlUsd: Number(r.realized_pnl_usd) / 10000,
    status: r.status as PositionTruth["status"],
  }));
}

/** Signed open qty of the MANUAL position in one symbol (0 when flat) — the
 * "position qty -> 0" close assertion reads this. */
export async function manualPositionSignedQty(
  c: Client,
  sessionId: number,
  symbol: string,
): Promise<number> {
  const { rows } = await c.query<{ sq: string | null }>(
    `SELECT signed_qty::text AS sq
       FROM tms.positions
      WHERE session_id = $1 AND strategy_id = $2 AND symbol = $3
        AND status = 'OPEN'
      ORDER BY id DESC
      LIMIT 1`,
    [sessionId, MANUAL_STRATEGY_ID, symbol],
  );
  if (!rows.length || rows[0].sq == null) return 0;
  return Number(rows[0].sq);
}

/** Count of OPEN (non-flat) MANUAL positions for a session. */
export async function openManualPositionCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.positions
      WHERE session_id = $1 AND strategy_id = $2
        AND status = 'OPEN' AND signed_qty <> 0`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return Number(rows[0].n);
}

/** Count of rejected (approved=false) MANUAL risk events for a session — the
 * gate's denials of a manual opening order (strategy_id = 'MANUAL'). A manual
 * order that violates a limit WITHOUT an override flag is rejected here. */
export async function rejectedManualRiskEventCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.risk_events
      WHERE session_id = $1 AND strategy_id = $2 AND approved = false`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return Number(rows[0].n);
}

/** Count of APPROVED MANUAL risk events for a session — an audited override of a
 * limit violation is an approved=true MANUAL gate decision (the operator's
 * recorded decision: risk_events + audit_log). The risk-override spec asserts
 * one appears after the operator checks `override` and resubmits. */
export async function approvedManualRiskEventCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.risk_events
      WHERE session_id = $1 AND strategy_id = $2 AND approved = true`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return Number(rows[0].n);
}

/** Total MANUAL risk-event rows for a session (any decision). */
export async function manualRiskEventCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.risk_events
      WHERE session_id = $1 AND strategy_id = $2`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return Number(rows[0].n);
}

/** Count of audit_log rows whose actor/action concern a manual trade action.
 * Every manual place/cancel/close writes an ops audit row; this gates the
 * "every manual action is audited" assertion. We match the documented manual
 * trade actions (trade.place / trade.cancel / trade.close) case-insensitively
 * and also accept an entity of 'trade'/'MANUAL' so a slightly different action
 * label still counts as a manual-desk audit entry. */
export async function manualAuditCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.audit_log
      WHERE lower(action) LIKE 'trade.%'
         OR lower(action) LIKE '%manual%'
         OR lower(COALESCE(entity,'')) IN ('trade','manual','manual_trade')`,
  );
  return Number(rows[0].n);
}

/** Count of audit_log rows that record a DIRECTION-2 broker -> TMS SYNC action.
 * SyncFromBroker writes a `trade.manual.sync` audit row (entity `manual_trade`,
 * action ending in `.sync` — internal/runner/live_persist.go RecordManualAction
 * + internal/livetrade/manual_sync.go). The sync spec asserts this monotonically
 * grows by exactly the number of syncs performed (the sync is always audited,
 * even though it is READ-ONLY at the broker). We match the `.sync` suffix
 * case-insensitively and also accept a plain `sync` action so a slightly
 * different label still counts. */
export async function syncAuditCount(c: Client): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(*)::text AS n
       FROM tms.audit_log
      WHERE lower(action) LIKE '%.sync'
         OR lower(action) = 'sync'`,
  );
  return Number(rows[0].n);
}

/** Total MANUAL-book position rows for a session, regardless of OPEN/CLOSED or
 * flat/non-flat (strategy_id = 'MANUAL'). DIRECTION-2 sync drives a symbol the
 * broker no longer reports back to flat (an external close), so the sync spec's
 * idempotency / no-duplicate assertion counts *distinct symbols* in the MANUAL
 * book, not just open rows — a re-sync of the same broker state must not add a
 * new symbol row. */
export async function manualPositionSymbolCount(
  c: Client,
  sessionId: number,
): Promise<number> {
  const { rows } = await c.query<{ n: string }>(
    `SELECT COUNT(DISTINCT symbol)::text AS n
       FROM tms.positions
      WHERE session_id = $1 AND strategy_id = $2`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return Number(rows[0].n);
}

/** Distinct symbols held in the OPEN (non-flat) MANUAL book for a session — the
 * set of positions the sync reflected into TMS. Used to assert a synced broker
 * position surfaces under the MANUAL/EXTERNAL book and to prove re-sync adds no
 * new symbol. */
export async function openManualPositionSymbols(
  c: Client,
  sessionId: number,
): Promise<string[]> {
  const { rows } = await c.query<{ symbol: string }>(
    `SELECT DISTINCT symbol
       FROM tms.positions
      WHERE session_id = $1 AND strategy_id = $2
        AND status = 'OPEN' AND signed_qty <> 0
      ORDER BY symbol ASC`,
    [sessionId, MANUAL_STRATEGY_ID],
  );
  return rows.map((r) => r.symbol);
}

/** A registered trading account from the `tms.accounts` registry — the rows the
 * cockpit/desk account selector lists. Mirrors GET /api/v1/trade/accounts
 * (handleTradeAccounts): the first-class account dimension added in the
 * live→trade refactor (migration 000014_accounts). */
export type AccountTruth = {
  id: string;
  venue: string;
  env: "sim" | "simulate" | "real";
  brokerAccId: number;
  label: string;
};

/** The full `tms.accounts` registry, ordered by id — the ground truth the
 * account selector's dropdown options must match (one <option> per row plus the
 * sentinel "All accounts"). */
export async function tradingAccounts(c: Client): Promise<AccountTruth[]> {
  const { rows } = await c.query<{
    id: string;
    venue: string;
    env: string;
    broker_acc_id: string;
    label: string;
  }>(
    `SELECT id, venue, env, broker_acc_id::text AS broker_acc_id, label
       FROM tms.accounts
      ORDER BY id ASC`,
  );
  return rows.map((r) => ({
    id: r.id,
    venue: r.venue,
    env: r.env as AccountTruth["env"],
    brokerAccId: Number(r.broker_acc_id),
    label: r.label,
  }));
}

/** Distinct account_ids that actually carry an OPEN position — the accounts for
 * which selecting `?account=<id>` yields a non-empty positions panel. NULL
 * account_id rows (unattributed legacy/signal book) are excluded. */
export async function accountsWithOpenPositions(c: Client): Promise<string[]> {
  const { rows } = await c.query<{ account_id: string }>(
    `SELECT DISTINCT account_id
       FROM tms.positions
      WHERE account_id IS NOT NULL
        AND status = 'OPEN' AND signed_qty <> 0
      ORDER BY account_id ASC`,
  );
  return rows.map((r) => r.account_id);
}
