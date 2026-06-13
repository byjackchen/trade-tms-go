"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useJobStream } from "./use-job-stream";
import { apiGet } from "./client";
import type { Job, JobEvent, JobProgress, JobStatus } from "./types";

export type LogLine = {
  ts: string;
  event: JobEvent["event"];
  status: JobStatus;
  /** Human line synthesized from the event (stage/pct/error/worker). */
  text: string;
};

export type TrackedJob = {
  id: number;
  kind: string;
  status: JobStatus;
  progress: JobProgress | null;
  pct: number | null;
  log: LogLine[];
  error: string | null;
  done: boolean;
  /** The job's `result` object once observed via the REST reconciliation poll. */
  result: Record<string, unknown> | null;
};

const TERMINAL: JobStatus[] = ["succeeded", "failed", "canceled"];

/** Percent from a progress object, accepting both `pct` (data) and `percent` (backtest). */
function pctOf(p: JobProgress | null | undefined): number | null {
  if (!p) return null;
  if (typeof p.pct === "number") return p.pct;
  if (typeof p.percent === "number") return p.percent;
  return null;
}

function lineFor(ev: JobEvent): string {
  switch (ev.event) {
    case "enqueued":
      return "job enqueued";
    case "claimed":
      return `claimed by ${ev.worker ?? "worker"}`;
    case "progress": {
      const p = ev.progress ?? {};
      // Data jobs report {stage, pct}; backtests report {phase, percent,
      // bars_processed, bars_total}. Accept both.
      const stage = p.stage
        ? String(p.stage)
        : p.phase
          ? String(p.phase)
          : "working";
      const pctVal =
        typeof p.pct === "number"
          ? p.pct
          : typeof p.percent === "number"
            ? p.percent
            : null;
      const pct = pctVal != null ? ` ${Math.round(pctVal)}%` : "";
      const bars =
        typeof p.bars_processed === "number" && typeof p.bars_total === "number"
          ? ` (${p.bars_processed}/${p.bars_total} bars)`
          : "";
      return `${stage}${pct}${bars}`;
    }
    case "succeeded":
      return "completed successfully";
    case "failed":
      return `failed: ${ev.error ?? "unknown error"}`;
    case "canceled":
      return `canceled${ev.error ? `: ${ev.error}` : ""}`;
    case "cancel_requested":
      return "cancel requested";
    case "requeued":
      return "requeued for retry";
    case "released":
      return "claim released";
    case "reaped":
      return "stale claim reaped";
    default:
      return ev.event;
  }
}

/**
 * Track a single in-flight job by id, merging the live SSE job event stream
 * (progress + log lines + completion) with a REST reconciliation poll. The SSE
 * stream is best-effort; the poll guarantees we observe terminal state even if
 * the relevant frame was dropped (e.g. queue overflow / reconnect gap).
 */
export function useJobTracker(): {
  tracked: TrackedJob | null;
  track: (job: Pick<Job, "id" | "kind" | "status">) => void;
  reset: () => void;
} {
  const [tracked, setTracked] = useState<TrackedJob | null>(null);
  const idRef = useRef<number | null>(null);

  const track = useCallback(
    (job: Pick<Job, "id" | "kind" | "status">) => {
      idRef.current = job.id;
      setTracked({
        id: job.id,
        kind: job.kind,
        status: job.status,
        progress: null,
        pct: null,
        error: null,
        done: TERMINAL.includes(job.status),
        result: null,
        log: [
          {
            ts: new Date().toISOString(),
            event: "enqueued",
            status: job.status,
            text: `tracking job #${job.id} (${job.kind})`,
          },
        ],
      });
    },
    [],
  );

  const reset = useCallback(() => {
    idRef.current = null;
    setTracked(null);
  }, []);

  // Apply a job event to the tracked job (ignoring events for other jobs).
  const onJobEvent = useCallback((ev: JobEvent) => {
    if (idRef.current == null || ev.job_id !== idRef.current) return;
    setTracked((prev) => {
      if (!prev || prev.id !== ev.job_id) return prev;
      const rawPct = pctOf(ev.progress);
      const pct =
        rawPct != null ? Math.max(0, Math.min(100, rawPct)) : prev.pct;
      const done = TERMINAL.includes(ev.status);
      return {
        ...prev,
        status: ev.status,
        progress: ev.progress ?? prev.progress,
        pct: done && ev.status === "succeeded" ? 100 : pct,
        error: ev.error ?? prev.error,
        done,
        log: [
          ...prev.log,
          {
            ts: ev.ts ?? new Date().toISOString(),
            event: ev.event,
            status: ev.status,
            text: lineFor(ev),
          },
        ].slice(-200),
      };
    });
  }, []);

  useJobStream({ onJobEvent });

  // REST reconciliation: while tracking a non-terminal job, poll its row so we
  // converge on terminal state even when SSE frames are missed.
  useEffect(() => {
    if (!tracked || tracked.done) return;
    let cancelled = false;
    const tick = async () => {
      const id = idRef.current;
      if (id == null) return;
      try {
        const resp = await apiGet<{ job?: Job }>(`jobs/${id}`);
        const job = resp?.job;
        if (cancelled || !job || typeof job.id !== "number") return;
        setTracked((prev) => {
          if (!prev || prev.id !== job.id) return prev;
          const done = TERMINAL.includes(job.status);
          const pollPct = pctOf(job.progress);
          const pct = pollPct != null ? pollPct : prev.pct;
          // Only append a reconciliation line on a status transition.
          const statusChanged = prev.status !== job.status;
          return {
            ...prev,
            status: job.status,
            progress: job.progress ?? prev.progress,
            pct: job.status === "succeeded" ? 100 : pct,
            error: job.last_error ?? prev.error,
            done,
            result: job.result ?? prev.result,
            log: statusChanged
              ? [
                  ...prev.log,
                  {
                    ts: job.updated_at,
                    event: "progress" as const,
                    status: job.status,
                    text: `status → ${job.status}`,
                  } satisfies LogLine,
                ].slice(-200)
              : prev.log,
          };
        });
      } catch {
        // Transient proxy/API hiccup — the next tick retries.
      }
    };
    const interval = setInterval(tick, 4000);
    void tick();
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
    // Intentionally keyed on id + done only: re-running on every log append
    // (which mutates `tracked`) would churn the poll timer. The poll reads the
    // live id via idRef, so a stale `tracked` closure is not a correctness risk.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tracked?.id, tracked?.done]);

  // On terminal completion, do one final REST read to backfill the job `result`
  // object (e.g. backtest run_id). The live poll above stops once `done` flips
  // via SSE, which can race ahead of capturing the result payload.
  const doneId =
    tracked?.done && tracked.result == null ? tracked.id : null;
  useEffect(() => {
    if (doneId == null) return;
    let cancelled = false;
    void (async () => {
      try {
        const resp = await apiGet<{ job?: Job }>(`jobs/${doneId}`);
        const job = resp?.job;
        if (cancelled || !job || typeof job.id !== "number") return;
        setTracked((prev) => {
          if (!prev || prev.id !== job.id || prev.result != null) return prev;
          return { ...prev, result: job.result ?? null };
        });
      } catch {
        /* best-effort */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [doneId]);

  return { tracked, track, reset };
}
