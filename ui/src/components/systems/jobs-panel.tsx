"use client";

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
import { Button } from "@/components/ui/button";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { JobStatusBadge } from "./status-badge";
import { useJobs, useCancelJob } from "@/lib/api/hooks";
import { formatRelative, formatTs } from "@/lib/format";
import type { Job } from "@/lib/api/types";

const ACTIVE = new Set(["queued", "running"]);

type CancelMutation = ReturnType<typeof useCancelJob>;

function buildColumns(cancel: CancelMutation): ColumnDef<Job>[] {
  return [
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
      render: (job) => <JobStatusBadge status={job.status} />,
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
    {
      key: "action",
      header: "Action",
      align: "right",
      render: (job) =>
        ACTIVE.has(job.status) ? (
          <Button
            size="sm"
            variant="outline"
            disabled={cancel.isPending}
            onClick={(e) => {
              e.stopPropagation();
              cancel.mutate({ id: job.id, actor: "ui" });
            }}
            data-testid={`job-cancel-${job.id}`}
          >
            Cancel
          </Button>
        ) : (
          <span className="text-muted-foreground">—</span>
        ),
    },
  ];
}

export function JobsPanel() {
  const { data, isLoading, error, refetch } = useJobs();
  const cancel = useCancelJob();

  return (
    <Card data-testid="jobs-card">
      <CardHeader className="flex-col items-start gap-1">
        <CardTitle>Recent jobs</CardTitle>
        <CardDescription>
          Data refresh and universe rebuild jobs, newest first.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={4} data-testid="jobs-loading" />
        ) : error ? (
          <ErrorState error={error} onRetry={() => refetch()} data-testid="jobs-error" />
        ) : !data || data.jobs.length === 0 ? (
          <EmptyState
            title="No jobs yet"
            hint="Trigger a refresh or rebuild to see jobs here."
            data-testid="jobs-empty"
          />
        ) : (
          <ResponsiveTable
            columns={buildColumns(cancel)}
            rows={data.jobs.slice(0, 12)}
            rowKey={(job) => job.id}
            rowTestId={(job) => `job-row-${job.id}`}
            data-testid="jobs-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
