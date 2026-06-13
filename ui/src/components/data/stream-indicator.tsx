"use client";

import { useJobStream } from "@/lib/api/use-job-stream";
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
 * Header pill reflecting the live event-stream (SSE bridge) connection state.
 * The bridge is mounted here once so job progress can flow even before a dialog
 * opens; the underlying EventSource is shared by `useJobStream` consumers.
 */
export function StreamIndicator() {
  const { state } = useJobStream({});
  return (
    <div
      className="flex items-center gap-1.5 rounded-full border border-border bg-card px-2.5 py-1 text-xs"
      data-testid="stream-indicator"
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
