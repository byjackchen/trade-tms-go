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
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { SyncStatusBadge } from "./status-badge";
import { useSyncRuns } from "@/lib/api/hooks";
import { formatInt, formatTs, formatDuration, formatRelative } from "@/lib/format";

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
                <Table data-testid="sync-watermarks-table">
                  <TableHeader>
                    <TableRow>
                      <TableHead>Dataset</TableHead>
                      <TableHead className="text-right">Rows</TableHead>
                      <TableHead>Last sync</TableHead>
                      <TableHead>Schema</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {data.datasets.map((d) => (
                      <TableRow
                        key={d.dataset}
                        data-testid={`sync-watermark-${d.dataset}`}
                      >
                        <TableCell className="font-mono text-xs" data-testid="sync-dataset">
                          {d.dataset}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatInt(d.row_count)}
                        </TableCell>
                        <TableCell
                          className="text-xs text-muted-foreground"
                          data-testid="sync-last"
                          title={d.last_sync ? formatTs(d.last_sync) : "never"}
                        >
                          {d.last_sync ? formatRelative(d.last_sync) : "never"}
                        </TableCell>
                        <TableCell>
                          <Badge variant="outline">v{d.schema_version}</Badge>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
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
                <Table data-testid="sync-runs-table">
                  <TableHeader>
                    <TableRow>
                      <TableHead>#</TableHead>
                      <TableHead>Dataset</TableHead>
                      <TableHead>Kind</TableHead>
                      <TableHead>Started</TableHead>
                      <TableHead>Duration</TableHead>
                      <TableHead className="text-right">Rows +</TableHead>
                      <TableHead>Status</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {data.runs.map((r) => (
                      <TableRow key={r.id} data-testid={`sync-run-${r.id}`}>
                        <TableCell className="tabular-nums text-muted-foreground">
                          {r.id}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {r.dataset}
                        </TableCell>
                        <TableCell className="text-xs">{r.kind}</TableCell>
                        <TableCell className="text-xs text-muted-foreground" title={formatTs(r.started_at)}>
                          {formatRelative(r.started_at)}
                        </TableCell>
                        <TableCell className="text-xs tabular-nums">
                          {formatDuration(r.started_at, r.finished_at)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatInt(r.rows_added)}
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-col gap-0.5">
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
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
