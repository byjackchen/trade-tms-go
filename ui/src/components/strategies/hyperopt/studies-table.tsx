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
import { StudyStatusBadge } from "./status-badge";
import { useStudies } from "@/lib/api/hooks";
import { formatNum, formatRelative, formatTs } from "@/lib/format";
import { HYPEROPT_STRATEGIES } from "@/lib/api/types";

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
          <Table data-testid="studies-table">
            <TableHeader>
              <TableRow>
                <TableHead>Study</TableHead>
                <TableHead>Strategy</TableHead>
                <TableHead>Window</TableHead>
                <TableHead className="text-right">Trials</TableHead>
                <TableHead className="text-right">Best Sharpe</TableHead>
                <TableHead className="text-right">Best Calmar</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((s) => {
                const wf = s.config.walk_forward;
                const best = s.progress.current_best;
                const done = s.progress.completed_trials;
                const total = s.progress.total_trials;
                const isSelected = selectedTs === s.ts;
                return (
                  <TableRow
                    key={s.ts}
                    data-testid={`study-row-${s.ts}`}
                    data-selected={isSelected ? "true" : "false"}
                    onClick={() => onSelect?.(s.ts)}
                    className={cn(
                      "cursor-pointer",
                      isSelected && "bg-accent/60",
                    )}
                  >
                    <TableCell>
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
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline">{s.config.strategy}</Badge>
                    </TableCell>
                    <TableCell
                      className="text-xs text-muted-foreground"
                      data-testid="study-window"
                    >
                      {s.config.start} → {s.config.end}
                      {wf?.enabled ? (
                        <span className="ml-1 text-[10px] uppercase tracking-wide text-muted-foreground/70">
                          · {wf.folds}f/{wf.embargo_days}d
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell
                      className="text-right tabular-nums"
                      data-testid="study-trials"
                    >
                      {done}
                      <span className="text-muted-foreground">/{total}</span>
                      {s.progress.failed_trials > 0 ? (
                        <span className="ml-1 text-xs text-destructive">
                          ({s.progress.failed_trials} fail)
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell
                      className="text-right tabular-nums"
                      data-testid="study-best-sharpe"
                    >
                      {best ? formatNum(best.sharpe) : "—"}
                    </TableCell>
                    <TableCell
                      className="text-right tabular-nums"
                      data-testid="study-best-calmar"
                    >
                      {best ? formatNum(best.calmar) : "—"}
                    </TableCell>
                    <TableCell>
                      <StudyStatusBadge
                        status={s.progress.status}
                        data-testid={`study-status-${s.ts}`}
                      />
                    </TableCell>
                    <TableCell
                      className="text-xs text-muted-foreground"
                      title={formatTs(s.config.created_at)}
                      data-testid="study-created"
                    >
                      {formatRelative(s.config.created_at)}
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
