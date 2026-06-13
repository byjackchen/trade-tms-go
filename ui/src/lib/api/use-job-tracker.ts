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
};

const TERMINAL: JobStatus[] = ["succeeded", "failed", "canceled"];

function lineFor(ev: JobEvent): string {
  switch (ev.event) {
    case "enqueued":
      return "job enqueued";
    case "claimed":
      return `claimed by ${ev.worker ?? "worker"}`;
    case "progress": {
      const stage = ev.progress?.stage ? String(ev.progress.stage) : "working";
      const pct =
        typeof ev.progress?.pct === "number" ? ` ${ev.progress.pct}%` : "";
      return `${stage}${pct}`;
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
      const pct =
        typeof ev.progress?.pct === "number"
          ? Math.max(0, Math.min(100, ev.progress.pct))
          : prev.pct;
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
          const pct =
            typeof job.progress?.pct === "number"
              ? job.progress.pct
              : prev.pct;
          // Only append a reconciliation line on a status transition.
          const statusChanged = prev.status !== job.status;
          return {
            ...prev,
            status: job.status,
            progress: job.progress ?? prev.progress,
            pct: job.status === "succeeded" ? 100 : pct,
            error: job.last_error ?? prev.error,
            done,
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

  return { tracked, track, reset };
}
