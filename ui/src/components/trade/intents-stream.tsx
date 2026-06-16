"use client";

import { useEffect, useMemo, useState } from "react";
import { useLiveIntents } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { IntentStateBadge } from "./live-badges";
import { DisconnectedBanner } from "./disconnected-banner";
import type { LiveIntent, WsSignalIntent } from "@/lib/api/types";
import { formatNum, formatRelative } from "@/lib/format";

/**
 * A normalized intent row keyed by (strategy_id, symbol). The latest intent for
 * a given strategy+symbol wins — the cockpit shows the current decision per
 * instrument, newest first, not an unbounded append log.
 */
type Row = {
  key: string;
  strategy_id: string;
  symbol: string;
  state: string;
  strength: number;
  generation: number;
  reasons: string[];
  ts: string; // RFC3339 (poll) or derived from ts_event (push)
  tsMs: number;
};

const rowKey = (strategyID: string, symbol: string) => `${strategyID}:${symbol}`;

/** Pull human-readable reasons out of the open intent payload, if present. */
function extractReasons(intent: Record<string, unknown> | undefined): string[] {
  if (!intent) return [];
  const r = intent["reasons"] ?? intent["reason"];
  if (Array.isArray(r)) return r.map((x) => String(x));
  if (typeof r === "string" && r) return [r];
  return [];
}

function fromIntent(i: LiveIntent): Row {
  return {
    key: rowKey(i.strategy_id, i.symbol),
    strategy_id: i.strategy_id,
    symbol: i.symbol,
    state: i.state,
    strength: i.strength,
    generation: i.generation,
    reasons: extractReasons(i.intent),
    ts: i.ts,
    tsMs: new Date(i.ts).getTime(),
  };
}

function fromPush(p: WsSignalIntent): Row {
  const intent = p.intent_json ?? {};
  const state = String(intent["state"] ?? intent["signal"] ?? "—");
  const strength = Number(intent["strength"] ?? intent["score"] ?? 0);
  const generation = Number(intent["generation"] ?? 0);
  const tsMs = Math.floor(p.ts_event / 1e6);
  return {
    key: rowKey(p.strategy_id, p.symbol),
    strategy_id: p.strategy_id,
    symbol: p.symbol,
    state,
    strength: Number.isNaN(strength) ? 0 : strength,
    generation: Number.isNaN(generation) ? 0 : generation,
    reasons: extractReasons(intent),
    ts: new Date(tsMs).toISOString(),
    tsMs,
  };
}

/**
 * Live signal-intent stream: the cockpit's primary panel. Hydrates from PG
 * (newest-first), then prepends/replaces rows from the `signal_intent` WS frames
 * in real time. One row per (strategy, symbol) — the current decision — sorted
 * newest first. An optional `strategyId` scopes the panel (used by the
 * strategies-live page).
 */
export function IntentsStream({
  strategyId,
  title = "Signal intents",
  compact = false,
}: {
  strategyId?: string;
  title?: string;
  compact?: boolean;
}) {
  const q = useLiveIntents(strategyId);
  // Live overlay rows, accumulated from WS pushes since mount.
  const [pushed, setPushed] = useState<Map<string, Row>>(new Map());
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  // useLiveStream keeps handlers in a ref, so this closure always sees the
  // current strategyId without us threading a ref ourselves.
  const { state } = useLiveStream({
    onSignalIntent: (p) => {
      // Respect the panel scope: a strategy-scoped panel ignores other strategies.
      if (strategyId && p.strategy_id !== strategyId) return;
      const row = fromPush(p);
      setPushed((prev) => {
        const next = new Map(prev);
        const existing = next.get(row.key);
        // Keep the newest by event time so out-of-order frames don't regress.
        if (!existing || row.tsMs >= existing.tsMs) next.set(row.key, row);
        return next;
      });
    },
  });

  const rows = useMemo(() => {
    const merged = new Map<string, Row>();
    for (const i of q.data?.intents ?? []) {
      const r = fromIntent(i);
      const existing = merged.get(r.key);
      if (!existing || r.tsMs >= existing.tsMs) merged.set(r.key, r);
    }
    for (const [k, r] of pushed) {
      const existing = merged.get(k);
      if (!existing || r.tsMs >= existing.tsMs) merged.set(k, r);
    }
    return [...merged.values()].sort((a, b) => b.tsMs - a.tsMs);
  }, [q.data, pushed]);

  const noReader =
    q.error instanceof ApiError && q.error.status === 503;

  return (
    // `live-intents` + `data-intent-count` is the e2e contract (spec 18): the
    // panel's rendered instrument count must never exceed the DB streaming
    // truth. `data-connected` mirrors the bridge state so the suite can assert
    // the WS opened (spec 18/19 read `live-connection`'s data-connected).
    <Card
      data-testid="live-intents"
      data-panel="intents-stream"
      data-strategy={strategyId ?? "all"}
      data-intent-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">{title}</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid="intents-count">
          {rows.length} {rows.length === 1 ? "instrument" : "instruments"}
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        {/* Connection indicator the e2e suite polls (data-connected boolean).
            Visually redundant with the header LiveIndicator + banner below;
            present for deterministic assertion. */}
        <span
          data-testid="live-connection"
          data-connected={state === "open" ? "true" : "false"}
          data-state={state}
          className="sr-only"
        />
        <DisconnectedBanner state={state} />

        {q.isLoading ? (
          <div className="space-y-2" data-testid="intents-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live reader not configured"
            hint="Start the API with a live reader and a signal session to populate intents."
            data-testid="intents-no-reader"
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid="intents-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No signal intents yet"
            hint="Intents appear here as strategies evaluate. The stream is live."
            data-testid="intents-empty"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Strategy</TableHead>
                <TableHead>Symbol</TableHead>
                <TableHead>Side / state</TableHead>
                <TableHead className="text-right">Strength</TableHead>
                {!compact ? <TableHead>Reasons</TableHead> : null}
                <TableHead className="text-right">As of</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <TableRow
                  key={r.key}
                  data-testid="live-intent-row"
                  data-strategy-id={r.strategy_id}
                  data-symbol={r.symbol}
                  data-state={r.state}
                >
                  <TableCell className="font-mono text-xs">
                    {r.strategy_id}
                  </TableCell>
                  <TableCell className="font-mono font-medium">
                    {r.symbol}
                  </TableCell>
                  <TableCell>
                    <IntentStateBadge state={r.state} />
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {formatNum(r.strength, 1)}
                  </TableCell>
                  {!compact ? (
                    <TableCell
                      className="max-w-[22rem] truncate text-xs text-muted-foreground"
                      title={r.reasons.join("; ")}
                    >
                      {r.reasons.length ? r.reasons.join("; ") : "—"}
                    </TableCell>
                  ) : null}
                  <TableCell
                    className="text-right text-xs text-muted-foreground"
                    title={r.ts}
                  >
                    {formatRelative(r.ts, now)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
