import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import type { LiveMode, LiveStatus, LiveOrderStatus } from "@/lib/api/types";

/** Session lifecycle badge: RUNNING green, STOPPED muted, CRASHED destructive. */
export function SessionStatusBadge({
  status,
  "data-testid": testId = "session-status-badge",
}: {
  status: LiveStatus;
  "data-testid"?: string;
}) {
  const v =
    status === "RUNNING"
      ? "success"
      : status === "CRASHED"
        ? "destructive"
        : "muted";
  return (
    <Badge variant={v} data-testid={testId} data-status={status}>
      {status}
    </Badge>
  );
}

/**
 * Derive the paper/live/signal label from the 2D session model that REPLACED the
 * legacy three-valued `mode` (docs/concept-alignment.md §1.3, C6): `signal`
 * exec-policy is always "signal" (emit-only); `auto` resolves by the bound
 * account's env — `real` → "live" (real money), `sim`/`simulate` → "paper".
 */
export function sessionModeLabel(
  execPolicy: string | undefined,
  accountEnv: string | undefined,
): LiveMode {
  if (execPolicy !== "auto") return "signal";
  return accountEnv === "real" ? "live" : "paper";
}

/** Trading-mode badge. signal is the only enabled mode in P5; paper/live P6. */
export function ModeBadge({
  mode,
  "data-testid": testId = "mode-badge",
}: {
  mode: LiveMode;
  "data-testid"?: string;
}) {
  const v =
    mode === "live" ? "destructive" : mode === "paper" ? "warning" : "secondary";
  return (
    <Badge variant={v} data-testid={testId} data-mode={mode}>
      {String(mode).toUpperCase()}
    </Badge>
  );
}

/**
 * Per-strategy decision-state badge (buy / forming / hold / exit / flat / …).
 * Unknown states render neutral so a new strategy token never breaks the table.
 */
export function IntentStateBadge({
  state,
  "data-testid": testId = "intent-state-badge",
}: {
  state: string;
  "data-testid"?: string;
}) {
  const s = state.toLowerCase();
  const v =
    s === "buy" || s === "long" || s === "enter"
      ? "success"
      : s === "forming" || s === "watch"
        ? "warning"
        : s === "exit" || s === "sell" || s === "short"
          ? "destructive"
          : "muted";
  return (
    <Badge variant={v} data-testid={testId} data-state={s}>
      {state}
    </Badge>
  );
}

/**
 * Order lifecycle badge (the moomoo→domain state machine, ADR-004). Terminal
 * good = FILLED (green); in-flight = SUBMITTED/ACCEPTED/WORKING/PARTIALLY_FILLED
 * (muted/warning); terminal bad = REJECTED/EXPIRED (destructive) and CANCELED
 * (muted). Unknown states render neutral so a new status never breaks the
 * blotter.
 */
export function OrderStatusBadge({
  status,
  "data-testid": testId = "order-status-badge",
}: {
  status: LiveOrderStatus;
  "data-testid"?: string;
}) {
  const s = String(status).toUpperCase();
  const v =
    s === "FILLED"
      ? "success"
      : s === "PARTIALLY_FILLED"
        ? "warning"
        : s === "REJECTED" || s === "EXPIRED"
          ? "destructive"
          : s === "SUBMITTED" || s === "ACCEPTED" || s === "WORKING"
            ? "secondary"
            : "muted";
  return (
    <Badge variant={v} data-testid={testId} data-status={s}>
      {s.replace(/_/g, " ")}
    </Badge>
  );
}

/** Buy/sell (or long/short) side badge — green for buy/long, red for sell/short. */
export function SideBadge({
  side,
  "data-testid": testId = "side-badge",
}: {
  side: string;
  "data-testid"?: string;
}) {
  const s = side.toLowerCase();
  const buy = s === "buy" || s === "long" || s === "b";
  return (
    <Badge
      variant={buy ? "success" : "destructive"}
      data-testid={testId}
      data-side={s}
    >
      {side.toUpperCase()}
    </Badge>
  );
}

const DOT: Record<string, string> = {
  green: "bg-emerald-500",
  yellow: "bg-amber-500",
  red: "bg-red-500",
  gray: "bg-zinc-400",
};

/** A small colored status dot with an optional pulse (used for connection/freshness). */
export function StatusDot({
  color,
  pulse,
  className,
}: {
  color: "green" | "yellow" | "red" | "gray";
  pulse?: boolean;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-block size-2.5 rounded-full",
        DOT[color],
        pulse && "animate-pulse",
        className,
      )}
    />
  );
}
