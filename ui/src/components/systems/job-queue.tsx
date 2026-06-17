"use client";

import { useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Select } from "@/components/ui/select";
import { Input } from "@/components/ui/input";
import { Progress } from "@/components/ui/progress";
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { JobStatusBadge } from "@/components/systems/status-badge";
import { StreamIndicator } from "@/components/systems/stream-indicator";
import { useOpsJobs } from "@/lib/api/hooks";
import { useJobStream } from "@/lib/api/use-job-stream";
import { formatRelative, formatTs } from "@/lib/format";
import { jobPct } from "./job-progress-util";
import { JobDrawer } from "./job-drawer";
import type { Job, JobStatus } from "@/lib/api/types";

const STATUSES: JobStatus[] = [
  "queued",
  "running",
  "succeeded",
  "failed",
  "canceled",
];

/** Inline progress cell shared by the desktop row and the mobile card. */
function ProgressCell({ job }: { job: Job }) {
  const pct = jobPct(job);
  const active = job.status === "queued" || job.status === "running";
  if (active) {
    return (
      <div className="flex items-center justify-end gap-2 md:justify-start">
        <Progress
          className="h-1.5 w-16"
          value={pct}
          indeterminate={job.status === "running" && pct == null}
          data-testid={`job-queue-progress-${job.id}`}
        />
        <span className="text-[11px] tabular-nums text-muted-foreground">
          {pct != null ? `${pct}%` : "—"}
        </span>
      </div>
    );
  }
  if (job.status === "failed") {
    return <Badge variant="destructive">error</Badge>;
  }
  return <span className="text-muted-foreground">—</span>;
}

const JOB_QUEUE_COLUMNS: ColumnDef<Job>[] = [
  {
    key: "id",
    header: "#",
    labelMobile: "Job",
    render: (job) => (
      <span className="tabular-nums text-muted-foreground">{job.id}</span>
    ),
  },
  {
    key: "kind",
    header: "Kind",
    primary: true,
    render: (job) => <span className="font-mono text-xs">{job.kind}</span>,
  },
  {
    key: "status",
    header: "Status",
    primary: true,
    render: (job) => (
      <JobStatusBadge
        status={job.status}
        data-testid={`job-queue-status-${job.id}`}
      />
    ),
  },
  {
    key: "progress",
    header: "Progress",
    primary: true,
    className: "w-32",
    render: (job) => <ProgressCell job={job} />,
  },
  {
    key: "worker",
    header: "Worker",
    render: (job) => (
      <span className="font-mono text-[11px] text-muted-foreground">
        {job.claimed_by ?? "—"}
      </span>
    ),
  },
  {
    key: "attempts",
    header: "Attempts",
    render: (job) => (
      <span className="tabular-nums text-xs text-muted-foreground">
        {job.attempts}/{job.max_attempts}
      </span>
    ),
  },
  {
    key: "updated",
    header: "Updated",
    render: (job) => (
      <span
        className="text-xs text-muted-foreground"
        title={formatTs(job.updated_at)}
      >
        {formatRelative(job.updated_at)}
      </span>
    ),
  },
];

/** The live JOB QUEUE: a filterable, self-refreshing table of ops.jobs with a
 * detail drawer and inline progress for in-flight jobs. */
export function JobQueue() {
  const [status, setStatus] = useState<string>("");
  const [kind, setKind] = useState<string>("");
  const [selected, setSelected] = useState<Job | null>(null);
  const qc = useQueryClient();

  // The status filter is applied server-side; the free-text kind filter is
  // applied client-side over the (unfiltered-by-kind) list so partial matches
  // work without round-trips.
  const { data, isLoading, error, refetch } = useOpsJobs({
    status: status || undefined,
  });

  // Live: every job event reconciles the queue (the SSE bridge gives sub-second
  // progress; the query's own poll is the reconnect backstop).
  useJobStream({
    onJobEvent: () => {
      void qc.invalidateQueries({ queryKey: ["ops", "jobs"] });
    },
  });

  const rows = useMemo(() => {
    const all = data?.jobs ?? [];
    const k = kind.trim().toLowerCase();
    return k ? all.filter((j) => j.kind.toLowerCase().includes(k)) : all;
  }, [data?.jobs, kind]);

  return (
    <>
      <Card data-testid="job-queue-card">
        <CardHeader>
          <div className="space-y-1">
            <CardTitle>Job queue</CardTitle>
            <CardDescription>
              Durable ops.jobs — newest first, live progress.
            </CardDescription>
          </div>
          <StreamIndicator />
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <Select
              className="min-w-0 flex-1 sm:w-40 sm:flex-none"
              value={status}
              onChange={(e) => setStatus(e.target.value)}
              data-testid="job-queue-status-filter"
              aria-label="Filter by status"
            >
              <option value="">All statuses</option>
              {STATUSES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </Select>
            <Input
              className="min-w-0 flex-1 sm:w-48 sm:flex-none"
              placeholder="Filter kind…"
              value={kind}
              onChange={(e) => setKind(e.target.value)}
              data-testid="job-queue-kind-filter"
              aria-label="Filter by kind"
            />
            <span
              className="ml-auto text-xs text-muted-foreground"
              data-testid="job-queue-count"
            >
              {rows.length} {rows.length === 1 ? "job" : "jobs"}
            </span>
          </div>

          {isLoading ? (
            <LoadingRows rows={6} data-testid="job-queue-loading" />
          ) : error ? (
            <ErrorState
              error={error}
              onRetry={() => refetch()}
              data-testid="job-queue-error"
            />
          ) : rows.length === 0 ? (
            <EmptyState
              title="No jobs"
              hint={
                status || kind
                  ? "No jobs match the current filters."
                  : "Enqueue a refresh, backtest or rebuild to populate the queue."
              }
              data-testid="job-queue-empty"
            />
          ) : (
            <div className="overflow-x-auto">
              <ResponsiveTable
                columns={JOB_QUEUE_COLUMNS}
                rows={rows}
                rowKey={(job) => job.id}
                rowTestId={(job) => `job-queue-row-${job.id}`}
                onRowClick={(job) => setSelected(job)}
                data-testid="job-queue-table"
              />
            </div>
          )}
        </CardContent>
      </Card>

      {selected ? (
        <JobDrawer
          jobId={selected.id}
          seed={selected}
          onClose={() => setSelected(null)}
        />
      ) : null}
    </>
  );
}
