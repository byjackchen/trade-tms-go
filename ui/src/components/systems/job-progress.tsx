"use client";

import { useEffect, useRef } from "react";
import { CheckCircle2, XCircle, Loader2, Ban } from "lucide-react";
import { Progress } from "@/components/ui/progress";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { JobStatusBadge } from "./status-badge";
import type { TrackedJob } from "@/lib/api/use-job-tracker";

/**
 * Live job progress view: a progress bar (determinate when the worker reports
 * pct, indeterminate otherwise) plus a streaming log of job events, and a
 * terminal completion state. Used by both the data-refresh and universe-rebuild
 * flows.
 */
export function JobProgress({
  tracked,
  onCancel,
  canceling,
}: {
  tracked: TrackedJob;
  onCancel?: () => void;
  canceling?: boolean;
}) {
  const logRef = useRef<HTMLDivElement>(null);

  // Keep the log scrolled to the newest line as events stream in.
  useEffect(() => {
    const el = logRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [tracked.log.length]);

  const running = tracked.status === "running" || tracked.status === "queued";

  return (
    <div className="space-y-3" data-testid="job-progress" data-job-id={tracked.id}>
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="font-mono text-xs text-muted-foreground">
            job #{tracked.id}
          </span>
          <JobStatusBadge status={tracked.status} data-testid="job-progress-status" />
          {tracked.pct != null ? (
            <Badge variant="muted" data-testid="job-progress-pct">
              {tracked.pct}%
            </Badge>
          ) : null}
        </div>
        {running && onCancel ? (
          <Button
            variant="destructive"
            size="sm"
            onClick={onCancel}
            disabled={canceling}
            data-testid="job-cancel"
          >
            <Ban />
            {canceling ? "Canceling…" : "Cancel"}
          </Button>
        ) : null}
      </div>

      <Progress
        value={tracked.pct}
        indeterminate={running && tracked.pct == null}
        data-testid="job-progress-bar"
        barClassName={
          tracked.status === "failed"
            ? "bg-destructive"
            : tracked.status === "canceled"
              ? "bg-amber-500"
              : undefined
        }
      />

      <div
        ref={logRef}
        data-testid="job-log"
        className="cockpit-scroll max-h-44 overflow-y-auto rounded-lg border border-border bg-background/60 p-2 font-mono text-[11px] leading-relaxed"
      >
        {tracked.log.map((line, i) => (
          <div
            key={i}
            data-testid="job-log-line"
            className="flex gap-2 whitespace-pre-wrap break-words"
          >
            <span className="shrink-0 text-muted-foreground">
              {line.ts.slice(11, 19)}
            </span>
            <span
              className={
                line.event === "failed"
                  ? "text-destructive"
                  : line.event === "succeeded"
                    ? "text-emerald-500"
                    : "text-foreground"
              }
            >
              {line.text}
            </span>
          </div>
        ))}
      </div>

      {tracked.done ? (
        <div
          className="flex items-center gap-2 text-sm"
          data-testid="job-complete"
          data-outcome={tracked.status}
        >
          {tracked.status === "succeeded" ? (
            <>
              <CheckCircle2 className="size-4 text-emerald-500" />
              <span>Completed successfully.</span>
            </>
          ) : tracked.status === "failed" ? (
            <>
              <XCircle className="size-4 text-destructive" />
              <span className="text-destructive" data-testid="job-error-text">
                {tracked.error ?? "Job failed."}
              </span>
            </>
          ) : (
            <>
              <Ban className="size-4 text-amber-500" />
              <span>Canceled.</span>
            </>
          )}
        </div>
      ) : (
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" />
          <span>Working… progress streams live.</span>
        </div>
      )}
    </div>
  );
}
