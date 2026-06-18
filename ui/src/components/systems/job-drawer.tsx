"use client";

import { useState } from "react";
import { X, Ban, RotateCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Progress } from "@/components/ui/progress";
import { JobStatusBadge } from "@/components/systems/status-badge";
import { ErrorState, LoadingRows } from "@/components/shell/states";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { cn } from "@/lib/utils";
import { useOpsJob, useCancelJob, useRetryJob } from "@/lib/api/hooks";
import { formatTs, formatRelative, formatDuration } from "@/lib/format";
import { jobPct, jobStage } from "./job-progress-util";
import type { Job } from "@/lib/api/types";

const ACTIVE = new Set(["queued", "running"]);
const RETRYABLE = new Set(["failed", "canceled"]);

/** A labeled key/value field row in the drawer. */
function Field({
  label,
  children,
  testid,
}: {
  label: string;
  children: React.ReactNode;
  testid?: string;
}) {
  return (
    <div className="flex items-start justify-between gap-4 py-1.5">
      <span className="shrink-0 text-xs text-muted-foreground">{label}</span>
      <span
        className="min-w-0 break-words text-right text-xs font-medium"
        data-testid={testid}
      >
        {children}
      </span>
    </div>
  );
}

/** Pretty-print a JSON blob for the payload / result / progress sections. */
function JsonBlock({
  value,
  testid,
}: {
  value: Record<string, unknown> | undefined;
  testid: string;
}) {
  if (!value || Object.keys(value).length === 0) {
    return (
      <span className="text-xs text-muted-foreground" data-testid={`${testid}-empty`}>
        —
      </span>
    );
  }
  return (
    <pre
      data-testid={testid}
      className="cockpit-scroll max-h-40 overflow-auto rounded-lg border border-border bg-background/60 p-2 text-[11px] leading-relaxed"
    >
      {JSON.stringify(value, null, 2)}
    </pre>
  );
}

/**
 * Side drawer with the full detail of one job: status, progress, timing,
 * claimer, payload/result/error, plus cancel (queued/running) and retry
 * (failed/canceled) actions — each guarded behind a confirmation pop-up. The
 * detail self-refreshes while the job is active.
 *
 * `seed` is the row the user clicked (so the drawer paints instantly); the live
 * GET /api/v1/jobs/{id} reconciles on top.
 */
export function JobDrawer({
  jobId,
  seed,
  onClose,
}: {
  jobId: number;
  seed?: Job;
  onClose: () => void;
}) {
  const detail = useOpsJob(jobId);
  const cancel = useCancelJob();
  const retry = useRetryJob();
  const { mode } = useUiMode();
  const bottom = mode === "mobile";

  // Cancel/Retry are guarded behind a confirmation pop-up — the footer button
  // only arms the dialog; the actual mutation fires from `runConfirm`.
  const [confirm, setConfirm] = useState<null | "cancel" | "retry">(null);
  const pending = cancel.isPending || retry.isPending;
  const runConfirm = () => {
    const onSettled = () => setConfirm(null);
    if (confirm === "cancel") cancel.mutate({ id: jobId, actor: "ui" }, { onSettled });
    else if (confirm === "retry") retry.mutate({ id: jobId, actor: "ui" }, { onSettled });
  };

  const job = detail.data?.job ?? seed;
  const pct = job ? jobPct(job) : null;
  const stage = job ? jobStage(job) : null;
  const active = job ? ACTIVE.has(job.status) : false;
  const retryable = job ? RETRYABLE.has(job.status) : false;

  return (
    <>
      {/* Backdrop */}
      <button
        type="button"
        aria-label="Close job detail"
        className="fixed inset-0 z-40 bg-black/40"
        onClick={onClose}
        data-testid="job-drawer-backdrop"
      />
      <aside
        className={cn(
          "fixed z-50 flex flex-col bg-card shadow-xl",
          bottom
            ? // Mobile: bottom sheet — full width, slides up, rounded top.
              "inset-x-0 bottom-0 max-h-[90vh] rounded-t-2xl border-t border-border sheet-slide-up"
            : // Desktop: right-side drawer (unchanged).
              "inset-y-0 right-0 w-[min(28rem,calc(100vw-2rem))] border-l border-border",
        )}
        data-testid="job-drawer"
        data-job-id={jobId}
        data-side={bottom ? "bottom" : "right"}
        role="dialog"
        aria-label={`Job ${jobId} detail`}
      >
        {bottom ? (
          <div className="flex justify-center pt-2.5">
            <span aria-hidden className="h-1 w-9 rounded-full bg-foreground/20" />
          </div>
        ) : null}
        <header className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
          <div className="flex items-center gap-2">
            <span className="font-mono text-sm">job #{jobId}</span>
            {job ? <JobStatusBadge status={job.status} data-testid="job-drawer-status" /> : null}
          </div>
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={onClose}
            aria-label="Close"
            data-testid="job-drawer-close"
          >
            <X />
          </Button>
        </header>

        <div className="cockpit-scroll flex-1 space-y-4 overflow-y-auto p-4">
          {detail.isLoading && !seed ? (
            <LoadingRows rows={6} data-testid="job-drawer-loading" />
          ) : detail.error && !seed ? (
            <ErrorState
              error={detail.error}
              onRetry={() => detail.refetch()}
              data-testid="job-drawer-error"
            />
          ) : job ? (
            <>
              {active ? (
                <div className="space-y-1" data-testid="job-drawer-progress">
                  <div className="flex items-center justify-between text-xs">
                    <span className="text-muted-foreground">
                      {stage ?? (job.status === "queued" ? "queued" : "running")}
                    </span>
                    {pct != null ? (
                      <span className="tabular-nums" data-testid="job-drawer-pct">
                        {pct}%
                      </span>
                    ) : null}
                  </div>
                  <Progress
                    value={pct}
                    indeterminate={job.status === "running" && pct == null}
                    data-testid="job-drawer-progress-bar"
                  />
                </div>
              ) : null}

              <section className="rounded-lg border border-border px-3 py-2">
                <Field label="Kind">
                  <span className="font-mono">{job.kind}</span>
                </Field>
                <Field label="Priority" testid="job-drawer-priority">
                  {job.priority}
                </Field>
                <Field label="Attempts" testid="job-drawer-attempts">
                  {job.attempts} / {job.max_attempts}
                </Field>
                <Field label="Dedupe key">
                  {job.dedupe_key ? (
                    <span className="font-mono">{job.dedupe_key}</span>
                  ) : (
                    "—"
                  )}
                </Field>
                <Field label="Claimed by" testid="job-drawer-claimed-by">
                  {job.claimed_by ? (
                    <span className="font-mono">{job.claimed_by}</span>
                  ) : (
                    "—"
                  )}
                </Field>
                {job.cancel_requested ? (
                  <Field label="Cancel requested" testid="job-drawer-cancel-requested">
                    yes
                  </Field>
                ) : null}
              </section>

              <section className="rounded-lg border border-border px-3 py-2">
                <Field label="Created" testid="job-drawer-created">
                  <span title={formatTs(job.created_at)}>
                    {formatRelative(job.created_at)}
                  </span>
                </Field>
                <Field label="Started">
                  {job.started_at ? (
                    <span title={formatTs(job.started_at)}>
                      {formatRelative(job.started_at)}
                    </span>
                  ) : (
                    "—"
                  )}
                </Field>
                <Field label="Finished" testid="job-drawer-finished">
                  {job.finished_at ? (
                    <span title={formatTs(job.finished_at)}>
                      {formatRelative(job.finished_at)}
                    </span>
                  ) : (
                    "—"
                  )}
                </Field>
                <Field label="Duration">
                  {formatDuration(job.started_at, job.finished_at)}
                </Field>
                <Field label="Heartbeat">
                  {job.heartbeat_at ? (
                    <span title={formatTs(job.heartbeat_at)}>
                      {formatRelative(job.heartbeat_at)}
                    </span>
                  ) : (
                    "—"
                  )}
                </Field>
              </section>

              {job.last_error ? (
                <section className="space-y-1">
                  <div className="text-xs font-medium text-muted-foreground">
                    Error
                  </div>
                  <p
                    className="break-words rounded-lg border border-destructive/30 bg-destructive/10 p-2 text-xs text-destructive"
                    data-testid="job-drawer-last-error"
                  >
                    {job.last_error}
                  </p>
                </section>
              ) : null}

              <section className="space-y-1">
                <div className="text-xs font-medium text-muted-foreground">
                  Payload
                </div>
                <JsonBlock value={job.payload} testid="job-drawer-payload" />
              </section>

              {job.progress && Object.keys(job.progress).length > 0 ? (
                <section className="space-y-1">
                  <div className="text-xs font-medium text-muted-foreground">
                    Progress
                  </div>
                  <JsonBlock value={job.progress} testid="job-drawer-progress-json" />
                </section>
              ) : null}

              {job.result && Object.keys(job.result).length > 0 ? (
                <section className="space-y-1">
                  <div className="text-xs font-medium text-muted-foreground">
                    Result
                  </div>
                  <JsonBlock value={job.result} testid="job-drawer-result" />
                </section>
              ) : null}
            </>
          ) : null}
        </div>

        {job && (active || retryable) ? (
          <footer className="flex items-center justify-end gap-2 border-t border-border bg-muted/40 px-4 py-3">
            {active ? (
              <Button
                variant="destructive"
                size="sm"
                disabled={pending}
                onClick={() => setConfirm("cancel")}
                data-testid="job-drawer-cancel"
              >
                <Ban />
                {cancel.isPending ? "Canceling…" : "Cancel"}
              </Button>
            ) : null}
            {retryable ? (
              <Button
                variant="outline"
                size="sm"
                disabled={pending}
                onClick={() => setConfirm("retry")}
                data-testid="job-drawer-retry"
              >
                <RotateCw />
                {retry.isPending ? "Retrying…" : "Retry"}
              </Button>
            ) : null}
          </footer>
        ) : null}
      </aside>

      {/* Confirmation pop-up for the cancel/retry actions (native <dialog>, so it
          top-layers above the drawer). The mutation only fires on confirm. */}
      <Dialog
        open={confirm !== null}
        onClose={() => {
          if (!pending) setConfirm(null);
        }}
        title={confirm === "cancel" ? `Cancel job #${jobId}?` : `Retry job #${jobId}?`}
        description={
          confirm === "cancel"
            ? "The worker is asked to stop; in-progress work is discarded."
            : "A fresh attempt is re-queued from this job."
        }
        data-testid="job-confirm-dialog"
        footer={
          <>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setConfirm(null)}
              disabled={pending}
              data-testid="job-confirm-back"
            >
              Back
            </Button>
            <Button
              variant={confirm === "cancel" ? "destructive" : "default"}
              size="sm"
              onClick={runConfirm}
              disabled={pending}
              data-testid="job-confirm-ok"
            >
              {confirm === "cancel" ? <Ban /> : <RotateCw />}
              {confirm === "cancel"
                ? cancel.isPending
                  ? "Canceling…"
                  : "Cancel job"
                : retry.isPending
                  ? "Retrying…"
                  : "Retry job"}
            </Button>
          </>
        }
      >
        <p className="text-sm text-muted-foreground">
          <span className="font-mono">{job?.kind}</span>
        </p>
      </Dialog>
    </>
  );
}
