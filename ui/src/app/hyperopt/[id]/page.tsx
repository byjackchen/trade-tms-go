"use client";

import { useMemo, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { buttonVariants } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ErrorState, LoadingRows } from "@/components/shell/states";
import { StreamIndicator } from "@/components/data/stream-indicator";
import { MetricCard } from "@/components/backtests/metric-card";
import { StudyStatusBadge } from "@/components/hyperopt/status-badge";
import { ParetoChart } from "@/components/hyperopt/pareto-chart";
import { ConvergenceChart } from "@/components/hyperopt/convergence-chart";
import { WalkForwardViz } from "@/components/hyperopt/walkforward-viz";
import { TrialsTable } from "@/components/hyperopt/trials-table";
import { PromoteDialog } from "@/components/hyperopt/promote-dialog";
import { useStudy, useStudyTrials } from "@/lib/api/hooks";
import { formatInt, formatNum, formatTs } from "@/lib/format";
import type { TrialRow } from "@/lib/api/types";

const TS_RE = /^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}$/;

export default function StudyDetailPage() {
  const params = useParams<{ id: string }>();
  const ts = useMemo(() => {
    const raw = params?.id ? decodeURIComponent(params.id) : "";
    return TS_RE.test(raw) ? raw : null;
  }, [params?.id]);

  const study = useStudy(ts);
  const status = study.data?.study.progress.status;
  const trials = useStudyTrials(ts, status);

  const [selected, setSelected] = useState<number | null>(null);
  const [promoteTrial, setPromoteTrial] = useState<TrialRow | null>(null);

  const trialRows = useMemo(
    () => trials.data?.trials ?? [],
    [trials.data],
  );

  // A representative fold breakdown for the walk-forward viz: the first
  // COMPLETE trial that carries fold date ranges.
  const wfFolds = useMemo(() => {
    const t = trialRows.find(
      (r) => r.folds?.some((f) => f.test_start && f.test_end),
    );
    return t?.folds ?? [];
  }, [trialRows]);

  const back = (
    <Link
      href="/hyperopt"
      className={buttonVariants({ variant: "ghost", size: "sm" })}
      data-testid="study-back"
    >
      <ArrowLeft />
      Studies
    </Link>
  );

  const s = study.data?.study;

  return (
    <>
      <PageHeader
        title={
          <span className="flex items-center gap-2">
            {s ? "Study" : "Study"}
            {s ? <StudyStatusBadge status={s.progress.status} /> : null}
          </span>
        }
        subtitle={
          s
            ? `${s.config.strategy} · ${s.config.start} → ${s.config.end}`
            : ts ?? undefined
        }
        data-testid="study-detail-header"
        actions={
          <>
            <StreamIndicator />
            {back}
          </>
        }
      />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="hyperopt-detail"
        data-study-ts={ts ?? undefined}
      >
        {ts == null ? (
          <ErrorState
            error={new Error("Invalid study id (want %Y-%m-%d_%H-%M-%S).")}
            data-testid="study-bad-id"
          />
        ) : study.isLoading ? (
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
      </main>

      {promoteTrial && ts ? (
        <PromoteDialog
          open={promoteTrial != null}
          onClose={() => setPromoteTrial(null)}
          studyTS={ts}
          trial={promoteTrial}
        />
      ) : null}
    </>
  );
}
