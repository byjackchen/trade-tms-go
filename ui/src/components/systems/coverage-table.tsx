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
import { Button } from "@/components/ui/button";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { FreshnessBadge } from "./freshness-badge";
import { formatInt, formatDate } from "@/lib/format";
import { useCoverage } from "@/lib/api/hooks";
import type { CoverageTable as CoverageRow } from "@/lib/api/types";

function GapCell({ row }: { row: CoverageRow }) {
  if (!row.gaps) {
    return <span className="text-muted-foreground">—</span>;
  }
  const { tickers_with_gaps, missing_days_total } = row.gaps;
  if (tickers_with_gaps === 0) {
    return (
      <Badge variant="success" data-testid="gaps-clean">
        no gaps
      </Badge>
    );
  }
  return (
    <Badge variant="warning" data-testid="gaps-present">
      {formatInt(tickers_with_gaps)} tickers · {formatInt(missing_days_total)}{" "}
      days
    </Badge>
  );
}

function buildColumns(
  onInspectTicker: (ticker: string) => void,
): ColumnDef<CoverageRow>[] {
  return [
    {
      key: "table",
      header: "Table",
      primary: true,
      render: (row) => (
        <span className="font-mono text-xs" data-testid="coverage-table-name">
          {row.table}
        </span>
      ),
    },
    {
      key: "rows",
      header: "Rows",
      align: "right",
      render: (row) => (
        <span className="tabular-nums" data-testid="coverage-rows">
          {formatInt(row.rows)}
        </span>
      ),
    },
    {
      key: "tickers",
      header: "Tickers",
      align: "right",
      render: (row) => (
        <span className="tabular-nums" data-testid="coverage-tickers">
          {formatInt(row.tickers)}
        </span>
      ),
    },
    {
      key: "range",
      header: "Date range",
      labelMobile: "Range",
      render: (row) => (
        <span className="text-xs text-muted-foreground" data-testid="coverage-range">
          {row.min_date && row.max_date
            ? `${row.min_date} → ${row.max_date}`
            : "—"}
        </span>
      ),
    },
    {
      key: "freshness",
      header: "Freshness",
      primary: true,
      render: (row) => (
        <span data-testid="coverage-freshness">
          {"freshness" in row && row.freshness ? (
            <FreshnessBadge freshness={row.freshness} />
          ) : (
            <span className="text-muted-foreground">—</span>
          )}
        </span>
      ),
    },
    {
      key: "gaps",
      header: "Gaps",
      primary: true,
      render: (row) => (
        <span data-testid="coverage-gaps">
          <GapCell row={row} />
        </span>
      ),
    },
    {
      key: "inspect",
      header: "Inspect",
      align: "right",
      render: (row) =>
        row.table === "bars_daily" ? (
          <Button
            size="sm"
            variant="outline"
            data-testid="coverage-inspect-gaps"
            onClick={(e) => {
              e.stopPropagation();
              const worst = row.gaps?.worst?.[0]?.ticker ?? "AAPL";
              onInspectTicker(worst);
            }}
          >
            Gaps
          </Button>
        ) : (
          <span className="text-muted-foreground">—</span>
        ),
    },
  ];
}

export function CoverageTable({
  onInspectTicker,
}: {
  onInspectTicker: (ticker: string) => void;
}) {
  const { data, isLoading, error, refetch } = useCoverage();

  return (
    <Card data-testid="coverage-card">
      <CardHeader className="flex-col items-start gap-1">
        <CardTitle>Dataset coverage</CardTitle>
        <CardDescription>
          {data
            ? `Latest NYSE session ${formatDate(data.latest_session)} · generated ${formatDate(
                data.generated_at.slice(0, 10),
              )}`
            : "Per-table rows, tickers, date range and freshness vs. the latest NYSE session."}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={4} data-testid="coverage-loading" />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="coverage-error"
          />
        ) : !data || data.tables.length === 0 ? (
          <EmptyState
            title="No datasets imported yet"
            hint="Run a data refresh to populate market-data tables."
            data-testid="coverage-empty"
          />
        ) : (
          <ResponsiveTable
            columns={buildColumns(onInspectTicker)}
            rows={data.tables}
            rowKey={(row) => row.table}
            rowTestId={(row) => `coverage-row-${row.table}`}
            data-testid="coverage-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
