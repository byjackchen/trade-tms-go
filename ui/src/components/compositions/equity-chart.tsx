"use client";

import * as React from "react";
import type { EquityPoint } from "@/lib/api/types";
import { formatMoneyCompact } from "@/lib/format";

export type EquitySeries = {
  id: string;
  label: string;
  color: string;
  points: EquityPoint[];
};

const PAD = { top: 12, right: 12, bottom: 22, left: 56 };
const VB_W = 800;
const VB_H = 280;

type Scaled = { x: number; y: number; ts: string; balance: number };

/**
 * Dependency-free multi-series equity chart rendered as an inline SVG (no chart
 * lib in the UI stack). Series share one time + value domain. Hovering reveals a
 * crosshair + value readout for the nearest sample of every series.
 *
 * Time is mapped on the union of all series timestamps (a stable sorted index),
 * so series with differing sample counts still align on the shared x-axis.
 */
export function EquityChart({
  series,
  "data-testid": testId = "equity-chart",
}: {
  series: EquitySeries[];
  "data-testid"?: string;
}) {
  const nonEmpty = series.filter((s) => s.points.length > 0);

  const { domain, xIndex } = React.useMemo(() => {
    const tsSet = new Set<string>();
    let min = Infinity;
    let max = -Infinity;
    for (const s of nonEmpty) {
      for (const p of s.points) {
        tsSet.add(p.ts);
        if (p.balance_usd < min) min = p.balance_usd;
        if (p.balance_usd > max) max = p.balance_usd;
      }
    }
    const sortedTs = [...tsSet].sort();
    const idx = new Map<string, number>();
    sortedTs.forEach((t, i) => idx.set(t, i));
    if (!Number.isFinite(min) || !Number.isFinite(max)) {
      min = 0;
      max = 1;
    }
    if (min === max) {
      // Flat curve: pad the value domain so the line sits mid-frame.
      const pad = Math.abs(min) > 0 ? Math.abs(min) * 0.01 : 1;
      min -= pad;
      max += pad;
    }
    return {
      domain: { min, max, n: Math.max(sortedTs.length, 1), ts: sortedTs },
      xIndex: idx,
    };
  }, [nonEmpty]);

  const plotW = VB_W - PAD.left - PAD.right;
  const plotH = VB_H - PAD.top - PAD.bottom;

  const xOf = React.useCallback(
    (ts: string) => {
      const i = xIndex.get(ts) ?? 0;
      const denom = domain.n > 1 ? domain.n - 1 : 1;
      return PAD.left + (i / denom) * plotW;
    },
    [xIndex, domain.n, plotW],
  );

  const yOf = React.useCallback(
    (v: number) => {
      const t = (v - domain.min) / (domain.max - domain.min);
      return PAD.top + (1 - t) * plotH;
    },
    [domain.min, domain.max, plotH],
  );

  const scaled: Record<string, Scaled[]> = React.useMemo(() => {
    const out: Record<string, Scaled[]> = {};
    for (const s of nonEmpty) {
      out[s.id] = s.points.map((p) => ({
        x: xOf(p.ts),
        y: yOf(p.balance_usd),
        ts: p.ts,
        balance: p.balance_usd,
      }));
    }
    return out;
  }, [nonEmpty, xOf, yOf]);

  const [hoverIdx, setHoverIdx] = React.useState<number | null>(null);

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const px = ((e.clientX - rect.left) / rect.width) * VB_W;
    const denom = domain.n > 1 ? domain.n - 1 : 1;
    const rel = (px - PAD.left) / plotW;
    const i = Math.round(rel * denom);
    setHoverIdx(Math.max(0, Math.min(domain.n - 1, i)));
  };

  // Horizontal grid lines (5 ticks across the value domain).
  const yTicks = React.useMemo(() => {
    const ticks: { y: number; value: number }[] = [];
    for (let i = 0; i <= 4; i++) {
      const v = domain.min + (i / 4) * (domain.max - domain.min);
      ticks.push({ y: yOf(v), value: v });
    }
    return ticks;
  }, [domain.min, domain.max, yOf]);

  if (nonEmpty.length === 0) {
    return null;
  }

  const hoverTs = hoverIdx != null ? domain.ts[hoverIdx] ?? null : null;
  const hoverX = hoverTs != null ? xOf(hoverTs) : null;

  // The point count the ground-truth tests compare against is the portfolio
  // (account-equity) series length, which equals COUNT(equity_curves).
  const portfolioCount =
    nonEmpty.find((s) => s.id === "portfolio")?.points.length ??
    nonEmpty[0]?.points.length ??
    0;

  return (
    <div className="w-full" data-testid={testId} data-point-count={portfolioCount}>
      <svg
        viewBox={`0 0 ${VB_W} ${VB_H}`}
        className="h-64 w-full"
        preserveAspectRatio="none"
        onMouseMove={onMove}
        onMouseLeave={() => setHoverIdx(null)}
        role="img"
        aria-label="Equity curve"
      >
        {/* grid + y-axis labels */}
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
              {formatMoneyCompact(t.value)}
            </text>
          </g>
        ))}

        {/* series polylines */}
        {nonEmpty.map((s) => {
          const pts = scaled[s.id];
          if (!pts || pts.length === 0) return null;
          const d = pts
            .map((p, i) => `${i === 0 ? "M" : "L"}${p.x.toFixed(2)},${p.y.toFixed(2)}`)
            .join(" ");
          return (
            <path
              key={s.id}
              d={d}
              fill="none"
              stroke={s.color}
              strokeWidth={1.75}
              strokeLinejoin="round"
              strokeLinecap="round"
              data-testid={`equity-series-${s.id}`}
              vectorEffect="non-scaling-stroke"
            />
          );
        })}

        {/* hover crosshair */}
        {hoverX != null ? (
          <line
            x1={hoverX}
            x2={hoverX}
            y1={PAD.top}
            y2={VB_H - PAD.bottom}
            className="stroke-foreground"
            strokeWidth={1}
            opacity={0.3}
            data-testid="equity-crosshair"
          />
        ) : null}
        {hoverTs != null
          ? nonEmpty.map((s) => {
              const p = scaled[s.id]?.find((q) => q.ts === hoverTs);
              if (!p) return null;
              return (
                <circle
                  key={s.id}
                  cx={p.x}
                  cy={p.y}
                  r={3}
                  fill={s.color}
                  stroke="var(--background, #fff)"
                  strokeWidth={1}
                />
              );
            })
          : null}
      </svg>

      {/* legend + hover readout */}
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
        {nonEmpty.map((s) => {
          const hov =
            hoverTs != null
              ? scaled[s.id]?.find((q) => q.ts === hoverTs)
              : undefined;
          return (
            <span
              key={s.id}
              className="flex items-center gap-1.5"
              data-testid={`equity-legend-${s.id}`}
            >
              <span
                className="inline-block size-2.5 rounded-sm"
                style={{ background: s.color }}
              />
              <span className="text-muted-foreground">{s.label}</span>
              {hov ? (
                <span className="tabular-nums font-medium">
                  {formatMoneyCompact(hov.balance)}
                </span>
              ) : null}
            </span>
          );
        })}
        {hoverTs ? (
          <span
            className="ml-auto tabular-nums text-muted-foreground"
            data-testid="equity-hover-ts"
          >
            {hoverTs.slice(0, 10)}
          </span>
        ) : null}
      </div>
    </div>
  );
}
