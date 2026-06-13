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
          <Table data-testid="coverage-table">
            <TableHeader>
              <TableRow>
                <TableHead>Table</TableHead>
                <TableHead className="text-right">Rows</TableHead>
                <TableHead className="text-right">Tickers</TableHead>
                <TableHead>Date range</TableHead>
                <TableHead>Freshness</TableHead>
                <TableHead>Gaps</TableHead>
                <TableHead className="text-right">Inspect</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.tables.map((row) => (
                <TableRow key={row.table} data-testid={`coverage-row-${row.table}`}>
                  <TableCell className="font-mono text-xs" data-testid="coverage-table-name">
                    {row.table}
                  </TableCell>
                  <TableCell className="text-right tabular-nums" data-testid="coverage-rows">
                    {formatInt(row.rows)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums" data-testid="coverage-tickers">
                    {formatInt(row.tickers)}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground" data-testid="coverage-range">
                    {row.min_date && row.max_date
                      ? `${row.min_date} → ${row.max_date}`
                      : "—"}
                  </TableCell>
                  <TableCell data-testid="coverage-freshness">
                    {"freshness" in row && row.freshness ? (
                      <FreshnessBadge freshness={row.freshness} />
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell data-testid="coverage-gaps">
                    <GapCell row={row} />
                  </TableCell>
                  <TableCell className="text-right">
                    {row.table === "bars_daily" ? (
                      <Button
                        size="sm"
                        variant="outline"
                        data-testid="coverage-inspect-gaps"
                        onClick={() => {
                          const worst = row.gaps?.worst?.[0]?.ticker ?? "AAPL";
                          onInspectTicker(worst);
                        }}
                      >
                        Gaps
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
