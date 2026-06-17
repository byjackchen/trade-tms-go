"use client";

import { useMemo, useState } from "react";
import { CheckCircle2 } from "lucide-react";
import { Dialog } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { usePromoteTrial, useStrategy } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import { formatNum } from "@/lib/format";
import type { TrialRow, PromoteResponse } from "@/lib/api/types";

/** Render a param scalar compactly for the from→to diff. */
function fmt(v: unknown): string {
  if (v == null) return "—";
  if (typeof v === "number")
    return Number.isInteger(v) ? String(v) : String(Number(v.toFixed(6)));
  if (typeof v === "boolean") return v ? "true" : "false";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

/**
 * Promotion confirmation dialog. Shows which strategy's active_params change and
 * the from→to diff per param, then POSTs the promotion (this replaces the Python
 * git-review gate). `joint` studies promote every sub-strategy; the dialog notes
 * that and shows the trial's params (the server maps them per sub-strategy).
 */
export function PromoteDialog({
  open,
  onClose,
  studyTS,
  trial,
}: {
  open: boolean;
  onClose: () => void;
  studyTS: string;
  trial: TrialRow | null;
}) {
  const isJoint = trial?.strategy === "joint";
  // For a single-strategy study we can show the live active_values diff.
  const { data: stratData } = useStrategy(
    open && trial && !isJoint ? trial.strategy : null,
  );
  const promote = usePromoteTrial(studyTS);

  const [result, setResult] = useState<PromoteResponse | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);

  const activeValues = stratData?.strategy.active_values;
  const newParams = trial?.params;

  // Param rows: union of the trial's params and the strategy's active values.
  const rows = useMemo(() => {
    const av = (activeValues ?? {}) as Record<string, unknown>;
    const np = (newParams ?? {}) as Record<string, unknown>;
    const keys = new Set<string>();
    for (const k of Object.keys(np)) keys.add(k);
    if (!isJoint) for (const k of Object.keys(av)) keys.add(k);
    return [...keys].sort().map((k) => ({
      key: k,
      from: av[k],
      to: np[k],
      hasFrom: Object.prototype.hasOwnProperty.call(av, k),
      hasTo: Object.prototype.hasOwnProperty.call(np, k),
    }));
  }, [newParams, activeValues, isJoint]);

  const close = () => {
    setResult(null);
    setLocalError(null);
    onClose();
  };

  const confirm = async () => {
    if (!trial) return;
    setLocalError(null);
    try {
      const res = await promote.mutateAsync({ trial_id: trial.number, actor: "ui" });
      setResult(res);
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Promotion failed.",
      );
    }
  };

  if (!open || !trial) return null;

  return (
    <Dialog
      open={open}
      onClose={close}
      title="Promote params"
      description="Set this trial's params as the strategy's active_params (next-run-only). This replaces the manual git-review gate."
      data-testid="promote-dialog"
      footer={
        result ? (
          <Button onClick={close} data-testid="promote-done">
            Close
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={close} data-testid="promote-cancel">
              Cancel
            </Button>
            <Button
              onClick={confirm}
              disabled={promote.isPending}
              data-testid="hyperopt-promote-confirm"
            >
              {promote.isPending ? "Promoting…" : "Confirm promotion"}
            </Button>
          </>
        )
      }
    >
      {result ? (
        <div className="space-y-3" data-testid="hyperopt-promote-success">
          <div className="flex items-center gap-2 text-sm">
            <CheckCircle2 className="size-4 text-emerald-500" />
            <span>
              Promoted trial #{result.trial_id} from study{" "}
              <span className="font-mono text-xs">{result.study_ts}</span>.
            </span>
          </div>
          <div className="space-y-1.5">
            {result.promoted.map((p) => (
              <div
                key={p.strategy}
                className="flex items-center justify-between rounded-lg border border-border px-3 py-2 text-sm"
                data-testid={`promoted-${p.strategy}`}
              >
                <Badge variant="outline">{p.strategy}</Badge>
                <span className="text-xs text-muted-foreground">
                  param_set #{p.param_set_id} · v{p.version} · now active
                </span>
              </div>
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            Effect is next-run-only — live processes read params at startup. The
            audit (promoted_by / promoted_at / source_study / source_trial) is
            recorded on the active_params row.
          </p>
        </div>
      ) : (
        <div className="space-y-4" data-testid="promote-form">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span className="text-muted-foreground">Target strategy:</span>
            <Badge variant="default">{trial.strategy}</Badge>
            <span className="text-muted-foreground">·</span>
            <span className="text-muted-foreground">trial</span>
            <Badge variant="secondary">#{trial.number}</Badge>
            <span className="ml-auto text-xs tabular-nums text-muted-foreground">
              sharpe {formatNum(trial.sharpe)} · calmar {formatNum(trial.calmar)}
            </span>
          </div>

          {isJoint ? (
            <Alert data-testid="promote-joint-note">
              <AlertDescription>
                This is a joint study — promoting updates the active_params of{" "}
                <strong>every</strong> sub-strategy (sepa, sector_rotation,
                pairs). The tuned params below are mapped per sub-strategy
                server-side.
              </AlertDescription>
            </Alert>
          ) : null}

          {trial.state !== "COMPLETE" ? (
            <Alert variant="warning" data-testid="promote-not-complete">
              <AlertDescription>
                Only COMPLETE trials can be promoted (this trial is{" "}
                {trial.state}).
              </AlertDescription>
            </Alert>
          ) : null}

          <div className="space-y-1.5">
            <div className="grid grid-cols-[1fr_auto_1fr] items-center gap-2 px-2 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
              <span>Param</span>
              <span className="text-right">{isJoint ? "Tuned" : "From → To"}</span>
              <span />
            </div>
            <div className="cockpit-scroll max-h-56 space-y-1 overflow-y-auto rounded-lg border border-border bg-background/60 p-2">
              {rows.length === 0 ? (
                <p className="px-1 py-2 text-xs text-muted-foreground">
                  No tunable params on this trial.
                </p>
              ) : (
                rows.map((r) => {
                  const changed =
                    !isJoint && r.hasFrom && r.hasTo && fmt(r.from) !== fmt(r.to);
                  return (
                    <div
                      key={r.key}
                      className="flex items-center justify-between gap-3 px-1 text-xs"
                      data-testid={`promote-param-${r.key}`}
                    >
                      <span className="font-mono">{r.key}</span>
                      {isJoint ? (
                        <span className="tabular-nums font-medium">
                          {fmt(r.to)}
                        </span>
                      ) : (
                        <span className="tabular-nums">
                          <span className="text-muted-foreground">{fmt(r.from)}</span>
                          <span className="mx-1.5 text-muted-foreground">→</span>
                          <span
                            className={
                              changed
                                ? "font-semibold text-emerald-600 dark:text-emerald-400"
                                : "font-medium"
                            }
                          >
                            {fmt(r.to)}
                          </span>
                        </span>
                      )}
                    </div>
                  );
                })
              )}
            </div>
            {!isJoint ? (
              <p className="text-xs text-muted-foreground">
                Current values come from{" "}
                <span className="font-mono">{trial.strategy}</span>&apos;s active
                params ({stratData?.strategy.params_source ?? "…"}).
              </p>
            ) : null}
          </div>

          {localError ? (
            <Alert variant="destructive" data-testid="promote-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Dialog>
  );
}
