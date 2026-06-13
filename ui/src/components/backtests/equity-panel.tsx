"use client";

import { useQueries } from "@tanstack/react-query";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { EquityChart, type EquitySeries } from "./equity-chart";
import { apiGet } from "@/lib/api/client";
import type { EquityResponse } from "@/lib/api/types";

// A stable, theme-friendly palette for per-strategy curves. Portfolio is the
// primary; strategies cycle through the rest.
const PORTFOLIO_COLOR = "var(--chart-portfolio, #6366f1)";
const STRATEGY_COLORS = [
  "#10b981",
  "#f59e0b",
  "#ef4444",
  "#06b6d4",
  "#a855f7",
  "#ec4899",
  "#84cc16",
];

export function EquityPanel({
  id,
  strategies,
}: {
  id: number;
  strategies: string[];
}) {
  // One query for the portfolio curve plus one per strategy. Combined so a
  // single loading/error state covers the whole chart.
  const results = useQueries({
    queries: [
      {
        queryKey: ["backtest", id, "equity", "portfolio"],
        queryFn: () => apiGet<EquityResponse>(`backtests/${id}/equity`),
      },
      ...strategies.map((s) => ({
        queryKey: ["backtest", id, "equity", s],
        queryFn: () =>
          apiGet<EquityResponse>(`backtests/${id}/equity`, { strategy: s }),
      })),
    ],
  });

  const isLoading = results.some((r) => r.isLoading);
  const portfolioErr = results[0]?.error as Error | undefined;
  const refetchAll = () => results.forEach((r) => void r.refetch());

  const series: EquitySeries[] = [];
  const portfolioPts = results[0]?.data?.points ?? [];
  if (portfolioPts.length > 0) {
    series.push({
      id: "portfolio",
      label: "Portfolio (account)",
      color: PORTFOLIO_COLOR,
      points: portfolioPts,
    });
  }
  strategies.forEach((s, i) => {
    const pts = results[i + 1]?.data?.points ?? [];
    if (pts.length > 0) {
      series.push({
        id: s,
        label: s,
        color: STRATEGY_COLORS[i % STRATEGY_COLORS.length] ?? PORTFOLIO_COLOR,
        points: pts,
      });
    }
  });

  return (
    <Card data-testid="equity-card">
      <CardHeader className="flex-col items-start gap-1">
        <CardTitle>Equity curve</CardTitle>
        <CardDescription>
          Portfolio account equity plus each strategy&apos;s cumulative PnL.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={6} data-testid="equity-loading" />
        ) : portfolioErr ? (
          <ErrorState
            error={portfolioErr}
            onRetry={refetchAll}
            data-testid="equity-error"
          />
        ) : series.length === 0 ? (
          <EmptyState
            title="No equity data"
            hint="The equity curve appears once the run completes."
            data-testid="equity-empty"
          />
        ) : (
          <EquityChart series={series} />
        )}
      </CardContent>
    </Card>
  );
}
