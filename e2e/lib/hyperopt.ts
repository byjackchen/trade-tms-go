/**
 * Hyperopt studio e2e helpers.
 *
 * The Hyperopt workspace drives NSGA-II walk-forward studies over the
 * deterministic backtest engine (docs/api.md "Hyperopt"; spec
 * docs/spec/hyperopt-metrics.md). A study:
 *   - is launched by `POST /api/v1/hyperopt` (enqueues a `hyperopt.run` job);
 *   - persists to tms.hyperopt_studies / tms.hyperopt_trials (DB source of
 *     truth, migrations/000004_research) plus a runs/hyperopt/<study_ts>/
 *     artifact tree;
 *   - exposes its progress / trials / promotion over the documented REST routes
 *     and the shared WS job stream.
 *
 * In the FINAL 4-top IA (docs/concept-alignment.md §3.4, A2): single-strategy
 * hyperopt FOLDED INTO Strategies — it is the per-strategy "Tune" surface
 * (`strategy-section-tune`) on /strategies, NOT a standalone /hyperopt workspace.
 * The retired /hyperopt route 301-redirects to /strategies and /hyperopt/:id to
 * /strategies?study=:id (see ui/next.config.ts). These specs are PERMANENT and
 * assert the documented contract; while the Tune affordance / a REST route is
 * absent, they self-skip cleanly so the gate stays green, exactly like specs
 * 07-12 did for Backtests / Strategies before those landed.
 *
 * Ground truth is read independently from postgres (this module's `withDb`
 * helpers over tms.hyperopt_studies / hyperopt_trials / active_params /
 * param_sets) AND the Go API — the UI renders the UI's proxy of the API, and
 * both must agree with the DB. No fabricated values.
 *
 * Conventional `data-testid`s (the Strategies "Tune" surface, /strategies):
 *   strategy tab  /strategies (Tune section of the active strategy)
 *     strategy-section-tune         the per-strategy hyperopt (Tune) surface root
 *     hyperopt-launch / open-hyperopt-dialog  launch affordance
 *     hyperopt-dialog / hyperopt-form         launch dialog + form
 *     hyperopt-strategy             strategy <select>
 *     hyperopt-start / hyperopt-end window inputs (YYYY-MM-DD)
 *     hyperopt-population / hyperopt-generations / hyperopt-folds / hyperopt-tickers
 *     hyperopt-submit               submit
 *     hyperopt-study-row-<ts>       one row per study (list)
 *   study detail (inline; deep-linked via /strategies?study={ts})
 *     hyperopt-detail               detail root (data-study-ts === route id)
 *     hyperopt-trials-table         trials table (rows: hyperopt-trial-row-<n>)
 *     pareto-scatter                Pareto-front scatter (canvas/svg points)
 *     trial-fold-row-<n> / trial-fold-<n>-<fold>   per-fold drill-down
 *     hyperopt-promote-<n>          promote affordance for trial n
 *     hyperopt-promote-confirm      confirmation accept
 *     hyperopt-promote-success      success banner
 *   shared job panel (identical to Backtests):
 *     job-progress (data-job-id) / job-cancel / job-complete (data-outcome)
 */

import type { Page } from "@playwright/test";
import { withDb } from "./db";

/** Terminal study statuses surfaced to the UI. The DB stores
 * RUNNING|INTERRUPTED|COMPLETE; the API may synthesize UNKNOWN for a stale
 * coordinator. A study the worker carried to the end lands COMPLETE; a canceled
 * job leaves the study INTERRUPTED. */
export const STUDY_TERMINAL = new Set(["COMPLETE", "INTERRUPTED", "UNKNOWN"]);

/** Terminal *job* outcomes shared with the Data/Backtests flow specs (the WS
 * job panel's data-outcome). */
export const JOB_TERMINAL = new Set(["succeeded", "failed", "canceled"]);

/** The canonical strategy a tiny study runs over. `pairs` is the cheapest to
 * evaluate (two-leg cointegration over a handful of tickers, no universe
 * rebuild) and needs only `tickers` — ideal for a fast, small population. */
export const STUDY_STRATEGY = "pairs";

// ---------------------------------------------------------------------------
// UI readiness — mirrors strategiesUiReady / strategyDetailReady.
// ---------------------------------------------------------------------------

/** True once the Strategies "Tune" (hyperopt) surface is reachable. In the FINAL
 * 4-top IA single-strategy hyperopt lives as the per-strategy Tune section
 * (`strategy-section-tune`) on /strategies — the retired /hyperopt 301-redirects
 * here. Returns false when the Strategies page or the Tune section never appears
 * (not built / strategy has no tuning, e.g. intraday). Navigates to /strategies. */
export async function hyperoptUiReady(page: Page): Promise<boolean> {
  await page.goto("/strategies", { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const page_ = page.getByTestId("strategies-page");
  try {
    await page_.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const tune = page.getByTestId("strategy-section-tune");
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await tune.count()) return true;
    await page.waitForTimeout(250);
  }
  return false;
}

/** Study detail is reachable once the inline `hyperopt-detail` root renders. In
 * the FINAL IA a study deep-links via /strategies?study={ts} (the retired
 * /hyperopt/{ts} 301-redirects there). Returns false when the detail root never
 * appears (deep-link not wired / study absent). Navigates to the deep-link. */
export async function hyperoptDetailReady(
  page: Page,
  ts: string,
): Promise<boolean> {
  await page.goto(`/strategies?study=${ts}`, { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const detail = page.getByTestId("hyperopt-detail");
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await detail.count()) return true;
    await page.waitForTimeout(250);
  }
  return false;
}

// ---------------------------------------------------------------------------
// DB ground truth — tms.hyperopt_studies / tms.hyperopt_trials.
// ---------------------------------------------------------------------------

/** A tiny study launch: `pairs` over a covered window + a few tickers with bars,
 * so NSGA-II has data to evaluate folds over. Population/generations are kept
 * minimal by the spec, not here. Returns null when no ticker has bars. */
export type StudyLaunch = {
  tickers: string[];
  start: string; // YYYY-MM-DD
  end: string; // YYYY-MM-DD
};

/** A few tickers that have daily bars + the overall covered window, so a tiny
 * pairs study has cointegration candidates to fold over. Returns null when the
 * stack carries no bars at all (specs should skip). */
export async function pickStudyLaunch(): Promise<StudyLaunch | null> {
  return withDb(async (c) => {
    const { rows } = await c.query<{ ticker: string }>(
      `SELECT ticker
         FROM tms.bars_daily
        GROUP BY ticker
       HAVING COUNT(*) >= 30
        ORDER BY COUNT(*) DESC, ticker ASC
        LIMIT 4`,
    );
    if (rows.length < 2) return null;
    const { rows: span } = await c.query<{ min_d: string; max_d: string }>(
      `SELECT MIN(ts)::date::text AS min_d, MAX(ts)::date::text AS max_d
         FROM tms.bars_daily
        WHERE ticker = ANY($1)`,
      [rows.map((r) => r.ticker)],
    );
    return {
      tickers: rows.map((r) => r.ticker),
      start: span[0].min_d,
      end: span[0].max_d,
    };
  });
}

/** A study's stored identity + progress, the ground truth behind a list row and
 * the detail header. `null` when the study_ts is unknown. */
export type StudyTruth = {
  studyTs: string;
  strategy: string;
  status: string;
  nTrials: number;
  completedTrials: number;
  failedTrials: number;
  runningTrials: number;
  folds: number;
  /** current_best.trial / .sharpe / .calmar, when a COMPLETE trial exists. */
  bestTrial: number | null;
  bestSharpe: number | null;
  bestCalmar: number | null;
};

/** The newest study (optionally filtered by strategy), or null when none. */
export async function latestStudy(
  strategy?: string,
): Promise<StudyTruth | null> {
  return withDb(async (c) => {
    const { rows } = await c.query<StudyRow>(
      `SELECT study_ts, strategy, status, n_trials, completed_trials,
              failed_trials, running_trials, folds, current_best
         FROM tms.hyperopt_studies
        ${strategy ? "WHERE strategy = $1" : ""}
        ORDER BY study_ts DESC
        LIMIT 1`,
      strategy ? [strategy] : [],
    );
    return rows.length ? rowToStudy(rows[0]) : null;
  });
}

/** A specific study by its study_ts, or null when unknown. */
export async function studyByTs(ts: string): Promise<StudyTruth | null> {
  return withDb(async (c) => {
    const { rows } = await c.query<StudyRow>(
      `SELECT study_ts, strategy, status, n_trials, completed_trials,
              failed_trials, running_trials, folds, current_best
         FROM tms.hyperopt_studies
        WHERE study_ts = $1`,
      [ts],
    );
    return rows.length ? rowToStudy(rows[0]) : null;
  });
}

type StudyRow = {
  study_ts: string;
  strategy: string;
  status: string;
  n_trials: string;
  completed_trials: string;
  failed_trials: string;
  running_trials: string;
  folds: string;
  current_best: { trial?: number; sharpe?: number; calmar?: number } | null;
};

function rowToStudy(r: StudyRow): StudyTruth {
  const best = r.current_best ?? {};
  return {
    studyTs: r.study_ts,
    strategy: r.strategy,
    status: r.status,
    nTrials: Number(r.n_trials),
    completedTrials: Number(r.completed_trials),
    failedTrials: Number(r.failed_trials),
    runningTrials: Number(r.running_trials),
    folds: Number(r.folds),
    bestTrial: typeof best.trial === "number" ? best.trial : null,
    bestSharpe: typeof best.sharpe === "number" ? best.sharpe : null,
    bestCalmar: typeof best.calmar === "number" ? best.calmar : null,
  };
}

/** One trial's ground truth: identity, objective values, params, fold count.
 * `pareto` is computed here (non-dominated over (sharpe, calmar), both
 * maximized — weak dominance with strict improvement, spec §10) so a spec can
 * assert the UI's Pareto flag without trusting the API. */
export type TrialTruth = {
  number: number;
  state: string;
  sharpe: number | null;
  calmar: number | null;
  params: Record<string, unknown>;
  foldCount: number;
  pareto: boolean;
};

/** All trials of a study (ascending number), each carrying a computed
 * `pareto` flag over the study's COMPLETE objective points. */
export async function studyTrials(ts: string): Promise<TrialTruth[]> {
  return withDb(async (c) => {
    const { rows } = await c.query<{
      number: string;
      state: string;
      sharpe: string | null;
      calmar: string | null;
      params: Record<string, unknown>;
      fold_count: string;
    }>(
      `SELECT number,
              state,
              sharpe::text  AS sharpe,
              calmar::text  AS calmar,
              params,
              jsonb_array_length(folds)::text AS fold_count
         FROM tms.hyperopt_trials
        WHERE study_ts = $1
        ORDER BY number ASC`,
      [ts],
    );
    const trials = rows.map((r) => ({
      number: Number(r.number),
      state: r.state,
      sharpe: r.sharpe != null ? Number(r.sharpe) : null,
      calmar: r.calmar != null ? Number(r.calmar) : null,
      params: r.params ?? {},
      foldCount: Number(r.fold_count),
      pareto: false,
    }));
    return markPareto(trials);
  });
}

/**
 * Mark the non-dominated COMPLETE trials over (sharpe, calmar), both maximized.
 * Weak dominance with strict improvement (spec §10): a point A dominates B iff
 * A is >= B in every objective AND strictly greater in at least one. The Pareto
 * front is the set of COMPLETE points not dominated by any other COMPLETE point.
 * Mutates + returns the input for convenience.
 */
export function markPareto(trials: TrialTruth[]): TrialTruth[] {
  const pts = trials.filter(
    (t) => t.state === "COMPLETE" && t.sharpe != null && t.calmar != null,
  );
  for (const a of pts) {
    let dominated = false;
    for (const b of pts) {
      if (b === a) continue;
      const ge =
        (b.sharpe as number) >= (a.sharpe as number) &&
        (b.calmar as number) >= (a.calmar as number);
      const gt =
        (b.sharpe as number) > (a.sharpe as number) ||
        (b.calmar as number) > (a.calmar as number);
      if (ge && gt) {
        dominated = true;
        break;
      }
    }
    a.pareto = !dominated;
  }
  return trials;
}

/** The first COMPLETE trial of a study (lowest number), or null. The promotion
 * specs target a COMPLETE trial — only those can be promoted (§422 rule). */
export function firstCompleteTrial(trials: TrialTruth[]): TrialTruth | null {
  for (const t of trials) if (t.state === "COMPLETE") return t;
  return null;
}

// ---------------------------------------------------------------------------
// Promotion ground truth — tms.active_params + tms.param_sets
// (migrations/000003_strategy). active_params is keyed by strategy (one row);
// a promotion sets param_set_id + audit (promoted_by / promoted_at /
// source_study / source_trial / source_id).
// ---------------------------------------------------------------------------

/** The active-params promotion row for a strategy: which param_set is active and
 * the full audit trail. `null` when no promotion exists (baseline). */
export type ActivePromotion = {
  strategy: string;
  paramSetId: number;
  version: number;
  source: string; // param_sets.source: baseline|tuned|manual|external
  sourceId: string; // active_params.source_id: 'hyperopt:<study_ts>' for a promote
  promotedBy: string;
  sourceStudy: string | null;
  sourceTrial: number | null;
  /** The active param_set's payload (the promoted trial's params, metadata-rewritten). */
  payload: Record<string, unknown> | null;
};

export async function activePromotion(
  strategy: string,
): Promise<ActivePromotion | null> {
  return withDb(async (c) => {
    const { rows } = await c.query<{
      strategy: string;
      param_set_id: string;
      version: string;
      source: string;
      source_id: string;
      promoted_by: string;
      source_study: string | null;
      source_trial: string | null;
      payload: Record<string, unknown> | null;
    }>(
      `SELECT ap.strategy,
              ap.param_set_id::text AS param_set_id,
              ps.version::text      AS version,
              ps.source             AS source,
              ap.source_id          AS source_id,
              ap.promoted_by        AS promoted_by,
              ap.source_study       AS source_study,
              ap.source_trial::text AS source_trial,
              ps.payload            AS payload
         FROM tms.active_params ap
         JOIN tms.param_sets ps ON ps.id = ap.param_set_id
        WHERE ap.strategy = $1`,
      [strategy],
    );
    if (!rows.length) return null;
    const r = rows[0];
    return {
      strategy: r.strategy,
      paramSetId: Number(r.param_set_id),
      version: Number(r.version),
      source: r.source,
      sourceId: r.source_id,
      promotedBy: r.promoted_by,
      sourceStudy: r.source_study,
      sourceTrial: r.source_trial != null ? Number(r.source_trial) : null,
      payload: r.payload,
    };
  });
}

/** Count of active_params rows whose audit points at a given (study, trial).
 * After a promote this must be >= 1 (the audit row exists). */
export async function promotionAuditCount(
  studyTs: string,
  trialNumber: number,
): Promise<number> {
  return withDb(async (c) => {
    const { rows } = await c.query<{ n: string }>(
      `SELECT COUNT(*)::text AS n
         FROM tms.active_params
        WHERE source_study = $1 AND source_trial = $2`,
      [studyTs, trialNumber],
    );
    return Number(rows[0].n);
  });
}
