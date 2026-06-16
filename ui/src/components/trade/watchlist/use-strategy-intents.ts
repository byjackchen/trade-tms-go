"use client";

import { useEffect, useMemo, useState } from "react";
import { useWatchlist, useLiveIntents } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";

/**
 * A merged per-symbol intent row carrying the FULL unwrapped `intent` JSONB so a
 * per-strategy table can read its purpose-built fields (pivot/z-score/momentum).
 */
export type StrategyIntentRow = {
  symbol: string;
  strategy_id: string;
  state: string;
  strength: number;
  ts: string;
  tsMs: number;
  /** The unwrapped SignalIntentUnion variant for this strategy (open shape). */
  intent: Record<string, unknown>;
};

const isIdleState = (st?: string) => !st || st === "no_setup" || st === "flat";

/**
 * THE per-strategy watchlist data layer. Joins the tracked universe (the
 * `watchlist` payload's own `intents`, the capped `live/intents` poll, and the WS
 * `signal_intent` pushes) into one row per symbol carrying the full intent JSONB,
 * latest-ts wins. Returns rows filtered to `strategyId` plus loading/error/query
 * bits the table needs.
 *
 * `symbolFilter` (the shared search box) narrows by symbol substring. Idle
 * (no_setup/flat) rows are dropped by default for the actionable strategy tabs;
 * pass `includeIdle` to keep the full universe (the Sector tab wants all 11 ETFs
 * even when every one is `hold`/`no_setup`).
 */
export function useStrategyIntents(
  strategyId: string,
  opts: { symbolFilter?: string; includeIdle?: boolean } = {},
) {
  const { symbolFilter = "", includeIdle = false } = opts;
  const symbolsQ = useWatchlist();
  const intentsQ = useLiveIntents(strategyId);
  const [wsIntents, setWsIntents] = useState<Map<string, StrategyIntentRow>>(
    new Map(),
  );
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  useLiveStream({
    onSignalIntent: (p) => {
      if (p.strategy_id !== strategyId) return;
      const intent = p.intent_json ?? {};
      const tsMs = Math.floor(p.ts_event / 1e6);
      const row: StrategyIntentRow = {
        symbol: p.symbol,
        strategy_id: p.strategy_id,
        state: String(intent["state"] ?? intent["signal"] ?? "—"),
        strength: Number(intent["strength"] ?? intent["score"] ?? 0) || 0,
        ts: new Date(tsMs).toISOString(),
        tsMs,
        intent,
      };
      setWsIntents((prev) => {
        const next = new Map(prev);
        const existing = next.get(p.symbol);
        if (!existing || row.tsMs >= existing.tsMs) next.set(p.symbol, row);
        return next;
      });
    },
  });

  // Latest intent per symbol for this strategy, full JSONB preserved.
  const bySymbol = useMemo(() => {
    const m = new Map<string, StrategyIntentRow>();
    const merge = (
      symbol: string,
      sid: string,
      state: string,
      strength: number,
      ts: string,
      intent: Record<string, unknown>,
    ) => {
      if (sid !== strategyId) return;
      const tsMs = new Date(ts).getTime();
      const row: StrategyIntentRow = {
        symbol,
        strategy_id: sid,
        state,
        strength,
        ts,
        tsMs: Number.isFinite(tsMs) ? tsMs : 0,
        intent,
      };
      const existing = m.get(symbol);
      if (!existing || row.tsMs >= existing.tsMs) m.set(symbol, row);
    };
    for (const i of symbolsQ.data?.intents ?? []) {
      merge(i.symbol, i.strategy_id, i.state, i.strength, i.ts, i.intent ?? {});
    }
    for (const i of intentsQ.data?.intents ?? []) {
      merge(i.symbol, i.strategy_id, i.state, i.strength, i.ts, i.intent ?? {});
    }
    for (const [sym, row] of wsIntents) {
      const existing = m.get(sym);
      if (!existing || row.tsMs >= existing.tsMs) m.set(sym, row);
    }
    return m;
  }, [symbolsQ.data, intentsQ.data, wsIntents, strategyId]);

  const rows = useMemo(() => {
    const q = symbolFilter.trim().toUpperCase();
    return [...bySymbol.values()].filter((r) => {
      if (q && !r.symbol.includes(q)) return false;
      if (!includeIdle && isIdleState(r.state)) return false;
      return true;
    });
  }, [bySymbol, symbolFilter, includeIdle]);

  const noReader =
    symbolsQ.error instanceof ApiError && symbolsQ.error.status === 503;

  return {
    rows,
    now,
    isLoading: symbolsQ.isLoading || intentsQ.isLoading,
    error: symbolsQ.error ?? intentsQ.error ?? null,
    noReader,
    refetch: () => {
      symbolsQ.refetch();
      intentsQ.refetch();
    },
  };
}

// ---- Small typed accessors for the open intent JSONB ----

/** Read a numeric field, coercing string-encoded decimals; null when absent/NaN. */
export function num(
  intent: Record<string, unknown>,
  key: string,
): number | null {
  const v = intent[key];
  if (v == null) return null;
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : null;
}

/** Read a string field; null when absent/empty. */
export function str(
  intent: Record<string, unknown>,
  key: string,
): string | null {
  const v = intent[key];
  if (v == null) return null;
  const s = String(v).trim();
  return s === "" ? null : s;
}

/** Read a boolean-ish field (true / "true" / 1). */
export function bool(
  intent: Record<string, unknown>,
  key: string,
): boolean | null {
  const v = intent[key];
  if (v == null) return null;
  if (typeof v === "boolean") return v;
  if (typeof v === "number") return v !== 0;
  const s = String(v).toLowerCase();
  if (s === "true") return true;
  if (s === "false") return false;
  return null;
}
