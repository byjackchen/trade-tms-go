"use client";

import { useState } from "react";
import { FlaskConical } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ErrorState, EmptyState, LoadingRows } from "@/components/shell/states";
import { NewBacktestDialog } from "@/components/models/new-backtest-dialog";
import { useStrategy } from "@/lib/api/hooks";
import {
  BACKTEST_STRATEGIES,
  type BacktestStrategy,
  type ParamSchema,
} from "@/lib/api/types";

/** Render any param value (number/string/list) as a stable display string. */
function formatValue(v: unknown): string {
  if (v == null) return "—";
  if (Array.isArray(v)) return v.map((x) => String(x)).join(", ");
  if (typeof v === "number") return String(v);
  return String(v);
}

/**
 * The exact, machine-parseable form of a param value for `data-param-value`
 * (e2e ground-truth coupling): numbers/strings verbatim, lists comma-joined with
 * no spaces so they round-trip against `String(array)`.
 */
function valueAttr(v: unknown): string {
  if (v == null) return "";
  if (Array.isArray(v)) return v.map((x) => String(x)).join(",");
  return String(v);
}

/** Render the [low, high] search range, em-dash when the param is static. */
function formatRange(p: ParamSchema): string {
  if (p.search_low == null || p.search_high == null) return "—";
  return `${p.search_low} – ${p.search_high}`;
}

/** Narrow the backtest token to the dialog's accepted set (defaults scripted). */
function asBacktestStrategy(token: string): BacktestStrategy {
  return (BACKTEST_STRATEGIES as readonly string[]).includes(token)
    ? (token as BacktestStrategy)
    : "scripted";
}

/**
 * One strategy's DETAILS: schema version + params source, the resolved active
 * params table (active_values overlaid on the schema defaults), and a launcher
 * for a single-member-Model backtest.
 *
 * NOTE (docs/concept-alignment.md §3.3, C3): `capital_pct` and `active` were
 * REMOVED from GET /strategies — weight + on/off are Model-member properties now,
 * served by /models — so this view does NOT render allocation or an enabled flag.
 */
export function StrategyDetails({ strategyId }: { strategyId: string }) {
  const query = useStrategy(strategyId);
  const m = query.data?.strategy;
  const [dialogOpen, setDialogOpen] = useState(false);

  return (
    <div
      className="space-y-4"
      data-testid="strategy-detail"
      data-strategy={m?.id ?? strategyId}
    >
      {query.isLoading ? (
        <LoadingRows rows={6} data-testid="strategy-detail-loading" />
      ) : query.isError ? (
        <ErrorState
          error={query.error}
          onRetry={() => query.refetch()}
          data-testid="strategy-detail-error"
        />
      ) : !m ? (
        <EmptyState
          title="Strategy not found"
          data-testid="strategy-detail-empty"
        />
      ) : (
        <>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex items-center gap-3" data-testid="strategy-meta">
              <Badge
                variant={m.params_source === "db" ? "default" : "secondary"}
                data-testid="strategy-source"
              >
                {m.params_source}
              </Badge>
              <span className="font-mono text-xs text-muted-foreground">
                schema v{m.schema_version}
              </span>
              <span className="text-xs text-muted-foreground">
                {m.parameters_count} params
              </span>
            </div>
            {!m.error ? (
              <Button
                size="sm"
                onClick={() => setDialogOpen(true)}
                data-testid="strategy-backtest-launch"
              >
                <FlaskConical />
                Run backtest with this strategy
              </Button>
            ) : null}
          </div>

          {m.description ? (
            <p
              className="text-sm text-muted-foreground"
              data-testid="strategy-description"
            >
              {m.description}
            </p>
          ) : null}

          {m.error ? (
            <ErrorState
              error={new Error(m.error)}
              data-testid="strategy-params-error"
            />
          ) : (
            <Card>
              <CardHeader>
                <CardTitle className="text-sm">
                  Parameters{" "}
                  <span className="font-normal text-muted-foreground">
                    ({m.parameters_count})
                  </span>
                </CardTitle>
              </CardHeader>
              <CardContent>
                {m.parameters.length === 0 ? (
                  <EmptyState
                    title="No parameters"
                    data-testid="strategy-params-empty"
                  />
                ) : (
                  <div
                    className="overflow-hidden rounded-lg border border-border"
                    data-testid="strategy-params-table"
                  >
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Name</TableHead>
                          <TableHead>Type</TableHead>
                          <TableHead className="text-right">
                            Active value
                          </TableHead>
                          <TableHead>Search range</TableHead>
                          <TableHead>Description</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {m.parameters.map((p) => {
                          const activeValue =
                            p.name in m.active_values
                              ? m.active_values[p.name]
                              : p.default;
                          return (
                            <TableRow
                              key={p.name}
                              data-testid={`param-row-${p.name}`}
                              data-param-name={p.name}
                              data-param-value={valueAttr(activeValue)}
                            >
                              <TableCell className="font-mono text-sm">
                                {p.name}
                              </TableCell>
                              <TableCell className="text-sm text-muted-foreground">
                                {p.type}
                              </TableCell>
                              <TableCell
                                className="text-right font-mono text-sm"
                                data-testid={`param-value-${p.name}`}
                              >
                                {formatValue(activeValue)}
                              </TableCell>
                              <TableCell className="font-mono text-sm text-muted-foreground">
                                {formatRange(p)}
                              </TableCell>
                              <TableCell className="max-w-xs text-sm text-muted-foreground">
                                {p.description || "—"}
                              </TableCell>
                            </TableRow>
                          );
                        })}
                      </TableBody>
                    </Table>
                  </div>
                )}
              </CardContent>
            </Card>
          )}
        </>
      )}

      {/* Mount the dialog only while open so its initial form state picks up this
          strategy as the prefill (no in-flight prop sync needed). */}
      {m && !m.error && dialogOpen ? (
        <NewBacktestDialog
          open
          onClose={() => setDialogOpen(false)}
          prefillStrategy={asBacktestStrategy(m.backtest_id)}
        />
      ) : null}
    </div>
  );
}
