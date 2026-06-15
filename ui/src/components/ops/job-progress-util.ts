import type { Job } from "@/lib/api/types";

/**
 * Extract a 0..100 percentage from a job's free-form `progress` object. The
 * worker writes different keys per handler (`pct`, `percent`); we accept either
 * and clamp. Returns null when no numeric progress is present (the bar then
 * renders indeterminate while running).
 */
export function jobPct(job: Pick<Job, "progress">): number | null {
  const p = job.progress;
  if (!p) return null;
  const raw = (p.pct ?? p.percent) as unknown;
  if (typeof raw !== "number" || Number.isNaN(raw)) return null;
  return Math.max(0, Math.min(100, raw));
}

/** A short human label for the current progress stage, if the worker set one. */
export function jobStage(job: Pick<Job, "progress">): string | null {
  const p = job.progress;
  if (!p) return null;
  const stage = (p.stage ?? p.phase) as unknown;
  return typeof stage === "string" && stage !== "" ? stage : null;
}
