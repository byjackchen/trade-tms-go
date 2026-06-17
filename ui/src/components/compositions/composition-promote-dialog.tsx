"use client";

import { useMemo, useState } from "react";
import { CheckCircle2 } from "lucide-react";
import { Sheet } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useComposition, useCompositionPromote } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import { formatNum } from "@/lib/format";
import type {
  Composition,
  CompositionPromoteResponse,
  TrialRow,
} from "@/lib/api/types";

/** Format a fraction as a percent string (or em-dash). */
function pct(v: unknown): string {
  if (typeof v !== "number" || !Number.isFinite(v)) return "—";
  return `${(v * 100).toFixed(1)}%`;
}

/**
 * The COMPOSITION promote dialog — DISTINCT from the strategy PromoteDialog. A
 * strategy promote writes signal params into active_params; this OVERWRITES the
 * composition IN PLACE (LOCKED DECISION 3): the composition's risk_* caps + each
 * member's weight + cash_pct, from the selected trial. It does NOT touch any
 * param_set (member strategy params stay fixed).
 *
 * Shown from the SHARED StudyPanel via its `renderPromote` prop, so the trials /
 * Pareto / study views are reused, not forked.
 */
export function CompositionPromoteDialog({
  open,
  onClose,
  compositionId,
  studyTS,
  trial,
}: {
  open: boolean;
  onClose: () => void;
  compositionId: string;
  studyTS: string;
  trial: TrialRow | null;
}) {
  const { data: compData } = useComposition(open ? compositionId : null);
  const composition: Composition | undefined = compData?.composition;
  const promote = useCompositionPromote(compositionId);

  const [result, setResult] = useState<CompositionPromoteResponse | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);

  // The trial's recorded dims (already NORMALIZED to a simplex server-side, LOCKED
  // DECISION 1a). The composition space records them as: a `weights` map
  // (strategy_id → weight) plus flat `cash_pct`, `single_name_pct`,
  // `concentration_pct`, `daily_loss_halt_pct` (internal/hyperopt/study/
  // composition_space.go RecordedParams). We show them as the proposed allocation/risk.
  const params = useMemo(
    () => (trial?.params ?? {}) as Record<string, unknown>,
    [trial],
  );
  const trialWeights = useMemo(() => {
    const w = params.weights;
    return w && typeof w === "object" ? (w as Record<string, unknown>) : {};
  }, [params]);

  const close = () => {
    setResult(null);
    setLocalError(null);
    onClose();
  };

  const confirm = async () => {
    if (!trial) return;
    setLocalError(null);
    try {
      const res = await promote.mutateAsync({
        study_ts: studyTS,
        trial_id: trial.number,
        actor: "ui",
      });
      setResult(res);
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Promotion failed.",
      );
    }
  };

  if (!open || !trial) return null;

  return (
    <Sheet
      open={open}
      onClose={close}
      title="Promote weights & risk"
      description="Overwrite this Composition IN PLACE with the trial's tuned weights + cash + risk caps. Member strategy params are NOT touched."
      data-testid="composition-promote-dialog"
      footer={
        result ? (
          <Button onClick={close} data-testid="composition-promote-done">
            Close
          </Button>
        ) : (
          <>
            <Button
              variant="ghost"
              onClick={close}
              data-testid="composition-promote-cancel"
            >
              Cancel
            </Button>
            <Button
              onClick={confirm}
              disabled={promote.isPending}
              data-testid="composition-promote-confirm"
            >
              {promote.isPending ? "Promoting…" : "Confirm promotion"}
            </Button>
          </>
        )
      }
    >
      {result ? (
        <div className="space-y-3" data-testid="composition-promote-success">
          <div className="flex items-center gap-2 text-sm">
            <CheckCircle2 className="size-4 text-emerald-500" />
            <span>
              Promoted trial #{result.trial_id} into Composition{" "}
              <span className="font-mono text-xs">{result.composition_id}</span>{" "}
              (now v{result.promoted.version}).
            </span>
          </div>
          <div className="grid grid-cols-2 gap-x-4 gap-y-1 rounded-lg border border-border px-3 py-2 text-xs">
            {Object.entries(result.promoted.weights).map(([sid, weight]) => (
              <span key={sid} className="contents">
                <span className="text-muted-foreground">{sid}</span>
                <span className="text-right font-mono tabular-nums">
                  {pct(weight)}
                </span>
              </span>
            ))}
            <span className="text-muted-foreground">cash</span>
            <span className="text-right font-mono tabular-nums">
              {pct(result.promoted.cash_pct)}
            </span>
            <span className="text-muted-foreground">
              risk (single / conc / halt)
            </span>
            <span className="text-right font-mono tabular-nums">
              {pct(result.promoted.single_name_pct)} /{" "}
              {pct(result.promoted.concentration_pct)} /{" "}
              {pct(result.promoted.daily_loss_halt_pct)}
            </span>
          </div>
          <p className="text-xs text-muted-foreground">
            The Composition&apos;s allocation + risk are overwritten in place;
            member param refs are unchanged. Effect is next-run-only.
          </p>
        </div>
      ) : (
        <div className="space-y-4" data-testid="composition-promote-form">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span className="text-muted-foreground">Target Composition:</span>
            <Badge variant="default">{compositionId}</Badge>
            <span className="text-muted-foreground">·</span>
            <span className="text-muted-foreground">trial</span>
            <Badge variant="secondary">#{trial.number}</Badge>
            <span className="ml-auto text-xs tabular-nums text-muted-foreground">
              sharpe {formatNum(trial.sharpe)} · calmar {formatNum(trial.calmar)}
            </span>
          </div>

          {trial.state !== "COMPLETE" ? (
            <Alert variant="warning" data-testid="composition-promote-not-complete">
              <AlertDescription>
                Only COMPLETE trials can be promoted (this trial is {trial.state}
                ).
              </AlertDescription>
            </Alert>
          ) : null}

          <div className="space-y-1.5">
            <div className="grid grid-cols-[1fr_auto] items-center gap-2 px-2 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
              <span>Dimension</span>
              <span className="text-right">Current → Tuned</span>
            </div>
            <div className="cockpit-scroll max-h-56 space-y-1 overflow-y-auto rounded-lg border border-border bg-background/60 p-2">
              {/* member weights */}
              {(composition?.members ?? []).map((m) => {
                const to = trialWeights[m.strategy_id];
                return (
                  <div
                    key={m.strategy_id}
                    className="flex items-center justify-between gap-3 px-1 text-xs"
                    data-testid={`composition-promote-weight-${m.strategy_id}`}
                  >
                    <span className="font-mono">weight · {m.strategy_id}</span>
                    <span className="tabular-nums">
                      <span className="text-muted-foreground">
                        {pct(m.weight)}
                      </span>
                      <span className="mx-1.5 text-muted-foreground">→</span>
                      <span className="font-semibold text-emerald-600 dark:text-emerald-400">
                        {pct(to)}
                      </span>
                    </span>
                  </div>
                );
              })}
              {/* cash + the three caps */}
              {(
                [
                  ["cash_pct", "cash", composition?.cash_pct],
                  [
                    "single_name_pct",
                    "risk · single name",
                    composition?.risk.single_name_pct,
                  ],
                  [
                    "concentration_pct",
                    "risk · concentration",
                    composition?.risk.concentration_pct,
                  ],
                  [
                    "daily_loss_halt_pct",
                    "risk · daily-loss halt",
                    composition?.risk.daily_loss_halt_pct,
                  ],
                ] as [string, string, number | undefined][]
              ).map(([key, label, from]) => (
                <div
                  key={key}
                  className="flex items-center justify-between gap-3 px-1 text-xs"
                  data-testid={`composition-promote-${key}`}
                >
                  <span className="font-mono">{label}</span>
                  <span className="tabular-nums">
                    <span className="text-muted-foreground">{pct(from)}</span>
                    <span className="mx-1.5 text-muted-foreground">→</span>
                    <span className="font-semibold text-emerald-600 dark:text-emerald-400">
                      {pct(params[key])}
                    </span>
                  </span>
                </div>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">
              Weights are normalized to a simplex (Σ weights + cash = 1). Member
              strategy params are FIXED at their active set and are not changed.
            </p>
          </div>

          {localError ? (
            <Alert variant="destructive" data-testid="composition-promote-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Sheet>
  );
}
