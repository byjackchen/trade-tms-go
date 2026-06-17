"use client";

import { useEffect, useState } from "react";
import { AlertOctagon, CheckCircle2 } from "lucide-react";
import { useLiveReconciliation } from "@/lib/api/hooks";
import { hasReconciliation } from "@/lib/api/types";
import type { ReconMismatch } from "@/lib/api/types";
import { ApiError } from "@/lib/api/client";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/shell/states";
import { formatInt, formatRelative } from "@/lib/format";

/**
 * Reconciliation panel: the latest broker-vs-strategy-books report
 * (GET /api/v1/live/reconciliation). A mismatch HALTS the live node and surfaces
 * here in red — it is never auto-corrected by trading (decision 5 /
 * portfolio-risk.md). `diff = broker_net − strategy_books_sum`.
 */
const MISMATCH_COLUMNS: ColumnDef<ReconMismatch>[] = [
  {
    key: "symbol",
    header: "Symbol",
    primary: true,
    render: (m) => <span className="font-mono font-medium">{m.symbol}</span>,
  },
  {
    key: "strategy_books_sum",
    header: "Strategy books",
    align: "right",
    render: (m) => (
      <span className="font-mono">{formatInt(m.strategy_books_sum)}</span>
    ),
  },
  {
    key: "broker_net",
    header: "Broker net",
    align: "right",
    render: (m) => <span className="font-mono">{formatInt(m.broker_net)}</span>,
  },
  {
    key: "diff",
    header: "Diff",
    align: "right",
    primary: true,
    render: (m) => (
      <span className="font-mono font-medium text-destructive">
        {m.diff > 0 ? "+" : ""}
        {formatInt(m.diff)}
      </span>
    ),
  },
];

export function ReconciliationPanel() {
  const q = useLiveReconciliation();
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const noReader = q.error instanceof ApiError && q.error.status === 503;
  const report = hasReconciliation(q.data) ? q.data : null;
  const hasIssues = report?.has_issues ?? false;

  return (
    <Card
      data-testid="live-reconciliation"
      data-panel="reconciliation-panel"
      data-state={
        noReader
          ? "no-reader"
          : report
            ? hasIssues
              ? "mismatch"
              : "matched"
            : "none"
      }
      data-has-issues={hasIssues ? "true" : "false"}
      className={hasIssues ? "ring-2 ring-destructive/60" : undefined}
    >
      <CardHeader>
        <CardTitle className="text-sm">Reconciliation</CardTitle>
        {report ? (
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {hasIssues ? (
              <Badge variant="destructive" data-testid="recon-status-badge">
                MISMATCH
              </Badge>
            ) : (
              <Badge variant="success" data-testid="recon-status-badge">
                matched
              </Badge>
            )}
            <span data-testid="recon-asof" title={report.ts}>
              as of {formatRelative(report.ts, now)}
            </span>
          </span>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-3">
        {q.isLoading ? (
          <div className="space-y-2" data-testid="recon-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Reconciliation compares broker positions to strategy books in paper/live."
            data-testid="recon-no-reader"
          />
        ) : q.error ? (
          <p className="py-2 text-xs text-destructive" data-testid="recon-error">
            Failed to load reconciliation: {q.error.message}
          </p>
        ) : !report ? (
          <EmptyState
            title="No reconciliation report yet"
            hint="Reconciliation runs periodically and on demand. Trigger one with the reconcile control."
            data-testid="recon-empty"
          />
        ) : (
          <>
            {/* Summary banner */}
            {hasIssues ? (
              <div
                className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
                data-testid="live-reconciliation-mismatch"
              >
                <AlertOctagon className="mt-0.5 size-4 shrink-0" />
                <div>
                  Broker positions diverge from the strategy books. The node
                  halts on mismatch and never auto-corrects — investigate before
                  resuming.
                </div>
              </div>
            ) : (
              <div
                className="flex items-center gap-2 rounded-lg border border-emerald-500/30 bg-emerald-500/5 px-3 py-2 text-sm text-emerald-600 dark:text-emerald-400"
                data-testid="recon-matched-banner"
              >
                <CheckCircle2 className="size-4 shrink-0" />
                <span>
                  Broker positions match the strategy books
                  {report.matched.length
                    ? ` (${report.matched.length} symbol${report.matched.length === 1 ? "" : "s"})`
                    : ""}
                  . Tolerance {formatInt(report.tolerance_shares)} shares.
                </span>
              </div>
            )}

            {/* Mismatches table (only when present) */}
            {report.mismatches.length > 0 ? (
              <ResponsiveTable
                columns={MISMATCH_COLUMNS}
                rows={report.mismatches}
                rowKey={(m) => m.symbol}
                rowTestId={() => "live-recon-mismatch-row"}
                rowAttrs={(m) => ({
                  "data-symbol": m.symbol,
                  "data-diff": String(m.diff),
                  "data-broker-net": String(m.broker_net),
                  "data-strategy-sum": String(m.strategy_books_sum),
                })}
                data-testid="recon-mismatch-table"
              />
            ) : null}

            {/* One-sided symbols */}
            {report.symbols_only_in_strategies.length > 0 ||
            report.symbols_only_at_broker.length > 0 ? (
              <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                <OneSided
                  label="Only in strategy books"
                  symbols={report.symbols_only_in_strategies}
                  testid="recon-only-strategies"
                />
                <OneSided
                  label="Only at broker"
                  symbols={report.symbols_only_at_broker}
                  testid="recon-only-broker"
                />
              </div>
            ) : null}
          </>
        )}
      </CardContent>
    </Card>
  );
}

function OneSided({
  label,
  symbols,
  testid,
}: {
  label: string;
  symbols: string[];
  testid: string;
}) {
  if (symbols.length === 0) return null;
  return (
    <div
      className="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2"
      data-testid={testid}
    >
      <p className="text-[10px] uppercase tracking-wide text-amber-600 dark:text-amber-400">
        {label} ({symbols.length})
      </p>
      <p className="mt-1 font-mono text-xs">{symbols.join(", ")}</p>
    </div>
  );
}
