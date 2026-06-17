"use client";

import { useMemo, useState, type ReactNode } from "react";
import { Rocket, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ErrorState, LoadingRows } from "@/components/shell/states";
import { MetricCard } from "@/components/compositions/metric-card";
import { StudyStatusBadge } from "./status-badge";
import { ParetoChart } from "./pareto-chart";
import { ConvergenceChart } from "./convergence-chart";
import { WalkForwardViz } from "./walkforward-viz";
import { TrialsTable } from "./trials-table";
import { PromoteDialog } from "./promote-dialog";
import { useStudy, useStudyTrials } from "@/lib/api/hooks";
import { formatInt, formatNum, formatTs } from "@/lib/format";
import type { TrialRow } from "@/lib/api/types";

/**
 * A render-prop that owns the promotion flow for a selected trial. The default
 * (strategy) flow renders the per-strategy `PromoteDialog` (writes active_params);
 * the Compositions module passes its own to promote weights+risk IN PLACE. This is
 * how the study/trials/Pareto views are SHARED, not forked, across the two hyperopt
 * flavours (Strategy Hyperopt — signal params · Composition Hyperopt — weights & risk).
 */
export type PromoteRenderProp = (args: {
  trial: TrialRow;
  studyTS: string;
  onClose: () => void;
}) => ReactNode;

/** Copy that distinguishes what promoting a trial DOES (signal params vs weights+risk). */
const DEFAULT_PROMOTE_COPY =
  "Promote to set its params as the strategy's active set.";

/**
 * The study detail, rendered INLINE (formerly the `/hyperopt/[id]` route).
 * Per the "inline results" decision (docs/concept-alignment.md §3.4) the study
 * no longer navigates to its own page — selecting a row in the Tune panel opens
 * this panel in place below the studies table. `ts` is the study timestamp id
 * (already validated by the caller / the row that produced it).
 *
 * SHARED by Strategy Hyperopt (signal params) and Composition Hyperopt (weights &
 * risk): pass `renderPromote` to swap the promotion flow + `promoteCopy` to change
 * the CTA wording; both default to the per-strategy promote.
 */
export function StudyPanel({
  ts,
  onClose,
  renderPromote,
  promoteCopy = DEFAULT_PROMOTE_COPY,
}: {
  ts: string;
  onClose?: () => void;
  /** Override the promotion flow (Compositions promotes weights+risk in place). */
  renderPromote?: PromoteRenderProp;
  /** CTA wording in the "complete — ready to promote" banner. */
  promoteCopy?: string;
}) {
  const study = useStudy(ts);
  const status = study.data?.study.progress.status;
  const trials = useStudyTrials(ts, status);

  const [selected, setSelected] = useState<number | null>(null);
  const [promoteTrial, setPromoteTrial] = useState<TrialRow | null>(null);

  const trialRows = useMemo(() => trials.data?.trials ?? [], [trials.data]);

  // The most promotable trial: a COMPLETE trial, preferring the Pareto front,
  // then the highest Sharpe. Surfaced in the "study complete" promote banner so
  // promoting a tuned param set is obvious — not buried in the trials table.
  const bestTrial = useMemo(() => {
    const complete = trialRows.filter((r) => r.state === "COMPLETE");
    if (complete.length === 0) return null;
    return [...complete].sort((a, b) => {
      if (a.pareto_front !== b.pareto_front) return a.pareto_front ? -1 : 1;
      return (b.sharpe ?? -Infinity) - (a.sharpe ?? -Infinity);
    })[0];
  }, [trialRows]);

  // A representative fold breakdown for the walk-forward viz: the first
  // COMPLETE trial that carries fold date ranges.
  const wfFolds = useMemo(() => {
    const t = trialRows.find((r) =>
      r.folds?.some((f) => f.test_start && f.test_end),
    );
    return t?.folds ?? [];
  }, [trialRows]);

  const s = study.data?.study;

  return (
    <div
      className="space-y-4"
      data-testid="hyperopt-detail"
      data-study-ts={ts}
    >
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">Study</span>
          {s ? <StudyStatusBadge status={s.progress.status} /> : null}
          <span className="font-mono text-xs text-muted-foreground">{ts}</span>
        </div>
        {onClose ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={onClose}
            data-testid="study-panel-close"
          >
            <X />
            Close
          </Button>
        ) : null}
      </div>

      {study.isLoading ? (
        <div className="space-y-4">
          <LoadingRows rows={2} data-testid="study-loading" />
          <LoadingRows rows={6} />
        </div>
      ) : study.error ? (
        <ErrorState
          error={study.error}
          onRetry={() => study.refetch()}
          data-testid="study-error"
        />
      ) : !s ? (
        <ErrorState
          error={new Error("Study not found.")}
          data-testid="study-empty"
        />
      ) : (
        <>
          {/* config + progress summary */}
          <div
            className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6"
            data-testid="study-summary"
          >
            <MetricCard
              label="Completed"
              value={`${formatInt(s.progress.completed_trials)}/${formatInt(s.progress.total_trials)}`}
              rawValue={s.progress.completed_trials}
              sub={
                s.progress.failed_trials > 0
                  ? `${formatInt(s.progress.failed_trials)} failed`
                  : s.progress.running_trials > 0
                    ? `${formatInt(s.progress.running_trials)} running`
                    : "all done"
              }
              data-testid="study-metric-completed"
            />
            <MetricCard
              label="Best Sharpe"
              value={s.progress.current_best ? formatNum(s.progress.current_best.sharpe) : "—"}
              rawValue={s.progress.current_best?.sharpe}
              sub={
                s.progress.current_best
                  ? `trial #${s.progress.current_best.trial}`
                  : undefined
              }
              data-testid="study-metric-best-sharpe"
            />
            <MetricCard
              label="Best Calmar"
              value={s.progress.current_best ? formatNum(s.progress.current_best.calmar) : "—"}
              rawValue={s.progress.current_best?.calmar}
              data-testid="study-metric-best-calmar"
            />
            <MetricCard
              label="Seed"
              value={formatInt(s.config.seed)}
              rawValue={s.config.seed}
              sub="deterministic"
              data-testid="study-metric-seed"
            />
            <MetricCard
              label="Workers"
              value={formatInt(s.config.workers)}
              rawValue={s.config.workers}
              data-testid="study-metric-workers"
            />
            <MetricCard
              label="Walk-forward"
              value={
                s.config.walk_forward.enabled
                  ? `${s.config.walk_forward.folds} folds`
                  : "off"
              }
              sub={
                s.config.walk_forward.enabled
                  ? `${s.config.walk_forward.embargo_days}d embargo`
                  : undefined
              }
              data-testid="study-metric-wf"
            />
          </div>

          {/* objectives + meta chips */}
          <div className="flex flex-wrap items-center gap-2 text-xs" data-testid="study-meta">
            <span className="text-muted-foreground">Objectives:</span>
            {s.config.objectives.map((o, i) => (
              <Badge key={o} variant="secondary" data-testid={`study-objective-${o}`}>
                {o} {s.config.directions[i] === "minimize" ? "↓" : "↑"}{" "}
                {s.config.directions[i] ?? "maximize"}
              </Badge>
            ))}
            <span className="ml-auto text-muted-foreground">
              updated {formatTs(s.config.updated_at)}
            </span>
            {s.progress.last_error ? (
              <span className="basis-full text-destructive" data-testid="study-last-error">
                {s.progress.last_error}
              </span>
            ) : null}
          </div>

          {/* Prominent promote CTA once the study is COMPLETE: promoting a
              tuned trial's params is the whole point of a hyperopt study, so we
              surface the best trial + a Promote button up top rather than only
              in the trials table below. */}
          {s.progress.status === "COMPLETE" && bestTrial ? (
            <Alert
              variant="success"
              className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"
              data-testid="study-promote-cta"
            >
              <div>
                <AlertTitle>Hyperopt complete — ready to promote</AlertTitle>
                <AlertDescription>
                  Best trial #{bestTrial.number}
                  {bestTrial.pareto_front ? " (Pareto front)" : ""} — sharpe{" "}
                  <span className="font-mono tabular-nums">
                    {formatNum(bestTrial.sharpe)}
                  </span>{" "}
                  · calmar{" "}
                  <span className="font-mono tabular-nums">
                    {formatNum(bestTrial.calmar)}
                  </span>
                  . {promoteCopy}
                </AlertDescription>
              </div>
              <Button
                onClick={() => setPromoteTrial(bestTrial)}
                className="shrink-0"
                data-testid="study-promote-best"
              >
                <Rocket />
                Promote best trial
              </Button>
            </Alert>
          ) : null}

          {/* charts: pareto + convergence */}
          <div className="grid gap-4 lg:grid-cols-2">
            <Card data-testid="pareto-card">
              <CardHeader>
                <CardTitle>Pareto front</CardTitle>
                <CardDescription>
                  Sharpe vs Calmar — non-dominated trials highlighted.
                </CardDescription>
              </CardHeader>
              <CardContent>
                {trials.isLoading ? (
                  <LoadingRows rows={4} data-testid="pareto-loading" />
                ) : trials.error ? (
                  <ErrorState
                    error={trials.error}
                    onRetry={() => trials.refetch()}
                    data-testid="pareto-error"
                  />
                ) : (
                  <ParetoChart
                    trials={trialRows}
                    selected={selected}
                    onSelect={setSelected}
                    data-testid="pareto-scatter"
                  />
                )}
              </CardContent>
            </Card>

            <Card data-testid="convergence-card">
              <CardHeader>
                <CardTitle>Convergence</CardTitle>
                <CardDescription>
                  Best objective achieved as trials accumulate.
                </CardDescription>
              </CardHeader>
              <CardContent>
                {trials.isLoading ? (
                  <LoadingRows rows={4} data-testid="convergence-loading" />
                ) : (
                  <ConvergenceChart trials={trialRows} />
                )}
              </CardContent>
            </Card>
          </div>

          {/* walk-forward fold viz */}
          <Card data-testid="walkforward-card">
            <CardHeader>
              <CardTitle>Walk-forward folds</CardTitle>
              <CardDescription>
                Anchored-expanding train windows, embargo, out-of-sample test.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <WalkForwardViz folds={wfFolds} config={s.config.walk_forward} />
            </CardContent>
          </Card>

          {/* trials table */}
          <Card data-testid="trials-card">
            <CardHeader>
              <CardTitle>Trials</CardTitle>
              <CardDescription>
                Pareto-front trials first. Expand a row for params + per-fold
                metrics; promote a completed trial&apos;s params.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {trials.isLoading ? (
                <LoadingRows rows={6} data-testid="trials-loading" />
              ) : trials.error ? (
                <ErrorState
                  error={trials.error}
                  onRetry={() => trials.refetch()}
                  data-testid="trials-error"
                />
              ) : trialRows.length === 0 ? (
                <p
                  className="py-6 text-center text-sm text-muted-foreground"
                  data-testid="trials-empty"
                >
                  {s.progress.status === "RUNNING"
                    ? "Trials will appear here as the study runs…"
                    : "No trials recorded for this study."}
                </p>
              ) : (
                <TrialsTable
                  trials={trialRows}
                  selected={selected}
                  onSelect={setSelected}
                  onPromote={setPromoteTrial}
                />
              )}
            </CardContent>
          </Card>
        </>
      )}

      {promoteTrial ? (
        renderPromote ? (
          renderPromote({
            trial: promoteTrial,
            studyTS: ts,
            onClose: () => setPromoteTrial(null),
          })
        ) : (
          <PromoteDialog
            open={promoteTrial != null}
            onClose={() => setPromoteTrial(null)}
            studyTS={ts}
            trial={promoteTrial}
          />
        )
      ) : null}
    </div>
  );
}
