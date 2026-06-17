"use client";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { JobStatusBadge } from "./status-badge";
import { useJobs, useCancelJob } from "@/lib/api/hooks";
import { formatRelative, formatTs } from "@/lib/format";

const ACTIVE = new Set(["queued", "running"]);

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
          <Table data-testid="jobs-table">
            <TableHeader>
              <TableRow>
                <TableHead>#</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead className="text-right">Action</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.jobs.slice(0, 12).map((job) => (
                <TableRow key={job.id} data-testid={`job-row-${job.id}`}>
                  <TableCell className="tabular-nums text-muted-foreground">
                    {job.id}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{job.kind}</TableCell>
                  <TableCell>
                    <JobStatusBadge status={job.status} />
                  </TableCell>
                  <TableCell
                    className="text-xs text-muted-foreground"
                    title={formatTs(job.updated_at)}
                  >
                    {formatRelative(job.updated_at)}
                  </TableCell>
                  <TableCell className="text-right">
                    {ACTIVE.has(job.status) ? (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={cancel.isPending}
                        onClick={() => cancel.mutate({ id: job.id, actor: "ui" })}
                        data-testid={`job-cancel-${job.id}`}
                      >
                        Cancel
                      </Button>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
