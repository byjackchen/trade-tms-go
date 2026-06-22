"use client";

import { useCallback, useRef, useState } from "react";
import { Radio } from "lucide-react";
import { cn } from "@/lib/utils";
import { useJobStream } from "@/lib/api/use-job-stream";
import type { WsEnvelope, WsBar } from "@/lib/api/types";

type TapeRow = WsBar & { _k: number };

/** How many closed bars the rolling tape keeps in view (newest first). */
const MAX_ROWS = 30;

/**
 * <TickTape> — an ephemeral live BAR tape. Tails the `bar_update` WS frames the
 * STREAMING trade node emits (one closed K-line per symbol per period, after the
 * feed's intraday forming-coalescing) and shows a small rolling window. NOTHING
 * is persisted — it's a monitoring ticker, dropped on unmount.
 *
 * Empty until a streaming session with a live market feed is running: a
 * `tms trade run` session (signal/paper/live all stream bars — exec policy only
 * gates ORDERS, not the feed). The MANUAL desk holds a trade connection but no
 * K-line feed, so its session shows the empty hint.
 */
export function TickTape() {
  const [rows, setRows] = useState<TapeRow[]>([]);
  const seq = useRef(0);

  const onEnvelope = useCallback((env: WsEnvelope) => {
    if (env.type !== "bar_update") return;
    const p = env.payload as unknown as WsBar;
    if (!p || typeof p.symbol !== "string") return;
    setRows((prev) => [{ ...p, _k: seq.current++ }, ...prev].slice(0, MAX_ROWS));
  }, []);

  const { state } = useJobStream({ onEnvelope });

  return (
    <div className="rounded-lg border border-border" data-testid="tick-tape">
      <div className="flex items-center justify-between border-b border-border px-3 py-2">
        <div className="flex items-center gap-2">
          <Radio
            className={cn(
              "size-3.5",
              state === "open" ? "text-emerald-500" : "text-muted-foreground",
            )}
          />
          <span className="text-xs font-medium">Live tape</span>
        </div>
        <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
          {rows.length > 0 ? `${rows.length} bars` : state}
        </span>
      </div>

      {rows.length === 0 ? (
        <p
          className="px-3 py-4 text-center text-xs text-muted-foreground"
          data-testid="tick-tape-empty"
        >
          No live feed. Bars stream here once a trading session with a market feed
          is running — start a `tms trade run` session (signal works too). The
          manual desk holds a trade connection but no K-line feed.
        </p>
      ) : (
        <ul
          className="cockpit-scroll max-h-48 divide-y divide-border/60 overflow-y-auto font-mono text-xs"
          data-testid="tick-tape-rows"
        >
          {rows.map((r) => {
            const up = r.close >= r.open;
            return (
              <li
                key={r._k}
                className="flex items-center justify-between gap-3 px-3 py-1"
                data-testid="tick-tape-row"
                data-symbol={r.symbol}
              >
                <span className="w-16 shrink-0 truncate font-medium">
                  {r.symbol}
                </span>
                <span
                  className={cn(
                    "flex-1 text-right tabular-nums",
                    up
                      ? "text-emerald-600 dark:text-emerald-400"
                      : "text-destructive",
                  )}
                >
                  {fmtPrice(r.close)}
                </span>
                <span className="shrink-0 text-muted-foreground">
                  {fmtBarTime(r)}
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

/** Two-decimal price, em-dash on a non-finite value. */
function fmtPrice(n: number): string {
  return Number.isFinite(n) ? n.toFixed(2) : "—";
}

/**
 * A DAILY bar is a trading DAY, not a moment, so show its exchange-local trading
 * DATE (e.g. "Jun 22") — NOT the midnight-ET instant rendered in the viewer's
 * timezone (which misleadingly reads as the previous evening). An intraday bar's
 * instant IS meaningful, so show local time-of-day.
 */
function fmtBarTime(r: TapeRow): string {
  if (r.interval_seconds >= 86400 && r.trading_date) {
    // trading_date is an authoritative NY date "YYYY-MM-DD"; parse it as UTC
    // midnight and render in UTC so the calendar date never shifts.
    const d = new Date(`${r.trading_date}T00:00:00Z`);
    return Number.isNaN(d.getTime())
      ? r.trading_date
      : d.toLocaleDateString(undefined, {
          month: "short",
          day: "numeric",
          timeZone: "UTC",
        });
  }
  if (!Number.isFinite(r.ts_event) || r.ts_event <= 0) return "—";
  return new Date(r.ts_event / 1e6).toLocaleTimeString(undefined, {
    hour12: false,
  });
}
