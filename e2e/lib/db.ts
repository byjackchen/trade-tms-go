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
