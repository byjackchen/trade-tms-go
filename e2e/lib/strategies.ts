/**
 * Strategies e2e helpers.
 *
 * The Strategies workspace lists the four shipped strategies (sepa, pairs,
 * sector_rotation, intraday_breakout — internal/params/loader.go canonical ids)
 * with their *active* parameter document, and a per-strategy detail route
 * /strategies/{id} whose param table renders the active `parameters` block. The
 * DB counterpart is tms.param_sets / tms.active_params (migrations/000003);
 * "active" = the payload selected by active_params, falling back to baseline
 * when there is no promotion ("No row = baseline", spec hyperopt §8.4).
 *
 * Build order: this section ships after the P1 Data + P2 Backtests workspaces.
 * The specs are PERMANENT and assert the documented contract; while the section
 * is still the coming-soon placeholder (or the API route is absent) they
 * self-skip cleanly so the gate stays green, exactly like specs 07-09 did for
 * Backtests before that workspace landed.
 *
 * Ground truth is read independently from postgres (lib/db.activeStrategy) AND
 * the Go API (GET /api/v1/strategies[/{id}], if served) — the UI renders the
 * UI's proxy of the API, and both must agree with the DB. No fabricated values.
 */

import type { Page } from "@playwright/test";
import { getAuthed } from "./api";
import { withDb, activeStrategy, STRATEGY_IDS, type StrategyId } from "./db";

export { STRATEGY_IDS };
export type { StrategyId };

/** Number of strategies the section is expected to surface. */
export const EXPECTED_STRATEGY_COUNT = STRATEGY_IDS.length;

/**
 * True once the real Strategies workspace replaced the coming-soon placeholder.
 * Mirrors backtestsUiReady: the list root (`strategies-page`) only exists in the
 * real workspace; the placeholder (`strategies-placeholder`) marks coming-soon.
 * Returns false when neither has appeared (route not built at all).
 */
export async function strategiesUiReady(page: Page): Promise<boolean> {
  await page.goto("/strategies", { waitUntil: "domcontentloaded" });
  // The app shell must always mount; if even that is missing the route 404s and
  // the section is simply not built yet.
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const real = page.getByTestId("strategies-page");
  const placeholder = page.getByTestId("strategies-placeholder");
  // Wait until one of the two surfaces (or neither, briefly) settles.
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await real.count()) return true;
    if (await placeholder.count()) return false;
    await page.waitForTimeout(250);
  }
  return false;
}

/** Detail route is real once /strategies/{id} exposes the `strategy-detail` root
 * (vs. the coming-soon placeholder / an unbuilt route). */
export async function strategyDetailReady(
  page: Page,
  id: string,
): Promise<boolean> {
  await page.goto(`/strategies/${id}`, { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const detail = page.getByTestId("strategy-detail");
  const placeholder = page.getByTestId("strategies-placeholder");
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await detail.count()) return true;
    if (await placeholder.count()) return false;
    await page.waitForTimeout(250);
  }
  return false;
}

/** One resolved active parameter (name + value), as the detail param table and
 * the list's "active params" summary must render it. */
export type ActiveParam = { name: string; value: number | string | boolean };

/**
 * The active parameter values for a strategy, resolved to {name: default}. The
 * params JSON stores each parameter as
 *   "parameters": { "<name>": { "default": <v>, "type": ..., ... }, ... }
 * and the *active value* is that `default` (the value the next run uses — spec
 * §8.4: live processes read params at startup). Order follows the document's
 * insertion order (the param table preserves it).
 *
 * Resolution precedence mirrors the run path: the DB's active param_set payload
 * first; if the DB carries no param_set for the strategy (baseline served from
 * the API's embedded baseline), fall back to GET /api/v1/strategies/{id}. When
 * neither yields a payload, returns null (the caller should skip rather than
 * assert against nothing).
 */
export async function activeParams(
  strategy: string,
): Promise<ActiveParam[] | null> {
  const truth = await withDb((c) => activeStrategy(c, strategy));
  let payload = truth.payload;

  // Baseline-only stack: no stored param_set. Use the API's resolved document
  // (the same source the UI renders) as the ground truth.
  if (!payload) {
    const res = await getAuthed(`strategies/${strategy}`);
    if (res.status === 200 && res.body && typeof res.body === "object") {
      const body = res.body as Record<string, unknown>;
      // The detail payload may be the params document directly, or wrap it under
      // `strategy`/`params`/`payload`. Probe the common shapes.
      payload =
        pickParamsDoc(body) ??
        pickParamsDoc(body.strategy) ??
        pickParamsDoc(body.params) ??
        pickParamsDoc(body.payload) ??
        null;
    }
  }
  if (!payload) return null;
  return paramsFromDoc(payload);
}

/** Returns the object iff it looks like a params document carrying a
 * `parameters` map; otherwise null. */
function pickParamsDoc(v: unknown): Record<string, unknown> | null {
  if (v && typeof v === "object" && "parameters" in (v as object)) {
    const params = (v as Record<string, unknown>).parameters;
    if (params && typeof params === "object") return v as Record<string, unknown>;
  }
  return null;
}

/** Flatten a params document's `parameters` block to ordered {name, value}.
 *
 * Two on-the-wire shapes are supported:
 *   1. The API/DB shape — `parameters` is an ARRAY of param specs, each
 *      `{name, type, default, ...}` (see docs/api.md `GET /api/v1/strategies`).
 *      The resolved value prefers the document's `active_values[name]` (the UI's
 *      rendered value) and falls back to the spec's `default`.
 *   2. A legacy map shape — `parameters` is a name->spec (or name->bare-value)
 *      object. Kept for backward compatibility with any stored param_set payload
 *      that predates the array contract.
 */
export function paramsFromDoc(
  doc: Record<string, unknown>,
): ActiveParam[] {
  const params = doc.parameters as unknown;
  if (!params || typeof params !== "object") return [];

  const activeValues =
    (doc.active_values as Record<string, unknown> | undefined) ?? undefined;
  const resolve = (
    name: string,
    spec: unknown,
  ): number | string | boolean => {
    if (activeValues && name in activeValues) {
      return activeValues[name] as number | string | boolean;
    }
    if (spec && typeof spec === "object" && "default" in (spec as object)) {
      return (spec as { default: number | string | boolean }).default;
    }
    // Some documents store a bare value rather than a {default,...} spec.
    return spec as number | string | boolean;
  };

  const out: ActiveParam[] = [];

  // Shape 1: array of {name, default, ...} specs (the API/DB contract).
  if (Array.isArray(params)) {
    for (const spec of params) {
      if (!spec || typeof spec !== "object") continue;
      const name = (spec as { name?: unknown }).name;
      if (typeof name !== "string" || name === "") continue;
      out.push({ name, value: resolve(name, spec) });
    }
    return out;
  }

  // Shape 2: legacy name -> spec (or bare value) map.
  for (const [name, spec] of Object.entries(
    params as Record<string, unknown>,
  )) {
    out.push({ name, value: resolve(name, spec) });
  }
  return out;
}

/** Parse a rendered cell ("1.50", "5e8", "500,000,000", "true") to a comparable
 * value. Numbers are returned as numbers; non-numeric text returned verbatim
 * (trimmed). Mirrors the parsing the backtest-detail spec uses for metric cards,
 * tolerant of grouping separators the UI may insert. */
export function parseParamCell(text: string): number | string {
  const t = text.trim();
  if (t === "") return "";
  // Strip grouping commas/currency/percent but keep sign, digits, dot, e/E.
  const numeric = t.replace(/[, $%]/g, "");
  if (/^-?\d*\.?\d+([eE][-+]?\d+)?$/.test(numeric)) {
    return Number(numeric);
  }
  return t;
}

/** Two values are "the same active param" if numerically close (display
 * rounding tolerant) or string-equal. */
export function paramValuesMatch(
  rendered: number | string,
  truth: number | string | boolean,
): boolean {
  if (typeof truth === "boolean") {
    return String(rendered).toLowerCase() === String(truth);
  }
  if (typeof truth === "number" && typeof rendered === "number") {
    if (truth === 0) return Math.abs(rendered) < 1e-9;
    return Math.abs(rendered - truth) / Math.abs(truth) < 1e-6;
  }
  return String(rendered).trim() === String(truth).trim();
}
