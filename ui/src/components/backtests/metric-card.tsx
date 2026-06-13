import * as React from "react";
import { cn } from "@/lib/utils";

/**
 * A single labeled metric tile for the detail header grid. `tone` colors the
 * value (positive/negative) for return-like metrics; default is neutral.
 */
export function MetricCard({
  label,
  value,
  sub,
  tone = "neutral",
  rawValue,
  "data-testid": testId,
}: {
  label: string;
  value: React.ReactNode;
  sub?: React.ReactNode;
  tone?: "neutral" | "positive" | "negative";
  /** The exact numeric value, exposed as `data-value` for ground-truth tests. */
  rawValue?: number;
  "data-testid"?: string;
}) {
  return (
    <div
      data-testid={testId}
      data-value={rawValue != null && Number.isFinite(rawValue) ? rawValue : undefined}
      className="flex flex-col gap-1 rounded-xl bg-card px-4 py-3 ring-1 ring-foreground/10"
    >
      <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span
        data-testid={testId ? `${testId}-value` : undefined}
        className={cn(
          "text-lg font-semibold tabular-nums leading-tight",
          tone === "positive" && "text-emerald-600 dark:text-emerald-400",
          tone === "negative" && "text-destructive",
        )}
      >
        {value}
      </span>
      {sub ? (
        <span className="text-xs text-muted-foreground">{sub}</span>
      ) : null}
    </div>
  );
}
