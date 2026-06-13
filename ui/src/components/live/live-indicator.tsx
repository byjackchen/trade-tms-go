"use client";

import { useLiveStream } from "@/lib/api/use-live-stream";
import { cn } from "@/lib/utils";

const DOT: Record<string, string> = {
  open: "bg-emerald-500",
  connecting: "bg-amber-500",
  error: "bg-red-500",
  closed: "bg-zinc-400",
};

const LABEL: Record<string, string> = {
  open: "live",
  connecting: "connecting",
  error: "offline",
  closed: "closed",
};

/**
 * Header pill reflecting the live WS/SSE bridge connection state. Subscribes to
 * the shared SSE bus (ref-counted) so mounting it alongside other live panels
 * costs no extra upstream connection.
 */
export function LiveIndicator() {
  const { state } = useLiveStream({});
  return (
    <div
      className="flex items-center gap-1.5 rounded-full border border-border bg-card px-2.5 py-1 text-xs"
      data-testid="live-indicator"
      data-state={state}
    >
      <span
        className={cn(
          "inline-block size-2 rounded-full",
          DOT[state] ?? "bg-zinc-400",
          state === "connecting" && "animate-pulse",
        )}
      />
      <span className="text-muted-foreground">{LABEL[state] ?? state}</span>
    </div>
  );
}
