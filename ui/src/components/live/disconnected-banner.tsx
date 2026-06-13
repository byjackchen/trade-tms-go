import { WifiOff } from "lucide-react";
import type { BridgeState } from "@/lib/api/sse-bus";

/**
 * A thin banner shown when the live event bridge is not currently open. The
 * panels keep rendering their last PG-hydrated snapshot (the durable truth), so
 * this only signals that real-time deltas are paused — not that data is gone.
 */
export function DisconnectedBanner({ state }: { state: BridgeState }) {
  if (state === "open") return null;
  const label =
    state === "connecting"
      ? "Reconnecting to the live stream… showing last known state."
      : "Live stream disconnected — showing last known state. Reconnecting automatically.";
  return (
    <div
      data-testid="live-disconnected-banner"
      data-state={state}
      className="flex items-center gap-2 rounded-lg border border-amber-500/40 bg-amber-500/5 px-3 py-2 text-xs text-amber-600 dark:text-amber-400"
    >
      <WifiOff className="size-3.5" />
      <span>{label}</span>
    </div>
  );
}
