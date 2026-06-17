"use client";

import * as React from "react";
import { formatNum } from "@/lib/format";
import type { TrialRow } from "@/lib/api/types";

const PAD = { top: 14, right: 16, bottom: 34, left: 52 };
const VB_W = 760;
const VB_H = 360;

type Pt = {
  number: number;
  x: number; // sharpe
  y: number; // calmar
  px: number;
  py: number;
  pareto: boolean;
};

/** A nice domain padded by 6% on each side; guards against a degenerate range. */
function paddedRange(lo: number, hi: number): [number, number] {
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) return [0, 1];
  if (lo === hi) {
    const pad = Math.abs(lo) > 0 ? Math.abs(lo) * 0.1 : 1;
    return [lo - pad, hi + pad];
  }
  const pad = (hi - lo) * 0.06;
  return [lo - pad, hi + pad];
}

/**
 * Dependency-free Pareto-front scatter: each COMPLETE trial is a point at
 * (sharpe, calmar). Pareto-front points (non-dominated, both objectives
 * maximized) are highlighted and connected by the front staircase. Hover/click
 * surfaces a trial; `selected` ties it to the trials table.
 */
export function ParetoChart({
  trials,
  selected,
  onSelect,
  "data-testid": testId = "pareto-chart",
}: {
  trials: TrialRow[];
  selected?: number | null;
  onSelect?: (n: number | null) => void;
  "data-testid"?: string;
}) {
  const usable = React.useMemo(
    () =>
      trials.filter(
        (t) =>
          t.state === "COMPLETE" &&
          typeof t.sharpe === "number" &&
          typeof t.calmar === "number",
      ),
    [trials],
  );

  const { pts, xDom, yDom, frontPath } = React.useMemo(() => {
    let xlo = Infinity,
      xhi = -Infinity,
      ylo = Infinity,
      yhi = -Infinity;
    for (const t of usable) {
      const x = t.sharpe as number;
      const y = t.calmar as number;
      if (x < xlo) xlo = x;
      if (x > xhi) xhi = x;
      if (y < ylo) ylo = y;
      if (y > yhi) yhi = y;
    }
    const [xMin, xMax] = paddedRange(xlo, xhi);
    const [yMin, yMax] = paddedRange(ylo, yhi);
    const plotW = VB_W - PAD.left - PAD.right;
    const plotH = VB_H - PAD.top - PAD.bottom;
    const xOf = (v: number) =>
      PAD.left + ((v - xMin) / (xMax - xMin)) * plotW;
    const yOf = (v: number) =>
      PAD.top + (1 - (v - yMin) / (yMax - yMin)) * plotH;

    const points: Pt[] = usable.map((t) => ({
      number: t.number,
      x: t.sharpe as number,
      y: t.calmar as number,
      px: xOf(t.sharpe as number),
      py: yOf(t.calmar as number),
      pareto: t.pareto_front,
    }));

    // Front staircase: pareto points sorted by sharpe asc.
    const front = points
      .filter((p) => p.pareto)
      .sort((a, b) => a.x - b.x);
    const path =
      front.length > 1
        ? front
            .map((p, i) => `${i === 0 ? "M" : "L"}${p.px.toFixed(1)},${p.py.toFixed(1)}`)
            .join(" ")
        : null;

    return {
      pts: points,
      xDom: { min: xMin, max: xMax },
      yDom: { min: yMin, max: yMax },
      frontPath: path,
    };
  }, [usable]);

  const xTicks = React.useMemo(() => {
    const out: { v: number; px: number }[] = [];
    const plotW = VB_W - PAD.left - PAD.right;
    for (let i = 0; i <= 4; i++) {
      const v = xDom.min + (i / 4) * (xDom.max - xDom.min);
      out.push({ v, px: PAD.left + (i / 4) * plotW });
    }
    return out;
  }, [xDom]);

  const yTicks = React.useMemo(() => {
    const out: { v: number; py: number }[] = [];
    const plotH = VB_H - PAD.top - PAD.bottom;
    for (let i = 0; i <= 4; i++) {
      const v = yDom.min + (i / 4) * (yDom.max - yDom.min);
      out.push({ v, py: PAD.top + (1 - i / 4) * plotH });
    }
    return out;
  }, [yDom]);

  const [hover, setHover] = React.useState<number | null>(null);

  if (usable.length === 0) {
    return (
      <div
        className="flex h-40 items-center justify-center text-sm text-muted-foreground"
        data-testid={`${testId}-empty`}
      >
        No completed trials to plot yet.
      </div>
    );
  }

  const active = hover ?? selected ?? null;
  const activePt = pts.find((p) => p.number === active) ?? null;
  const frontCount = pts.filter((p) => p.pareto).length;

  return (
    <div
      className="w-full"
      data-testid={testId}
      data-point-count={pts.length}
      data-front-count={frontCount}
      data-pareto-count={frontCount}
    >
      <svg
        viewBox={`0 0 ${VB_W} ${VB_H}`}
        className="h-[320px] w-full"
        role="img"
        aria-label="Pareto front: Sharpe vs Calmar"
        onMouseLeave={() => setHover(null)}
      >
        {/* grid */}
        {yTicks.map((t, i) => (
          <g key={`y${i}`}>
            <line
              x1={PAD.left}
              x2={VB_W - PAD.right}
              y1={t.py}
              y2={t.py}
              className="stroke-border"
              strokeWidth={1}
              opacity={0.5}
            />
            <text
              x={PAD.left - 6}
              y={t.py + 3}
              textAnchor="end"
              className="fill-muted-foreground"
              fontSize={10}
            >
              {formatNum(t.v, 2)}
            </text>
          </g>
        ))}
        {xTicks.map((t, i) => (
          <text
            key={`x${i}`}
            x={t.px}
            y={VB_H - PAD.bottom + 14}
            textAnchor="middle"
            className="fill-muted-foreground"
            fontSize={10}
          >
            {formatNum(t.v, 2)}
          </text>
        ))}

        {/* axis labels */}
        <text
          x={(VB_W + PAD.left - PAD.right) / 2}
          y={VB_H - 6}
          textAnchor="middle"
          className="fill-muted-foreground"
          fontSize={11}
        >
          Sharpe →
        </text>
        <text
          x={14}
          y={(VB_H + PAD.top - PAD.bottom) / 2}
          textAnchor="middle"
          className="fill-muted-foreground"
          fontSize={11}
          transform={`rotate(-90 14 ${(VB_H + PAD.top - PAD.bottom) / 2})`}
        >
          Calmar →
        </text>

        {/* front staircase */}
        {frontPath ? (
          <path
            d={frontPath}
            fill="none"
            className="stroke-primary"
            strokeWidth={1.5}
            strokeDasharray="4 3"
            opacity={0.7}
            data-testid="pareto-front-path"
          />
        ) : null}

        {/* points: dominated first (muted), then pareto on top */}
        {pts
          .filter((p) => !p.pareto)
          .map((p) => (
            <circle
              key={p.number}
              cx={p.px}
              cy={p.py}
              r={active === p.number ? 5 : 3}
              className="fill-muted-foreground"
              opacity={active === p.number ? 0.9 : 0.45}
              data-testid={`pareto-point-${p.number}`}
              onMouseEnter={() => setHover(p.number)}
              onClick={() => onSelect?.(p.number)}
              style={{ cursor: onSelect ? "pointer" : "default" }}
            />
          ))}
        {pts
          .filter((p) => p.pareto)
          .map((p) => (
            <circle
              key={p.number}
              cx={p.px}
              cy={p.py}
              r={active === p.number ? 6 : 4.5}
              className="fill-primary"
              stroke="var(--background, #fff)"
              strokeWidth={1}
              data-testid={`pareto-point-${p.number}`}
              data-pareto="true"
              onMouseEnter={() => setHover(p.number)}
              onClick={() => onSelect?.(p.number)}
              style={{ cursor: onSelect ? "pointer" : "default" }}
            />
          ))}

        {/* hover readout */}
        {activePt ? (
          <g data-testid="pareto-hover">
            <line
              x1={activePt.px}
              x2={activePt.px}
              y1={PAD.top}
              y2={VB_H - PAD.bottom}
              className="stroke-foreground"
              strokeWidth={1}
              opacity={0.2}
            />
            <line
              x1={PAD.left}
              x2={VB_W - PAD.right}
              y1={activePt.py}
              y2={activePt.py}
              className="stroke-foreground"
              strokeWidth={1}
              opacity={0.2}
            />
          </g>
        ) : null}
      </svg>

      <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
        <span className="flex items-center gap-1.5">
          <span className="inline-block size-2.5 rounded-full bg-primary" />
          <span className="text-muted-foreground">Pareto front ({frontCount})</span>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="inline-block size-2.5 rounded-full bg-muted-foreground opacity-45" />
          <span className="text-muted-foreground">Dominated ({pts.length - frontCount})</span>
        </span>
        {activePt ? (
          <span
            className="ml-auto tabular-nums text-muted-foreground"
            data-testid="pareto-hover-readout"
          >
            trial #{activePt.number} · sharpe {formatNum(activePt.x)} · calmar{" "}
            {formatNum(activePt.y)}
          </span>
        ) : null}
      </div>
    </div>
  );
}
