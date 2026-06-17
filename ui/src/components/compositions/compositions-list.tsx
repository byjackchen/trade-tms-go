"use client";

import { FlaskConical, Pencil, Trash2, Plus, Sparkles } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { useCompositions, useDeleteComposition } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import type { Composition } from "@/lib/api/types";

const STRATEGY_LABEL: Record<string, string> = {
  sepa: "SEPA",
  sector_rotation: "Sector",
  pairs: "Pairs",
  intraday_breakout: "ORB",
};

function activeWeight(composition: Composition): number {
  return composition.members
    .filter((m) => m.active)
    .reduce((sum, m) => sum + m.weight, 0);
}

/**
 * The Compositions list (docs/concept-alignment.md §3.4 ③). Each Composition is a card with
 * its members + weights + cash + risk caps and the per-Composition actions:
 * Backtest, Hyperopt, Edit, Delete. A Composition COMPOSES already-tuned strategies:
 * Backtest VALIDATES the blueprint, while Composition Hyperopt tunes the blueprint's
 * WEIGHTS & RISK (member strategy params stay FIXED — per-strategy Hyperopt in the
 * Strategies module owns SIGNAL params). Backtest/Hyperopt/Edit are delegated to the
 * page (which owns the dialogs + inline panels); Delete is handled inline.
 */
export function CompositionsList({
  onNew,
  onEdit,
  onBacktest,
  onHyperopt,
}: {
  onNew: () => void;
  onEdit: (composition: Composition) => void;
  onBacktest: (composition: Composition) => void;
  onHyperopt: (composition: Composition) => void;
}) {
  const { data, isLoading, error, refetch } = useCompositions();
  const del = useDeleteComposition();
  const compositions = data?.compositions ?? [];

  const remove = (composition: Composition) => {
    if (
      !window.confirm(
        `Delete Composition "${composition.name}" (${composition.id})? This cannot be undone.`,
      )
    ) {
      return;
    }
    del.mutate({ id: composition.id, actor: "ui" });
  };

  if (isLoading) {
    return <LoadingRows rows={4} data-testid="compositions-loading" />;
  }

  // A 503 means the composition store is not wired (an expected degraded state).
  if (error) {
    const status = error instanceof ApiError ? error.status : undefined;
    if (status === 503) {
      return (
        <Alert data-testid="compositions-unavailable">
          <AlertDescription>
            The Composition store is not configured on this server yet. Compositions become
            available once the backend store is wired.
          </AlertDescription>
        </Alert>
      );
    }
    return (
      <ErrorState error={error} onRetry={() => refetch()} data-testid="compositions-error" />
    );
  }

  return (
    <div className="space-y-4" data-testid="compositions-list">
      <div className="flex items-center justify-between gap-2">
        <p className="text-sm text-muted-foreground">
          {compositions.length} Composition{compositions.length === 1 ? "" : "s"} — named portfolio
          blueprints the engine drops in for backtest, paper and live.
        </p>
        <Button size="sm" onClick={onNew} data-testid="compositions-new">
          <Plus />
          New Composition
        </Button>
      </div>

      {del.isError ? (
        <Alert variant="destructive" data-testid="compositions-delete-error">
          <AlertDescription>
            {del.error instanceof ApiError
              ? del.error.message
              : "Failed to delete Composition."}
          </AlertDescription>
        </Alert>
      ) : null}

      {compositions.length === 0 ? (
        <EmptyState
          title="No Compositions yet"
          hint='Click "New Composition" to compose your first portfolio blueprint.'
          data-testid="compositions-empty"
        />
      ) : (
        <div className="grid gap-4 lg:grid-cols-2" data-testid="compositions-grid">
          {compositions.map((m) => {
            const aw = activeWeight(m);
            const remainder = 1 - aw - m.cash_pct;
            return (
              <Card key={m.id} data-testid={`composition-card-${m.id}`}>
                <CardHeader className="flex-row items-start justify-between gap-2">
                  <div className="space-y-1">
                    <CardTitle className="flex items-center gap-2">
                      {m.name}
                      <Badge variant="outline" className="font-mono text-[10px]">
                        {m.id}
                      </Badge>
                      <Badge variant="muted" className="text-[10px]">
                        v{m.version}
                      </Badge>
                    </CardTitle>
                    <CardDescription>
                      {m.description || "No description."}
                    </CardDescription>
                  </div>
                </CardHeader>
                <CardContent className="space-y-3">
                  {/* members */}
                  <div className="flex flex-wrap gap-1.5" data-testid={`composition-members-${m.id}`}>
                    {m.members.length === 0 ? (
                      <span className="text-xs text-muted-foreground">No members.</span>
                    ) : (
                      m.members.map((mem) => (
                        <Badge
                          key={mem.strategy_id}
                          variant={mem.active ? "secondary" : "muted"}
                          className="gap-1"
                          data-testid={`composition-member-${m.id}-${mem.strategy_id}`}
                        >
                          {STRATEGY_LABEL[mem.strategy_id] ?? mem.strategy_id}
                          <span className="tabular-nums opacity-80">
                            {(mem.weight * 100).toFixed(0)}%
                          </span>
                          {!mem.active ? (
                            <span className="text-[9px] uppercase opacity-70">off</span>
                          ) : null}
                        </Badge>
                      ))
                    )}
                  </div>

                  {/* allocation + risk summary */}
                  <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
                    <span className="text-muted-foreground">Active weight</span>
                    <span className="text-right font-mono tabular-nums">
                      {(aw * 100).toFixed(0)}%
                    </span>
                    <span className="text-muted-foreground">Cash reserve</span>
                    <span className="text-right font-mono tabular-nums">
                      {(m.cash_pct * 100).toFixed(0)}%
                    </span>
                    <span className="text-muted-foreground">Unallocated</span>
                    <span className="text-right font-mono tabular-nums">
                      {(remainder * 100).toFixed(0)}%
                    </span>
                    <span className="text-muted-foreground">
                      Risk (single / conc / halt)
                    </span>
                    <span className="text-right font-mono tabular-nums">
                      {(m.risk.single_name_pct * 100).toFixed(0)}/
                      {(m.risk.concentration_pct * 100).toFixed(0)}/
                      {(m.risk.daily_loss_halt_pct * 100).toFixed(0)}%
                    </span>
                  </div>

                  {/* actions */}
                  <div className="flex flex-wrap gap-2 pt-1">
                    <Button
                      size="sm"
                      onClick={() => onBacktest(m)}
                      data-testid={`composition-backtest-${m.id}`}
                    >
                      <FlaskConical />
                      Backtest
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => onHyperopt(m)}
                      data-testid={`composition-hyperopt-${m.id}`}
                    >
                      <Sparkles />
                      Hyperopt
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => onEdit(m)}
                      data-testid={`composition-edit-${m.id}`}
                    >
                      <Pencil />
                      Edit
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => remove(m)}
                      disabled={del.isPending}
                      data-testid={`composition-delete-${m.id}`}
                    >
                      <Trash2 />
                      Delete
                    </Button>
                  </div>
                </CardContent>
              </Card>
            );
          })}
        </div>
      )}
    </div>
  );
}
