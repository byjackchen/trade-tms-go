"use client";

import * as React from "react";
import { formatNum } from "@/lib/format";
import type { TrialRow } from "@/lib/api/types";

const PAD = { top: 12, right: 14, bottom: 26, left: 46 };
const VB_W = 760;
const VB_H = 240;

type Series = { id: string; label: string; color: string; vals: number[] };

/**
 * Best-objective-over-time (convergence) chart. For COMPLETE trials ordered by
 * trial number we plot the running maximum of each objective — the monotonic
 * staircase NSGA-II drives upward as generations improve the front.
 */
export function ConvergenceChart({
  trials,
  "data-testid": testId = "convergence-chart",
}: {
  trials: TrialRow[];
  "data-testid"?: string;
}) {
  const { series, n } = React.useMemo(() => {
    const ordered = trials
      .filter(
        (t) =>
          t.state === "COMPLETE" &&
          typeof t.sharpe === "number" &&
          typeof t.calmar === "number",
      )
      .sort((a, b) => a.number - b.number);

    let bestSharpe = -Infinity;
    let bestCalmar = -Infinity;
    const sharpe: number[] = [];
    const calmar: number[] = [];
    for (const t of ordered) {
      bestSharpe = Math.max(bestSharpe, t.sharpe as number);
      bestCalmar = Math.max(bestCalmar, t.calmar as number);
      sharpe.push(bestSharpe);
      calmar.push(bestCalmar);
    }
    return {
      series: [
        { id: "sharpe", label: "Best Sharpe", color: "var(--primary)", vals: sharpe },
        { id: "calmar", label: "Best Calmar", color: "#10b981", vals: calmar },
      ] as Series[],
      n: ordered.length,
    };
  }, [trials]);

  const { yMin, yMax } = React.useMemo(() => {
    let lo = Infinity,
      hi = -Infinity;
    for (const s of series)
      for (const v of s.vals) {
        if (v < lo) lo = v;
        if (v > hi) hi = v;
      }
    if (!Number.isFinite(lo) || !Number.isFinite(hi)) return { yMin: 0, yMax: 1 };
    if (lo === hi) {
      const pad = Math.abs(lo) > 0 ? Math.abs(lo) * 0.1 : 1;
      return { yMin: lo - pad, yMax: hi + pad };
    }
    const pad = (hi - lo) * 0.08;
    return { yMin: lo - pad, yMax: hi + pad };
  }, [series]);

  if (n === 0) {
    return (
      <div
        className="flex h-32 items-center justify-center text-sm text-muted-foreground"
        data-testid={`${testId}-empty`}
      >
        No completed trials yet.
      </div>
    );
  }

  const plotW = VB_W - PAD.left - PAD.right;
  const plotH = VB_H - PAD.top - PAD.bottom;
  const xOf = (i: number) =>
    PAD.left + (n > 1 ? i / (n - 1) : 0) * plotW;
  const yOf = (v: number) =>
    PAD.top + (1 - (v - yMin) / (yMax - yMin)) * plotH;

  const yTicks = [0, 1, 2, 3, 4].map((i) => {
    const v = yMin + (i / 4) * (yMax - yMin);
    return { v, y: yOf(v) };
  });

  return (
    <div className="w-full" data-testid={testId} data-trial-count={n}>
      <svg
        viewBox={`0 0 ${VB_W} ${VB_H}`}
        className="h-52 w-full"
        preserveAspectRatio="none"
        role="img"
        aria-label="Convergence: best objective over trials"
      >
        {yTicks.map((t, i) => (
          <g key={i}>
            <line
              x1={PAD.left}
              x2={VB_W - PAD.right}
              y1={t.y}
              y2={t.y}
              className="stroke-border"
              strokeWidth={1}
              opacity={0.5}
            />
            <text
              x={PAD.left - 6}
              y={t.y + 3}
              textAnchor="end"
              className="fill-muted-foreground"
              fontSize={10}
            >
              {formatNum(t.v, 1)}
            </text>
          </g>
        ))}
        {series.map((s) => {
          if (s.vals.length === 0) return null;
          const d = s.vals
            .map(
              (v, i) =>
                `${i === 0 ? "M" : "L"}${xOf(i).toFixed(1)},${yOf(v).toFixed(1)}`,
            )
            .join(" ");
          return (
            <path
              key={s.id}
              d={d}
              fill="none"
              stroke={s.color}
              strokeWidth={1.75}
              strokeLinejoin="round"
              vectorEffect="non-scaling-stroke"
              data-testid={`convergence-series-${s.id}`}
            />
          );
        })}
      </svg>
      <div className="mt-1 flex flex-wrap items-center gap-x-4 text-xs">
        {series.map((s) => (
          <span key={s.id} className="flex items-center gap-1.5">
            <span
              className="inline-block size-2.5 rounded-sm"
              style={{ background: s.color }}
            />
            <span className="text-muted-foreground">{s.label}</span>
            <span className="tabular-nums font-medium">
              {formatNum(s.vals[s.vals.length - 1])}
            </span>
          </span>
        ))}
        <span className="ml-auto text-muted-foreground">{n} completed trials</span>
      </div>
    </div>
  );
}
