"use client";

import { AlertOctagon, Radio, FlaskConical, Banknote } from "lucide-react";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import type { ExecPolicy } from "@/lib/api/types";
import type { TradeEnv } from "./trade-env";

/**
 * The Portfolio view's loud, always-on EXEC + ENV banner. It reflects the new
 * 2D session model (docs/concept-alignment.md §1.3, C6): the ENVIRONMENT axis
 * (`env` — paper vs LIVE-REAL, from the bound account) drives the color, and the
 * EXECUTION axis (`exec_policy` — signal vs auto, from the live session) is shown
 * as a secondary note. The deleted three-valued `mode` enum is NOT consulted.
 *
 * Colors are deliberately distinct so an operator can never mistake LIVE-REAL for
 * PAPER at a glance:
 *   - paper → amber (simulated fills, no real money)
 *   - live  → red, full-width destructive (REAL money, real orders)
 * A halt overlays a destructive sub-banner regardless of env.
 */
const ENV_META: Record<
  TradeEnv,
  {
    label: string;
    Icon: typeof Radio;
    cls: string;
    note: string;
  }
> = {
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

/** Human note for the execution axis. */
function execNote(policy: ExecPolicy): { label: string; Icon: typeof Radio } {
  switch (String(policy)) {
    case "auto":
      return { label: "auto-submit", Icon: Banknote };
    case "signal":
    default:
      return { label: "signal-only", Icon: Radio };
  }
}

export function ExecBanner({ env }: { env: TradeEnv }) {
  const q = useLiveSession();
  const session = hasSession(q.data) ? q.data : null;
  const meta = ENV_META[env];
  const execPolicy: ExecPolicy = session?.exec_policy ?? "signal";
  const exec = execNote(execPolicy);
  const ExecIcon = exec.Icon;
  const halted = session?.halt != null;
  const Icon = meta.Icon;

  return (
    <div
      data-testid="live-mode-banner"
      data-env={env}
      data-exec-policy={execPolicy}
      data-halted={halted ? "true" : "false"}
      className="space-y-2"
    >
      <div
        className={`flex flex-wrap items-center gap-3 rounded-lg border px-4 py-2.5 ${meta.cls}`}
      >
        <Icon className="size-5 shrink-0" />
        <span
          className="text-sm font-semibold tracking-wide"
          data-testid="mode-banner-label"
        >
          {meta.label}
        </span>
        <span className="text-xs opacity-80">{meta.note}</span>
        <span
          className="inline-flex items-center gap-1 rounded-full border border-current/30 px-2 py-0.5 text-[11px] font-medium opacity-90"
          data-testid="exec-policy-badge"
          data-exec-policy={execPolicy}
        >
          <ExecIcon className="size-3" />
          {exec.label}
        </span>
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
