import * as React from "react";
import { cn } from "@/lib/utils";

/**
 * Determinate / indeterminate progress bar. `value` in [0,100]; pass
 * `indeterminate` (or omit value) while a stage has no percentage yet.
 */
function Progress({
  value,
  indeterminate,
  className,
  barClassName,
  ...props
}: React.ComponentProps<"div"> & {
  value?: number | null;
  indeterminate?: boolean;
  barClassName?: string;
}) {
  const pct =
    value == null ? 0 : Math.max(0, Math.min(100, value));
  const isIndeterminate = indeterminate || value == null;
  return (
    <div
      data-slot="progress"
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={isIndeterminate ? undefined : pct}
      className={cn(
        "relative h-2 w-full overflow-hidden rounded-full bg-muted",
        className,
      )}
      {...props}
    >
      <div
        data-slot="progress-bar"
        className={cn(
          "h-full rounded-full bg-primary transition-[width] duration-300 ease-out",
          isIndeterminate && "w-1/3 animate-[progress-indeterminate_1.2s_ease-in-out_infinite]",
          barClassName,
        )}
        style={isIndeterminate ? undefined : { width: `${pct}%` }}
      />
      <style>{`@keyframes progress-indeterminate {0%{transform:translateX(-100%)}100%{transform:translateX(300%)}}`}</style>
    </div>
  );
}

export { Progress };
