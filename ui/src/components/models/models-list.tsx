"use client";

import { FlaskConical, Sparkles, Pencil, Trash2, Plus } from "lucide-react";
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
import { useModels, useDeleteModel } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import type { Model } from "@/lib/api/types";

const STRATEGY_LABEL: Record<string, string> = {
  sepa: "SEPA",
  sector_rotation: "Sector",
  pairs: "Pairs",
  intraday_breakout: "ORB",
};

function activeWeight(model: Model): number {
  return model.members
    .filter((m) => m.active)
    .reduce((sum, m) => sum + m.weight, 0);
}

/**
 * The Models list (docs/concept-alignment.md §3.4 ③). Each Model is a card with
 * its members + weights + cash + risk caps and the four per-Model actions:
 * Backtest, Optimize, Edit, Delete. Backtest/Optimize/Edit are delegated to the
 * page (which owns the dialogs + inline panels); Delete is handled inline.
 */
export function ModelsList({
  onNew,
  onEdit,
  onBacktest,
  onOptimize,
}: {
  onNew: () => void;
  onEdit: (model: Model) => void;
  onBacktest: (model: Model) => void;
  onOptimize: (model: Model) => void;
}) {
  const { data, isLoading, error, refetch } = useModels();
  const del = useDeleteModel();
  const models = data?.models ?? [];

  const remove = (model: Model) => {
    if (
      !window.confirm(
        `Delete Model "${model.name}" (${model.id})? This cannot be undone.`,
      )
    ) {
      return;
    }
    del.mutate({ id: model.id, actor: "ui" });
  };

  if (isLoading) {
    return <LoadingRows rows={4} data-testid="models-loading" />;
  }

  // A 503 means the model store is not wired (an expected degraded state).
  if (error) {
    const status = error instanceof ApiError ? error.status : undefined;
    if (status === 503) {
      return (
        <Alert data-testid="models-unavailable">
          <AlertDescription>
            The Model store is not configured on this server yet. Models become
            available once the backend store is wired.
          </AlertDescription>
        </Alert>
      );
    }
    return (
      <ErrorState error={error} onRetry={() => refetch()} data-testid="models-error" />
    );
  }

  return (
    <div className="space-y-4" data-testid="models-list">
      <div className="flex items-center justify-between gap-2">
        <p className="text-sm text-muted-foreground">
          {models.length} Model{models.length === 1 ? "" : "s"} — named portfolio
          blueprints the engine drops in for backtest, optimize, paper and live.
        </p>
        <Button size="sm" onClick={onNew} data-testid="models-new">
          <Plus />
          New Model
        </Button>
      </div>

      {del.isError ? (
        <Alert variant="destructive" data-testid="models-delete-error">
          <AlertDescription>
            {del.error instanceof ApiError
              ? del.error.message
              : "Failed to delete Model."}
          </AlertDescription>
        </Alert>
      ) : null}

      {models.length === 0 ? (
        <EmptyState
          title="No Models yet"
          hint='Click "New Model" to compose your first portfolio blueprint.'
          data-testid="models-empty"
        />
      ) : (
        <div className="grid gap-4 lg:grid-cols-2" data-testid="models-grid">
          {models.map((m) => {
            const aw = activeWeight(m);
            const remainder = 1 - aw - m.cash_pct;
            return (
              <Card key={m.id} data-testid={`model-card-${m.id}`}>
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
                  <div className="flex flex-wrap gap-1.5" data-testid={`model-members-${m.id}`}>
                    {m.members.length === 0 ? (
                      <span className="text-xs text-muted-foreground">No members.</span>
                    ) : (
                      m.members.map((mem) => (
                        <Badge
                          key={mem.strategy_id}
                          variant={mem.active ? "secondary" : "muted"}
                          className="gap-1"
                          data-testid={`model-member-${m.id}-${mem.strategy_id}`}
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
                      data-testid={`model-backtest-${m.id}`}
                    >
                      <FlaskConical />
                      Backtest
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => onOptimize(m)}
                      data-testid={`model-optimize-${m.id}`}
                    >
                      <Sparkles />
                      Optimize
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => onEdit(m)}
                      data-testid={`model-edit-${m.id}`}
                    >
                      <Pencil />
                      Edit
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => remove(m)}
                      disabled={del.isPending}
                      data-testid={`model-delete-${m.id}`}
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
