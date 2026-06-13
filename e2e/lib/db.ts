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
