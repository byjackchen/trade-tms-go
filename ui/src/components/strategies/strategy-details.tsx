"use client";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { ErrorState, EmptyState, LoadingRows } from "@/components/shell/states";
import { useStrategy } from "@/lib/api/hooks";
import { type ParamSchema } from "@/lib/api/types";

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

/**
 * One strategy's DETAILS: schema version + params source and the resolved active
 * params table (active_values overlaid on the schema defaults).
 *
 * A strategy has NO backtest entry here (docs/concept-alignment.md §3.4 A3):
 * Backtest is a Composition operation — to backtest a single strategy, backtest its
 * single-member Composition (e.g. `sepa-only`) from the Compositions module. The only
 * param-tuning surface on a strategy is the Hyperopt panel (sibling component).
 *
 * NOTE (§3.3, C3): `capital_pct` and `active` were REMOVED from GET /strategies —
 * weight + on/off are Composition-member properties now, served by /compositions — so this
 * view does NOT render allocation or an enabled flag.
 */
/** Build the param-table columns; `activeValues` resolves the active value/row. */
function buildParamColumns(
  activeValues: Record<string, unknown>,
): ColumnDef<ParamSchema>[] {
  const activeOf = (p: ParamSchema) =>
    p.name in activeValues ? activeValues[p.name] : p.default;
  return [
    {
      key: "name",
      header: "Name",
      primary: true,
      render: (p) => <span className="font-mono text-sm">{p.name}</span>,
    },
    {
      key: "type",
      header: "Type",
      render: (p) => (
        <span className="text-sm text-muted-foreground">{p.type}</span>
      ),
    },
    {
      key: "active",
      header: "Active value",
      labelMobile: "Active",
      align: "right",
      primary: true,
      render: (p) => (
        <span
          className="font-mono text-sm"
          data-testid={`param-value-${p.name}`}
          data-param-name={p.name}
          data-param-value={valueAttr(activeOf(p))}
        >
          {formatValue(activeOf(p))}
        </span>
      ),
    },
    {
      key: "range",
      header: "Search range",
      labelMobile: "Range",
      render: (p) => (
        <span className="font-mono text-sm text-muted-foreground">
          {formatRange(p)}
        </span>
      ),
    },
    {
      key: "description",
      header: "Description",
      className: "max-w-xs",
      render: (p) => (
        <span className="text-sm text-muted-foreground">
          {p.description || "—"}
        </span>
      ),
    },
  ];
}

export function StrategyDetails({ strategyId }: { strategyId: string }) {
  const { mode } = useUiMode();
  const query = useStrategy(strategyId);
  const m = query.data?.strategy;

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
                    className={
                      mode === "mobile"
                        ? undefined
                        : "overflow-hidden rounded-lg border border-border"
                    }
                    data-testid="strategy-params-table"
                  >
                    <ResponsiveTable
                      columns={buildParamColumns(m.active_values)}
                      rows={m.parameters}
                      rowKey={(p) => p.name}
                      rowTestId={(p) => `param-row-${p.name}`}
                    />
                  </div>
                )}
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
