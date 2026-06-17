"use client";

import * as React from "react";
import { EmptyState } from "@/components/shell/states";
import type { TrialFold, WalkForwardConfig } from "@/lib/api/types";

type Band = {
  fold: number;
  trainStart: number;
  trainEnd: number;
  testStart: number;
  testEnd: number;
  trainStartStr: string;
  trainEndStr: string;
  testStartStr: string;
  testEndStr: string;
};

function toMs(d: string | undefined): number | null {
  if (!d) return null;
  const t = Date.parse(d.length === 10 ? `${d}T00:00:00Z` : d);
  return Number.isNaN(t) ? null : t;
}

/**
 * Walk-forward fold visualization. Each fold is a horizontal band: the
 * anchored-expanding train window (solid), an embargo gap, then the out-of-
 * sample test window (accent). Fold boundaries are read from a representative
 * trial's per-fold breakdown (folds[].{train,test}_{start,end}) — the same
 * deterministic splits, golden-gated against the canonical splitter.
 */
export function WalkForwardViz({
  folds,
  config,
  "data-testid": testId = "walkforward-viz",
}: {
  folds: TrialFold[];
  config: WalkForwardConfig;
  "data-testid"?: string;
}) {
  const bands = React.useMemo<Band[]>(() => {
    const out: Band[] = [];
    for (const f of folds) {
      const ts = toMs(f.train_start);
      const te = toMs(f.train_end);
      const vs = toMs(f.test_start);
      const ve = toMs(f.test_end);
      if (ts == null || te == null || vs == null || ve == null) continue;
      out.push({
        fold: f.fold,
        trainStart: ts,
        trainEnd: te,
        testStart: vs,
        testEnd: ve,
        trainStartStr: (f.train_start as string).slice(0, 10),
        trainEndStr: (f.train_end as string).slice(0, 10),
        testStartStr: (f.test_start as string).slice(0, 10),
        testEndStr: (f.test_end as string).slice(0, 10),
      });
    }
    return out.sort((a, b) => a.fold - b.fold);
  }, [folds]);

  const { lo, hi } = React.useMemo(() => {
    let mn = Infinity,
      mx = -Infinity;
    for (const b of bands) {
      mn = Math.min(mn, b.trainStart);
      mx = Math.max(mx, b.testEnd);
    }
    return { lo: mn, hi: mx };
  }, [bands]);

  if (!config.enabled) {
    return (
      <EmptyState
        title="Walk-forward disabled"
        hint="Each candidate is scored on a single full-window backtest."
        data-testid={`${testId}-disabled`}
      />
    );
  }
  if (bands.length === 0) {
    return (
      <EmptyState
        title="No fold breakdown yet"
        hint="Fold boundaries appear once a trial completes."
        data-testid={`${testId}-empty`}
      />
    );
  }

  const span = hi - lo || 1;
  const pct = (ms: number) => ((ms - lo) / span) * 100;

  return (
    <div className="space-y-2" data-testid={testId} data-fold-count={bands.length}>
      <div className="flex items-center gap-4 text-xs text-muted-foreground">
        <span className="flex items-center gap-1.5">
          <span className="inline-block h-2.5 w-4 rounded-sm bg-primary/70" />
          Train (anchored-expanding)
        </span>
        <span className="flex items-center gap-1.5">
          <span className="inline-block h-2.5 w-4 rounded-sm bg-muted" />
          Embargo {config.embargo_days}d
        </span>
        <span className="flex items-center gap-1.5">
          <span className="inline-block h-2.5 w-4 rounded-sm bg-emerald-500/70" />
          Test (out-of-sample)
        </span>
      </div>

      <div className="space-y-1.5">
        {bands.map((b) => {
          const trainL = pct(b.trainStart);
          const trainW = pct(b.trainEnd) - trainL;
          const testL = pct(b.testStart);
          const testW = pct(b.testEnd) - testL;
          const embL = pct(b.trainEnd);
          const embW = testL - embL;
          return (
            <div
              key={b.fold}
              className="flex items-center gap-2"
              data-testid={`wf-fold-${b.fold}`}
            >
              <span className="w-12 shrink-0 text-xs font-medium text-muted-foreground">
                fold {b.fold}
              </span>
              <div className="relative h-6 flex-1 rounded bg-background/40 ring-1 ring-border">
                <div
                  className="absolute top-0.5 bottom-0.5 rounded-sm bg-primary/70"
                  style={{ left: `${trainL}%`, width: `${Math.max(trainW, 0.5)}%` }}
                  title={`train ${b.trainStartStr} → ${b.trainEndStr}`}
                  data-testid={`wf-train-${b.fold}`}
                />
                {embW > 0.2 ? (
                  <div
                    className="absolute top-0.5 bottom-0.5 bg-muted"
                    style={{ left: `${embL}%`, width: `${embW}%` }}
                    title={`embargo ${config.embargo_days}d`}
                  />
                ) : null}
                <div
                  className="absolute top-0.5 bottom-0.5 rounded-sm bg-emerald-500/70"
                  style={{ left: `${testL}%`, width: `${Math.max(testW, 0.5)}%` }}
                  title={`test ${b.testStartStr} → ${b.testEndStr}`}
                  data-testid={`wf-test-${b.fold}`}
                />
              </div>
              <span className="hidden w-44 shrink-0 text-right text-[10px] tabular-nums text-muted-foreground sm:block">
                {b.testStartStr} → {b.testEndStr}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
