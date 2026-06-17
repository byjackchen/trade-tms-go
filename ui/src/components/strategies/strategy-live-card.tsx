"use client";

import { useEffect, useMemo, useState } from "react";
import { useLiveSignals } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { StatusDot } from "@/components/portfolio/live-badges";
import { formatRelative } from "@/lib/format";

/**
 * Parse the `state_json` string from a strategy_state frame into a flat record.
 * The shape is strategy-defined; we surface a few well-known keys and otherwise
 * leave the raw document to a details toggle.
 */
function parseState(json: string | null): Record<string, unknown> | null {
  if (!json) return null;
  try {
    const v = JSON.parse(json);
    return v && typeof v === "object" ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

/**
 * One strategy's live state summary card: running indicator (from the
 * strategy_state stream), the latest intent counts by decision-state (from PG +
 * WS), and the latest state document. The intents poll is scoped to this
 * strategy so each card stays independent.
 */
export function StrategyLiveCard({
  strategyId,
  label,
}: {
  strategyId: string;
  label: string;
}) {
  const intentsQ = useLiveSignals(strategyId);
  const [stateJson, setStateJson] = useState<string | null>(null);
  const [stateTs, setStateTs] = useState<string | null>(null);
  const [seen, setSeen] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [showRaw, setShowRaw] = useState(false);

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  useLiveStream({
    onStrategyState: (p) => {
      if (p.strategy_id !== strategyId) return;
      setStateJson(p.state_json);
      setStateTs(new Date(Math.floor(p.ts_event / 1e6)).toISOString());
      setSeen(true);
    },
  });

  // Counts by decision-state across the latest intent per symbol for this
  // strategy (newest-wins, so a symbol is counted once in its current state).
  const counts = useMemo(() => {
    const latest = new Map<string, string>();
    const ts = new Map<string, number>();
    for (const i of intentsQ.data?.signals ?? []) {
      const t = new Date(i.ts).getTime();
      if (!ts.has(i.symbol) || t >= (ts.get(i.symbol) ?? 0)) {
        ts.set(i.symbol, t);
        latest.set(i.symbol, i.state.toLowerCase());
      }
    }
    const acc: Record<string, number> = {};
    for (const s of latest.values()) acc[s] = (acc[s] ?? 0) + 1;
    return acc;
  }, [intentsQ.data]);

  const state = parseState(stateJson);
  const live = seen;
  const totalSymbols = Object.values(counts).reduce((a, b) => a + b, 0);

  return (
    <Card data-testid="strategy-live-card" data-strategy={strategyId}>
      <CardHeader>
        <CardTitle className="flex items-center justify-between gap-2 text-sm">
          <span>{label}</span>
          <span className="font-mono text-xs text-muted-foreground">
            {strategyId}
          </span>
        </CardTitle>
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <StatusDot color={live ? "green" : "gray"} pulse={live} />
          <span data-testid="strategy-live-state">
            {live ? "live" : "no state yet"}
            {stateTs ? ` · ${formatRelative(stateTs, now)}` : ""}
          </span>
        </div>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <div className="flex flex-wrap items-center gap-3" data-testid="strategy-counts">
          <span className="text-xs text-muted-foreground">
            {totalSymbols} {totalSymbols === 1 ? "symbol" : "symbols"}
          </span>
          {Object.keys(counts).length === 0 ? (
            <span className="text-xs text-muted-foreground">no intents yet</span>
          ) : (
            Object.entries(counts)
              .sort((a, b) => b[1] - a[1])
              .map(([st, n]) => (
                <span key={st} className="flex items-center gap-1">
                  <Badge variant="outline" className="text-[10px]">
                    {st}
                  </Badge>
                  <span className="font-mono text-xs">{n}</span>
                </span>
              ))
          )}
        </div>

        {state ? (
          <div className="space-y-1.5">
            <button
              type="button"
              className="text-xs text-muted-foreground underline-offset-2 hover:underline"
              onClick={() => setShowRaw((v) => !v)}
              data-testid="strategy-state-toggle"
            >
              {showRaw ? "hide" : "show"} state summary
            </button>
            {showRaw ? (
              <pre
                className="max-h-48 overflow-auto rounded-lg bg-muted/50 p-2 font-mono text-[11px] leading-relaxed"
                data-testid="strategy-state-json"
              >
                {JSON.stringify(state, null, 2)}
              </pre>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
