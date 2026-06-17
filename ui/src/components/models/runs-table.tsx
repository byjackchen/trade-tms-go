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
import { Select } from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { BacktestStatusBadge } from "./status-badge";
import { useBacktests } from "@/lib/api/hooks";
import { formatMoney, formatRelative, formatTs } from "@/lib/format";
import type { BacktestSummary } from "@/lib/api/types";

/** Total return ratio (final - start) / start; null when start is non-positive. */
function returnRatio(b: BacktestSummary): number | null {
  if (!b.starting_balance_usd) return null;
  return (b.final_balance_usd - b.starting_balance_usd) / b.starting_balance_usd;
}

function ReturnCell({ b }: { b: BacktestSummary }) {
  const r = returnRatio(b);
  if (r == null || b.status === "RUNNING") {
    return <span className="text-muted-foreground">—</span>;
  }
  const pct = r * 100;
  return (
    <span
      data-testid="backtest-return"
      className={
        pct > 0
          ? "text-emerald-600 dark:text-emerald-400"
          : pct < 0
            ? "text-destructive"
            : undefined
      }
    >
      {pct > 0 ? "+" : ""}
      {pct.toFixed(2)}%
    </span>
  );
}

const STATUS_OPTIONS = ["", "RUNNING", "COMPLETE", "INTERRUPTED", "FAIL"];

/**
 * Backtest runs list for the Models module. A row click selects a run for the
 * inline backtest panel — the standalone `/backtests/[id]` route is retired and
 * results render in place (docs/concept-alignment.md §3.4 ③). `selectedId`
 * highlights the open run.
 */
export function RunsTable({
  status,
  onStatusChange,
  selectedId,
  onSelect,
}: {
  status: string;
  onStatusChange: (s: string) => void;
  selectedId?: number | null;
  onSelect?: (id: number) => void;
}) {
  const { data, isLoading, error, refetch } = useBacktests({
    status: status || undefined,
  });

  const rows = data?.backtests ?? [];

  return (
    <Card data-testid="runs-card">
      <CardHeader className="flex-row items-start justify-between gap-2">
        <div className="space-y-1">
          <CardTitle>Backtest runs</CardTitle>
          <CardDescription>
            Persisted runs (DB source of truth), newest first.
          </CardDescription>
        </div>
        <Select
          value={status}
          onChange={(e) => onStatusChange(e.target.value)}
          className="h-7 w-40 text-xs"
          data-testid="runs-status-filter"
          aria-label="Filter by status"
        >
          {STATUS_OPTIONS.map((s) => (
            <option key={s || "all"} value={s}>
              {s || "All statuses"}
            </option>
          ))}
        </Select>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={5} data-testid="runs-loading" />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="runs-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No backtest runs yet"
            hint='Click "New backtest" to run one.'
            data-testid="runs-empty"
          />
        ) : (
          <Table data-testid="runs-table">
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Window</TableHead>
                <TableHead className="text-right">Start bal.</TableHead>
                <TableHead className="text-right">Final bal.</TableHead>
                <TableHead className="text-right">Return</TableHead>
                <TableHead className="text-right">P&amp;L</TableHead>
                <TableHead>Strategies</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((b) => {
                const isSelected = selectedId === b.id;
                return (
                <TableRow
                  key={b.id}
                  data-testid={`run-row-${b.id}`}
                  data-selected={isSelected ? "true" : "false"}
                  onClick={() => onSelect?.(b.id)}
                  className={cn("cursor-pointer", isSelected && "bg-accent/60")}
                >
                  <TableCell className="tabular-nums">
                    <button
                      type="button"
                      onClick={(e) => {
                        e.stopPropagation();
                        onSelect?.(b.id);
                      }}
                      className="font-medium text-primary underline-offset-2 hover:underline"
                      data-testid={`run-link-${b.id}`}
                    >
                      #{b.id}
                    </button>
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline">{b.kind}</Badge>
                  </TableCell>
                  <TableCell
                    className="text-xs text-muted-foreground"
                    data-testid="run-window"
                  >
                    {b.start_date} → {b.end_date}
                  </TableCell>
                  <TableCell className="text-right tabular-nums text-muted-foreground">
                    {formatMoney(b.starting_balance_usd)}
                  </TableCell>
                  <TableCell
                    className="text-right tabular-nums"
                    data-testid="run-final-balance"
                  >
                    {b.status === "RUNNING"
                      ? "—"
                      : formatMoney(b.final_balance_usd)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    <ReturnCell b={b} />
                  </TableCell>
                  <TableCell
                    className="text-right tabular-nums"
                    data-testid="run-pnl"
                  >
                    {b.status === "RUNNING" ? (
                      <span className="text-muted-foreground">—</span>
                    ) : (
                      <span
                        className={
                          b.total_pnl_usd > 0
                            ? "text-emerald-600 dark:text-emerald-400"
                            : b.total_pnl_usd < 0
                              ? "text-destructive"
                              : undefined
                        }
                      >
                        {formatMoney(b.total_pnl_usd)}
                      </span>
                    )}
                  </TableCell>
                  <TableCell
                    className="max-w-[14rem] truncate text-xs text-muted-foreground"
                    title={b.strategies.join(", ")}
                    data-testid="run-strategies"
                  >
                    {b.strategies.length ? b.strategies.join(", ") : "—"}
                  </TableCell>
                  <TableCell>
                    <BacktestStatusBadge
                      status={b.status}
                      data-testid={`run-status-${b.id}`}
                    />
                  </TableCell>
                  <TableCell
                    className="text-xs text-muted-foreground"
                    title={formatTs(b.created_at)}
                    data-testid="run-created"
                  >
                    {formatRelative(b.created_at)}
                  </TableCell>
                </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
