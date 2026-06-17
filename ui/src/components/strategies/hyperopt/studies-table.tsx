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
import { StudyStatusBadge } from "./status-badge";
import { useStudies } from "@/lib/api/hooks";
import { formatNum, formatRelative, formatTs } from "@/lib/format";
import { HYPEROPT_STRATEGIES, type StudyRow } from "@/lib/api/types";

const STRATEGY_OPTIONS = ["", ...HYPEROPT_STRATEGIES];

/**
 * The studies list for a strategy's Tune panel. A row click selects the study
 * for the inline detail panel — the study detail is no longer a separate route
 * (`/hyperopt/[id]` is retired); it opens in place, per the "inline results"
 * decision (docs/concept-alignment.md §3.4).
 *
 * `strategy` scopes the list. The standalone view passes `onStrategyChange` to
 * keep the strategy filter dropdown; the per-strategy Tune panel fixes the
 * strategy and omits it, hiding the dropdown.
 */
export function StudiesTable({
  strategy,
  onStrategyChange,
  selectedTs,
  onSelect,
}: {
  strategy: string;
  onStrategyChange?: (s: string) => void;
  selectedTs?: string | null;
  onSelect?: (ts: string) => void;
}) {
  const { data, isLoading, error, refetch } = useStudies(strategy || undefined);
  const rows = data?.studies ?? [];

  const columns: ColumnDef<StudyRow>[] = [
    {
      key: "study",
      header: "Study",
      primary: true,
      render: (s) => (
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            onSelect?.(s.ts);
          }}
          className="font-mono text-xs font-medium text-primary underline-offset-2 hover:underline"
          data-testid={`study-link-${s.ts}`}
        >
          {s.ts}
        </button>
      ),
    },
    {
      key: "strategy",
      header: "Strategy",
      primary: true,
      render: (s) => <Badge variant="outline">{s.config.strategy}</Badge>,
    },
    {
      key: "window",
      header: "Window",
      render: (s) => {
        const wf = s.config.walk_forward;
        return (
          <span className="text-xs text-muted-foreground" data-testid="study-window">
            {s.config.start} → {s.config.end}
            {wf?.enabled ? (
              <span className="ml-1 text-[10px] uppercase tracking-wide text-muted-foreground/70">
                · {wf.folds}f/{wf.embargo_days}d
              </span>
            ) : null}
          </span>
        );
      },
    },
    {
      key: "trials",
      header: "Trials",
      align: "right",
      render: (s) => (
        <span className="tabular-nums" data-testid="study-trials">
          {s.progress.completed_trials}
          <span className="text-muted-foreground">/{s.progress.total_trials}</span>
          {s.progress.failed_trials > 0 ? (
            <span className="ml-1 text-xs text-destructive">
              ({s.progress.failed_trials} fail)
            </span>
          ) : null}
        </span>
      ),
    },
    {
      key: "best-sharpe",
      header: "Best Sharpe",
      align: "right",
      primary: true,
      render: (s) => (
        <span className="tabular-nums" data-testid="study-best-sharpe">
          {s.progress.current_best ? formatNum(s.progress.current_best.sharpe) : "—"}
        </span>
      ),
    },
    {
      key: "best-calmar",
      header: "Best Calmar",
      align: "right",
      render: (s) => (
        <span className="tabular-nums" data-testid="study-best-calmar">
          {s.progress.current_best ? formatNum(s.progress.current_best.calmar) : "—"}
        </span>
      ),
    },
    {
      key: "status",
      header: "Status",
      primary: true,
      render: (s) => (
        <StudyStatusBadge
          status={s.progress.status}
          data-testid={`study-status-${s.ts}`}
        />
      ),
    },
    {
      key: "created",
      header: "Created",
      render: (s) => (
        <span
          className="text-xs text-muted-foreground"
          title={formatTs(s.config.created_at)}
          data-testid="study-created"
        >
          {formatRelative(s.config.created_at)}
        </span>
      ),
    },
  ];

  return (
    <Card data-testid="studies-card">
      <CardHeader className="flex-row items-start justify-between gap-2">
        <div className="space-y-1">
          <CardTitle>Hyperopt studies</CardTitle>
          <CardDescription>
            NSGA-II walk-forward studies (DB source of truth), newest first.
          </CardDescription>
        </div>
        {onStrategyChange ? (
          <Select
            value={strategy}
            onChange={(e) => onStrategyChange(e.target.value)}
            className="h-7 w-44 text-xs"
            data-testid="studies-strategy-filter"
            aria-label="Filter by strategy"
          >
            {STRATEGY_OPTIONS.map((s) => (
              <option key={s || "all"} value={s}>
                {s || "All strategies"}
              </option>
            ))}
          </Select>
        ) : null}
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={5} data-testid="studies-loading" />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="studies-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No studies yet"
            hint='Click "New study" to launch an NSGA-II walk-forward optimization.'
            data-testid="studies-empty"
          />
        ) : (
          <ResponsiveTable
            columns={columns}
            rows={rows}
            rowKey={(s) => s.ts}
            data-testid="studies-table"
            onRowClick={(s) => onSelect?.(s.ts)}
            rowTestId={(s) => `study-row-${s.ts}`}
            rowAttrs={(s) => ({
              "data-selected": selectedTs === s.ts ? "true" : "false",
            })}
            rowClassName={(s) => (selectedTs === s.ts ? "bg-accent/60" : undefined)}
          />
        )}
      </CardContent>
    </Card>
  );
}
