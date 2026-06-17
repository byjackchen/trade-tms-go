/**
 * Backtests e2e helpers.
 *
 * These specs exercise backtests — the result + control plane over the DB source
 * of truth (tms.runs / run_metrics / equity_curves / trades; P2 locked decision
 * 4). In the FINAL 4-top IA (docs/concept-alignment.md §3.4 A3) a backtest's
 * object is always a Composition, so this all lives in the Compositions module:
 *   - a launch dialog opened PER-COMPOSITION (`composition-backtest-<id>`),
 *     carrying a `backtest-form` with scoped inputs, submitted by `backtest-submit`;
 *   - the shared `job-progress` panel (data-job-id / job-cancel / job-complete
 *     with a terminal data-outcome) drives every job, backtests included;
 *   - an inline detail panel (deep-linked via /compositions?backtest=<id>) renders
 *     metric cards, an equity chart, a trades table and an orders table, all keyed
 *     off the documented contract.
 *
 * The suite asserts what the UI renders against ground truth queried directly
 * from postgres (lib/db) and the Go API (lib/api) — never fabricated numbers.
 *
 * Launching a real backtest needs market data for the requested window. The
 * deterministic seed plants CLEAN/GAPPY/DELIS tickers; a real stack carries
 * Sharadar tickers (AAPL/KO/…). `pickTickers` chooses two symbols that actually
 * have bars so the engine has something to trade, and specs self-skip when the
 * stack has neither the Compositions backtest flow nor any tradable data yet.
 */

import { withDb } from "./db";

/** A scripted launch: a tiny ~3-month window over two liquid-ish symbols with a
 * single LONG intent, run under the parity (zero-cost) profile so the result is
 * deterministic. Dates are chosen inside the seed's / a real cache's coverage. */
export type ScriptedLaunch = {
  tickers: [string, string];
  start: string; // YYYY-MM-DD
  end: string; // YYYY-MM-DD
  intentDate: string; // YYYY-MM-DD, a trading day inside the window
  intentTicker: string;
};

/** Two tickers that have daily bars, plus the covered date window for the first
 * of them, so a launched backtest has data to run over. Returns null when no
 * ticker has any bars at all (the stack is empty — specs should skip). */
export async function pickScriptedLaunch(): Promise<ScriptedLaunch | null> {
  return withDb(async (c) => {
    // Tickers with the widest bar coverage, newest data first. The seed's
    // synthetic tickers and a real Sharadar cache both satisfy this.
    const { rows } = await c.query<{
      ticker: string;
      min_d: string;
      max_d: string;
      n: string;
    }>(
      `SELECT ticker,
              MIN(ts)::date::text AS min_d,
              MAX(ts)::date::text AS max_d,
              COUNT(*)::text  AS n
         FROM tms.bars_daily
        GROUP BY ticker
       HAVING COUNT(*) >= 5
        ORDER BY COUNT(*) DESC, ticker ASC
        LIMIT 8`,
    );
    if (rows.length < 2) return null;

    const a = rows[0];
    const b = rows[1];

    // A ~3-month (or whatever the coverage allows) window inside a's coverage.
    const minD = new Date(`${a.min_d}T00:00:00Z`);
    const maxD = new Date(`${a.max_d}T00:00:00Z`);
    const spanDays = Math.round(
      (maxD.getTime() - minD.getTime()) / 86_400_000,
    );
    // Aim for ~90 days but never exceed available coverage.
    const windowDays = Math.min(90, spanDays);
    const startD = minD;
    const endD = new Date(startD.getTime() + windowDays * 86_400_000);
    const fmt = (d: Date): string => d.toISOString().slice(0, 10);

    // Intent fires on the first available trading day strictly after start so
    // there is at least one prior bar to fill against.
    const { rows: tdRows } = await c.query<{ date: string }>(
      `SELECT ts::date::text AS date
         FROM tms.bars_daily
        WHERE ticker = $1 AND ts::date > $2 AND ts::date <= $3
        ORDER BY ts ASC
        LIMIT 1`,
      [a.ticker, fmt(startD), fmt(endD)],
    );
    const intentDate = tdRows.length ? tdRows[0].date : a.min_d;

    return {
      tickers: [a.ticker, b.ticker],
      start: fmt(startD),
      end: fmt(endD),
      intentDate,
      intentTicker: a.ticker,
    };
  });
}

/** Terminal job outcomes shared with the Data flow specs. */
export const TERMINAL = new Set(["succeeded", "failed", "canceled"]);
