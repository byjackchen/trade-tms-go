"use client";

import { Fragment, useState, type ReactNode } from "react";
import { ChevronDown, ChevronRight, Rocket } from "lucide-react";
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
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { TrialStateBadge } from "./status-badge";
import { formatNum, formatMoney } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { TrialRow, TrialFold } from "@/lib/api/types";

function fmtParam(v: unknown): string {
  if (v == null) return "—";
  if (typeof v === "number")
    return Number.isInteger(v) ? String(v) : String(Number(v.toFixed(4)));
  if (typeof v === "boolean") return v ? "true" : "false";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

function foldNum(f: TrialFold, key: keyof TrialFold): number | undefined {
  const v = f[key];
  return typeof v === "number" ? v : undefined;
}

/** One labelled metric in the mobile trial card. */
function Stat({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="font-mono tabular-nums">{children}</dd>
    </div>
  );
}

/** Expandable per-fold breakdown for a trial. */
function FoldBreakdown({ trial }: { trial: TrialRow }) {
  const folds = trial.folds ?? [];
  return (
    <div className="space-y-3 p-3" data-testid={`trial-folds-${trial.number}`}>
      {/* params */}
      <div>
        <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          Params
        </p>
        <div className="flex flex-wrap gap-1.5">
          {trial.params && Object.keys(trial.params).length > 0 ? (
            Object.entries(trial.params).map(([k, v]) => (
              <Badge
                key={k}
                variant="outline"
                className="font-mono text-[11px]"
                data-testid={`trial-param-${trial.number}-${k}`}
              >
                {k}={fmtParam(v)}
              </Badge>
            ))
          ) : (
            <span className="text-xs text-muted-foreground">no params</span>
          )}
        </div>
      </div>

      {/* per-fold metrics */}
      <div>
        <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          Per-fold metrics
        </p>
        {folds.length === 0 ? (
          <span className="text-xs text-muted-foreground">
            no fold breakdown (single-window or not yet computed)
          </span>
        ) : (
          <div className="-mx-1 overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Fold</TableHead>
                <TableHead>Test window</TableHead>
                <TableHead className="text-right">Sharpe</TableHead>
                <TableHead className="text-right">Calmar</TableHead>
                <TableHead className="text-right">Max DD%</TableHead>
                <TableHead className="text-right">Final bal.</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {[...folds]
                .sort((a, b) => a.fold - b.fold)
                .map((f) => (
                  <TableRow key={f.fold} data-testid={`trial-fold-${trial.number}-${f.fold}`}>
                    <TableCell className="tabular-nums">{f.fold}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {f.test_start && f.test_end
                        ? `${String(f.test_start).slice(0, 10)} → ${String(f.test_end).slice(0, 10)}`
                        : "—"}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatNum(foldNum(f, "sharpe"))}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatNum(foldNum(f, "calmar"))}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatNum(foldNum(f, "max_drawdown_pct"))}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {foldNum(f, "final_balance_usd") != null
                        ? formatMoney(foldNum(f, "final_balance_usd"))
                        : "—"}
                    </TableCell>
                  </TableRow>
                ))}
            </TableBody>
          </Table>
          </div>
        )}
      </div>

      {trial.error ? (
        <p className="text-xs text-destructive" data-testid={`trial-error-${trial.number}`}>
          {trial.error}
        </p>
      ) : null}
    </div>
  );
}

export function TrialsTable({
  trials,
  selected,
  onSelect,
  onPromote,
}: {
  trials: TrialRow[];
  selected?: number | null;
  onSelect?: (n: number | null) => void;
  onPromote: (t: TrialRow) => void;
}) {
  const { mode } = useUiMode();
  const [expanded, setExpanded] = useState<Set<number>>(new Set());

  const toggle = (n: number) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(n)) next.delete(n);
      else next.add(n);
      return next;
    });

  // Pareto-front first, then by sharpe desc — the most promotable trials on top.
  const ordered = [...trials].sort((a, b) => {
    if (a.pareto_front !== b.pareto_front) return a.pareto_front ? -1 : 1;
    return (b.sharpe ?? -Infinity) - (a.sharpe ?? -Infinity);
  });

  // Mobile (cookie-driven, not CSS): a stacked card list with the same data and
  // the same inline fold-breakdown expansion as the desktop table — FULL PARITY
  // (docs/concept-alignment.md LOCKED DECISION 2). The desktop <Table> below is
  // unchanged.
  if (mode === "mobile") {
    return (
      <div
        className="flex flex-col gap-2"
        data-testid="hyperopt-trials-table"
        data-slot="responsive-table-cards"
      >
        {ordered.map((t) => {
          const isOpen = expanded.has(t.number);
          const isSel = selected === t.number;
          const dd = t.metrics?.max_drawdown_pct;
          const fb = t.metrics?.final_balance_usd;
          const paramKeys = t.params ? Object.keys(t.params) : [];
          return (
            <div
              key={t.number}
              data-testid={`hyperopt-trial-row-${t.number}`}
              data-slot="responsive-table-card"
              data-pareto={t.pareto_front ? "true" : "false"}
              data-sharpe={typeof t.sharpe === "number" ? String(t.sharpe) : undefined}
              data-calmar={typeof t.calmar === "number" ? String(t.calmar) : undefined}
              onClick={() => onSelect?.(isSel ? null : t.number)}
              className={cn(
                "cursor-pointer rounded-lg bg-card p-3 text-card-foreground ring-1 ring-foreground/10 transition-colors",
                isSel ? "ring-2 ring-primary" : "hover:bg-muted/40",
              )}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="flex items-center gap-2 text-sm font-medium tabular-nums">
                  trial #{t.number}
                  <TrialStateBadge state={t.state} data-testid={`trial-state-${t.number}`} />
                  {t.pareto_front ? (
                    <Badge variant="success" data-testid={`trial-pareto-${t.number}`}>
                      Pareto
                    </Badge>
                  ) : null}
                </span>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={t.state !== "COMPLETE"}
                  onClick={(e) => {
                    e.stopPropagation();
                    onPromote(t);
                  }}
                  data-testid={`hyperopt-promote-${t.number}`}
                >
                  <Rocket />
                  Promote
                </Button>
              </div>

              <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
                <Stat label="Sharpe">
                  <span data-testid={`trial-sharpe-${t.number}`}>{formatNum(t.sharpe)}</span>
                </Stat>
                <Stat label="Calmar">
                  <span data-testid={`trial-calmar-${t.number}`}>{formatNum(t.calmar)}</span>
                </Stat>
                <Stat label="Max DD%">
                  {formatNum(typeof dd === "number" ? dd : undefined)}
                </Stat>
                <Stat label="Final bal.">
                  {typeof fb === "number" ? formatMoney(fb) : "—"}
                </Stat>
                <Stat label="Params">
                  {paramKeys.length ? `${paramKeys.length} params` : "—"}
                </Stat>
              </dl>

              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  toggle(t.number);
                }}
                aria-expanded={isOpen}
                className="mt-1 flex min-h-[44px] items-center gap-1 text-xs font-medium text-muted-foreground"
                data-testid={`hyperopt-trial-expand-${t.number}`}
              >
                {isOpen ? (
                  <ChevronDown className="size-3.5" />
                ) : (
                  <ChevronRight className="size-3.5" />
                )}
                {isOpen ? "Hide breakdown" : "Params + per-fold metrics"}
              </button>
              {isOpen ? (
                <div data-testid={`trial-detail-${t.number}`} className="border-t">
                  <FoldBreakdown trial={t} />
                </div>
              ) : null}
            </div>
          );
        })}
      </div>
    );
  }

  return (
    <Table data-testid="hyperopt-trials-table">
      <TableHeader>
        <TableRow>
          <TableHead className="w-8" />
          <TableHead className="text-right">#</TableHead>
          <TableHead>State</TableHead>
          <TableHead>Front</TableHead>
          <TableHead className="text-right">Sharpe</TableHead>
          <TableHead className="text-right">Calmar</TableHead>
          <TableHead className="text-right">Max DD%</TableHead>
          <TableHead className="text-right">Final bal.</TableHead>
          <TableHead>Params</TableHead>
          <TableHead className="text-right">Promote</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {ordered.map((t) => {
          const isOpen = expanded.has(t.number);
          const isSel = selected === t.number;
          const dd = t.metrics?.max_drawdown_pct;
          const fb = t.metrics?.final_balance_usd;
          const paramKeys = t.params ? Object.keys(t.params) : [];
          return (
            <Fragment key={t.number}>
              <TableRow
                data-testid={`hyperopt-trial-row-${t.number}`}
                data-pareto={t.pareto_front ? "true" : "false"}
                data-sharpe={typeof t.sharpe === "number" ? String(t.sharpe) : undefined}
                data-calmar={typeof t.calmar === "number" ? String(t.calmar) : undefined}
                onClick={() => onSelect?.(isSel ? null : t.number)}
                className={cn("cursor-pointer", isSel && "bg-muted/60")}
              >
                <TableCell>
                  <button
                    type="button"
                    aria-label={isOpen ? "Collapse" : "Expand"}
                    onClick={(e) => {
                      e.stopPropagation();
                      toggle(t.number);
                    }}
                    className="text-muted-foreground hover:text-foreground"
                    data-testid={`hyperopt-trial-expand-${t.number}`}
                  >
                    {isOpen ? (
                      <ChevronDown className="size-4" />
                    ) : (
                      <ChevronRight className="size-4" />
                    )}
                  </button>
                </TableCell>
                <TableCell className="text-right font-medium tabular-nums">
                  {t.number}
                </TableCell>
                <TableCell>
                  <TrialStateBadge state={t.state} data-testid={`trial-state-${t.number}`} />
                </TableCell>
                <TableCell>
                  {t.pareto_front ? (
                    <Badge variant="success" data-testid={`trial-pareto-${t.number}`}>
                      Pareto
                    </Badge>
                  ) : (
                    <span className="text-xs text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="text-right tabular-nums" data-testid={`trial-sharpe-${t.number}`}>
                  {formatNum(t.sharpe)}
                </TableCell>
                <TableCell className="text-right tabular-nums" data-testid={`trial-calmar-${t.number}`}>
                  {formatNum(t.calmar)}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {formatNum(typeof dd === "number" ? dd : undefined)}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {typeof fb === "number" ? formatMoney(fb) : "—"}
                </TableCell>
                <TableCell className="max-w-[12rem] truncate text-xs text-muted-foreground">
                  {paramKeys.length
                    ? `${paramKeys.length} params`
                    : "—"}
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={t.state !== "COMPLETE"}
                    onClick={(e) => {
                      e.stopPropagation();
                      onPromote(t);
                    }}
                    data-testid={`hyperopt-promote-${t.number}`}
                  >
                    <Rocket />
                    Promote
                  </Button>
                </TableCell>
              </TableRow>
              {isOpen ? (
                <TableRow data-testid={`trial-detail-${t.number}`} className="hover:bg-transparent">
                  <TableCell colSpan={10} className="bg-muted/30 p-0">
                    <FoldBreakdown trial={t} />
                  </TableCell>
                </TableRow>
              ) : null}
            </Fragment>
          );
        })}
      </TableBody>
    </Table>
  );
}
