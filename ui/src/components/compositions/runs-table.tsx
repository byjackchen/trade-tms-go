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
import { Select } from "@/components/ui/select";
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

/** Column definitions for the backtest-runs ResponsiveTable. The `#id` and
 * Status columns are the always-visible primaries on the mobile card; the rest
 * (full parity) fold under "More". */
function buildColumns(
  selectedId: number | null | undefined,
  onSelect: ((id: number) => void) | undefined,
): ColumnDef<BacktestSummary>[] {
  return [
    {
      key: "id",
      header: "ID",
      primary: true,
      className: "tabular-nums",
      render: (b) => (
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
      ),
    },
    {
      key: "kind",
      header: "Kind",
      render: (b) => <Badge variant="outline">{b.kind}</Badge>,
    },
    {
      key: "window",
      header: "Window",
      render: (b) => (
        <span className="text-xs text-muted-foreground" data-testid="run-window">
          {b.start_date} → {b.end_date}
        </span>
      ),
    },
    {
      key: "start_bal",
      header: "Start bal.",
      labelMobile: "Start balance",
      align: "right",
      className: "tabular-nums text-muted-foreground",
      render: (b) => formatMoney(b.starting_balance_usd),
    },
    {
      key: "final_bal",
      header: "Final bal.",
      labelMobile: "Final balance",
      align: "right",
      className: "tabular-nums",
      render: (b) => (
        <span data-testid="run-final-balance">
          {b.status === "RUNNING" ? "—" : formatMoney(b.final_balance_usd)}
        </span>
      ),
    },
    {
      key: "return",
      header: "Return",
      align: "right",
      className: "tabular-nums",
      render: (b) => <ReturnCell b={b} />,
    },
    {
      key: "pnl",
      header: "P&L",
      align: "right",
      className: "tabular-nums",
      render: (b) => (
        <span data-testid="run-pnl">
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
        </span>
      ),
    },
    {
      key: "strategies",
      header: "Strategies",
      className: "max-w-[14rem] truncate text-xs text-muted-foreground",
      render: (b) => (
        <span title={b.strategies.join(", ")} data-testid="run-strategies">
          {b.strategies.length ? b.strategies.join(", ") : "—"}
        </span>
      ),
    },
    {
      key: "status",
      header: "Status",
      primary: true,
      render: (b) => (
        <BacktestStatusBadge status={b.status} data-testid={`run-status-${b.id}`} />
      ),
    },
    {
      key: "created",
      header: "Created",
      className: "text-xs text-muted-foreground",
      render: (b) => (
        <span title={formatTs(b.created_at)} data-testid="run-created">
          {formatRelative(b.created_at)}
        </span>
      ),
    },
  ];
}

/**
 * Backtest runs list for the Compositions module. A row click selects a run for the
 * inline backtest panel — the standalone `/backtests/[id]` route is retired and
 * results render in place (docs/concept-alignment.md §3.4 ③). `selectedId`
 * highlights the open run.
 *
 * Rendered via <ResponsiveTable>: the shadcn table on desktop, a stacked card
 * list on mobile (full parity — every column carried, secondary columns under
 * "More").
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
      <CardHeader className="flex-col items-start gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-1">
          <CardTitle>Backtest runs</CardTitle>
          <CardDescription>
            Persisted runs (DB source of truth), newest first.
          </CardDescription>
        </div>
        <Select
          value={status}
          onChange={(e) => onStatusChange(e.target.value)}
          className="h-9 w-full text-xs sm:h-7 sm:w-40"
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
          <ResponsiveTable
            columns={buildColumns(selectedId, onSelect)}
            rows={rows}
            rowKey={(b) => b.id}
            rowTestId={(b) => `run-row-${b.id}`}
            onRowClick={(b) => onSelect?.(b.id)}
            data-testid="runs-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
