"use client";

import { AlertOctagon, Radio, FlaskConical, Banknote } from "lucide-react";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import type { LiveMode } from "@/lib/api/types";

/**
 * The cockpit's loud, always-on mode + halt banner. Mode colors are deliberately
 * distinct so an operator can never mistake LIVE-REAL for PAPER or SIGNAL at a
 * glance:
 *   - signal  → muted/blue (informational; never trades)
 *   - paper   → amber (simulated fills, no real money)
 *   - live    → red, full-width destructive (REAL money, real orders)
 * A halt overlays a destructive sub-banner regardless of mode.
 */
const MODE_META: Record<
  string,
  {
    label: string;
    Icon: typeof Radio;
    cls: string;
    note: string;
  }
> = {
  signal: {
    label: "SIGNAL",
    Icon: Radio,
    cls: "border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-300",
    note: "Signals only — no orders are ever submitted.",
  },
  paper: {
    label: "PAPER",
    Icon: FlaskConical,
    cls: "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-300",
    note: "Simulated fills against the SIMULATE account — no real money.",
  },
  live: {
    label: "LIVE — REAL MONEY",
    Icon: Banknote,
    cls: "border-destructive/60 bg-destructive/10 text-destructive",
    note: "REAL orders are placed against the real-money account.",
  },
};

export function ModeBanner() {
  const q = useLiveSession();
  const session = hasSession(q.data) ? q.data : null;
  const mode: LiveMode = session?.mode ?? "signal";
  const meta = MODE_META[String(mode)] ?? MODE_META.signal!;
  const halted = session?.halt != null;
  const Icon = meta.Icon;

  return (
    <div
      data-testid="live-mode-banner"
      data-mode={mode}
      data-halted={halted ? "true" : "false"}
      className="space-y-2"
    >
      <div
        className={`flex flex-wrap items-center gap-3 rounded-lg border px-4 py-2.5 ${meta.cls}`}
      >
        <Icon className="size-5 shrink-0" />
        <span className="text-sm font-semibold tracking-wide" data-testid="mode-banner-label">
          {meta.label}
        </span>
        <span className="text-xs opacity-80">{meta.note}</span>
        {session?.trader_id ? (
          <span className="ml-auto font-mono text-xs opacity-70">
            {session.trader_id}
          </span>
        ) : null}
      </div>

      {halted && session?.halt ? (
        <div
          className="flex items-start gap-2 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-2.5 text-sm text-destructive"
          data-testid="mode-banner-halt"
        >
          <AlertOctagon className="mt-0.5 size-4 shrink-0" />
          <span>
            <span className="font-semibold capitalize">{session.halt.kind}</span>{" "}
            HALT — {session.halt.reason || "no reason given"}. New opening orders
            are blocked; existing positions stay open. FLATTEN is still allowed.
          </span>
        </div>
      ) : null}
    </div>
  );
}
