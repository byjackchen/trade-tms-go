"use client";

import { useMemo, useState } from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { ApiError } from "@/lib/api/client";
import { useTickerGaps } from "@/lib/api/hooks";
import { formatInt } from "@/lib/format";

/** Inclusive list of YYYY-MM-DD calendar dates from `first` to `last`. */
function dateRange(first: string, last: string): string[] {
  const out: string[] = [];
  const start = new Date(`${first}T00:00:00Z`);
  const end = new Date(`${last}T00:00:00Z`);
  if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) return out;
  for (let d = start; d <= end; d.setUTCDate(d.getUTCDate() + 1)) {
    out.push(d.toISOString().slice(0, 10));
  }
  return out;
}

const MONTH_LABELS = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

/**
 * Per-ticker gap heatmap: a calendar strip over the ticker's bar span, one cell
 * per calendar day, grouped by month. Weekends are dimmed; days flagged as a
 * missing NYSE session (from the API's exact `missing[]` set) light up red.
 * This visualizes which sessions are absent at a glance.
 */
export function GapHeatmap({ initialTicker }: { initialTicker: string | null }) {
  const [input, setInput] = useState(initialTicker ?? "");
  const [ticker, setTicker] = useState<string | null>(initialTicker);

  // Sync the input/selection when the parent requests a new ticker (e.g. the
  // coverage table's "Gaps" action). Uses React's "adjust state during render"
  // pattern (a prev-prop state slot) instead of an effect, avoiding a
  // cascading-render round trip.
  const [seenInitial, setSeenInitial] = useState(initialTicker);
  if (initialTicker && initialTicker !== seenInitial) {
    setSeenInitial(initialTicker);
    setInput(initialTicker);
    setTicker(initialTicker);
  }

  const { data, isLoading, error, refetch, isFetching } = useTickerGaps(ticker);

  const months = useMemo(() => {
    if (!data || data.bars === 0) return [];
    const missing = new Set(data.missing);
    const days = dateRange(data.first, data.last);
    const byMonth = new Map<
      string,
      { date: string; missing: boolean; weekend: boolean }[]
    >();
    for (const date of days) {
      const key = date.slice(0, 7); // YYYY-MM
      const dow = new Date(`${date}T00:00:00Z`).getUTCDay();
      const weekend = dow === 0 || dow === 6;
      if (!byMonth.has(key)) byMonth.set(key, []);
      byMonth.get(key)!.push({ date, missing: missing.has(date), weekend });
    }
    return [...byMonth.entries()].map(([key, cells]) => {
      const [y, m] = key.split("-");
      return { key, label: `${MONTH_LABELS[Number(m) - 1]} ${y}`, cells };
    });
  }, [data]);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const v = input.trim().toUpperCase();
    if (v) setTicker(v);
  };

  return (
    <Card data-testid="gap-heatmap-card">
      <CardHeader className="flex-col items-start gap-2">
        <div className="flex w-full items-start justify-between gap-2">
          <div>
            <CardTitle>Session gap heatmap</CardTitle>
            <CardDescription>
              Missing NYSE sessions over a ticker&apos;s {`bars_daily`} span.
            </CardDescription>
          </div>
        </div>
        <form onSubmit={submit} className="flex w-full max-w-xs items-center gap-2">
          <Input
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Ticker (e.g. AAPL)"
            aria-label="Ticker"
            data-testid="gap-ticker-input"
            className="font-mono uppercase"
          />
          <Button type="submit" size="sm" data-testid="gap-ticker-submit">
            Inspect
          </Button>
        </form>
      </CardHeader>
      <CardContent>
        {!ticker ? (
          <EmptyState
            title="Pick a ticker"
            hint="Enter a symbol to visualize its missing trading sessions."
            data-testid="gap-empty"
          />
        ) : isLoading || (isFetching && !data) ? (
          <LoadingRows rows={3} data-testid="gap-loading" />
        ) : error ? (
          error instanceof ApiError && error.status === 404 ? (
            <EmptyState
              title={`Unknown ticker "${ticker}"`}
              hint="No such symbol in tms.tickers."
              data-testid="gap-not-found"
            />
          ) : (
            <ErrorState
              error={error}
              onRetry={() => refetch()}
              data-testid="gap-error"
            />
          )
        ) : !data || data.bars === 0 ? (
          <EmptyState
            title={`No bars for ${ticker}`}
            hint="This ticker has no stored daily bars."
            data-testid="gap-no-bars"
          />
        ) : (
          <div className="space-y-4" data-testid="gap-heatmap">
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <Badge variant="outline" data-testid="gap-span">
                {data.first} → {data.last}
              </Badge>
              <Badge variant="muted">{formatInt(data.bars)} bars</Badge>
              <Badge variant="muted">
                {formatInt(data.expected_sessions)} sessions expected
              </Badge>
              {data.missing_days > 0 ? (
                <Badge variant="destructive" data-testid="gap-missing-count">
                  {formatInt(data.missing_days)} missing
                </Badge>
              ) : (
                <Badge variant="success" data-testid="gap-no-missing">
                  complete
                </Badge>
              )}
              {data.missing_truncated ? (
                <Badge variant="warning" data-testid="gap-truncated">
                  list truncated at 1000
                </Badge>
              ) : null}
            </div>

            <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs text-muted-foreground">
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-3 rounded-sm bg-emerald-500/30 ring-1 ring-emerald-500/40" />
                present
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-3 rounded-sm bg-destructive ring-1 ring-destructive/60" />
                missing session
              </span>
              <span className="flex items-center gap-1.5">
                <span className="inline-block size-3 rounded-sm bg-muted ring-1 ring-border" />
                weekend
              </span>
            </div>

            <div className="grid grid-cols-1 gap-3 ui-desktop:sm:grid-cols-2 ui-desktop:lg:grid-cols-3">
              {months.map((month) => (
                <div
                  key={month.key}
                  className="rounded-lg border border-border p-2"
                  data-testid={`gap-month-${month.key}`}
                >
                  <div className="mb-1.5 text-[11px] font-medium text-muted-foreground">
                    {month.label}
                  </div>
                  <div className="flex flex-wrap gap-[3px]">
                    {month.cells.map((cell) => (
                      <span
                        key={cell.date}
                        title={`${cell.date}${cell.missing ? " — missing session" : cell.weekend ? " — weekend" : ""}`}
                        data-testid={cell.missing ? "gap-cell-missing" : "gap-cell"}
                        data-missing={cell.missing ? "true" : "false"}
                        className={
                          cell.missing
                            ? "size-3 rounded-sm bg-destructive ring-1 ring-destructive/60"
                            : cell.weekend
                              ? "size-3 rounded-sm bg-muted ring-1 ring-border"
                              : "size-3 rounded-sm bg-emerald-500/30 ring-1 ring-emerald-500/40"
                        }
                      />
                    ))}
                  </div>
                </div>
              ))}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
