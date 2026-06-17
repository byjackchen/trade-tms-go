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
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { SyncStatusBadge } from "./status-badge";
import { useSyncRuns } from "@/lib/api/hooks";
import { formatInt, formatTs, formatDuration, formatRelative } from "@/lib/format";
import type { DatasetWatermark, SyncRun } from "@/lib/api/types";

const WATERMARK_COLUMNS: ColumnDef<DatasetWatermark>[] = [
  {
    key: "dataset",
    header: "Dataset",
    primary: true,
    render: (d) => (
      <span className="font-mono text-xs" data-testid="sync-dataset">
        {d.dataset}
      </span>
    ),
  },
  {
    key: "rows",
    header: "Rows",
    align: "right",
    render: (d) => (
      <span className="tabular-nums">{formatInt(d.row_count)}</span>
    ),
  },
  {
    key: "last-sync",
    header: "Last sync",
    primary: true,
    render: (d) => (
      <span
        className="text-xs text-muted-foreground"
        data-testid="sync-last"
        title={d.last_sync ? formatTs(d.last_sync) : "never"}
      >
        {d.last_sync ? formatRelative(d.last_sync) : "never"}
      </span>
    ),
  },
  {
    key: "schema",
    header: "Schema",
    render: (d) => <Badge variant="outline">v{d.schema_version}</Badge>,
  },
];

const RUN_COLUMNS: ColumnDef<SyncRun>[] = [
  {
    key: "id",
    header: "#",
    labelMobile: "Run",
    render: (r) => (
      <span className="tabular-nums text-muted-foreground">{r.id}</span>
    ),
  },
  {
    key: "dataset",
    header: "Dataset",
    primary: true,
    render: (r) => <span className="font-mono text-xs">{r.dataset}</span>,
  },
  {
    key: "kind",
    header: "Kind",
    render: (r) => <span className="text-xs">{r.kind}</span>,
  },
  {
    key: "started",
    header: "Started",
    render: (r) => (
      <span
        className="text-xs text-muted-foreground"
        title={formatTs(r.started_at)}
      >
        {formatRelative(r.started_at)}
      </span>
    ),
  },
  {
    key: "duration",
    header: "Duration",
    render: (r) => (
      <span className="text-xs tabular-nums">
        {formatDuration(r.started_at, r.finished_at)}
      </span>
    ),
  },
  {
    key: "rows-added",
    header: "Rows +",
    align: "right",
    render: (r) => (
      <span className="tabular-nums">{formatInt(r.rows_added)}</span>
    ),
  },
  {
    key: "status",
    header: "Status",
    primary: true,
    render: (r) => (
      <div className="flex flex-col items-end gap-0.5 md:items-start">
        <SyncStatusBadge status={r.status} />
        {r.error ? (
          <span
            className="max-w-[18rem] truncate text-[11px] text-destructive"
            title={r.error}
            data-testid="sync-run-error"
          >
            {r.error}
          </span>
        ) : null}
      </div>
    ),
  },
];

export function SyncRunsTable() {
  const { data, isLoading, error, refetch } = useSyncRuns();

  return (
    <Card data-testid="sync-runs-card">
      <CardHeader className="flex-col items-start gap-1">
        <CardTitle>Dataset sync</CardTitle>
        <CardDescription>
          Per-dataset watermarks and recent import run history.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        {isLoading ? (
          <LoadingRows rows={4} data-testid="sync-loading" />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="sync-error"
          />
        ) : !data ? (
          <EmptyState title="No sync data" data-testid="sync-empty" />
        ) : (
          <>
            <div>
              <div className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Watermarks
              </div>
              {data.datasets.length === 0 ? (
                <EmptyState
                  title="No datasets synced yet"
                  data-testid="sync-watermarks-empty"
                />
              ) : (
                <ResponsiveTable
                  columns={WATERMARK_COLUMNS}
                  rows={data.datasets}
                  rowKey={(d) => d.dataset}
                  rowTestId={(d) => `sync-watermark-${d.dataset}`}
                  data-testid="sync-watermarks-table"
                />
              )}
            </div>

            <div>
              <div className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Run history
              </div>
              {data.runs.length === 0 ? (
                <EmptyState
                  title="No sync runs recorded"
                  hint="Import runs appear here once a refresh completes."
                  data-testid="sync-runs-empty"
                />
              ) : (
                <ResponsiveTable
                  columns={RUN_COLUMNS}
                  rows={data.runs}
                  rowKey={(r) => r.id}
                  rowTestId={(r) => `sync-run-${r.id}`}
                  data-testid="sync-runs-table"
                />
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
