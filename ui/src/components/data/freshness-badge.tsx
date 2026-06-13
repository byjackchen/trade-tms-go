import { Badge } from "@/components/ui/badge";
import type { Freshness } from "@/lib/api/types";

/**
 * Freshness badge from a table's lag in NYSE sessions vs the latest session.
 *   0 sessions  -> fresh (green)
 *   1 session   -> 1 behind (amber) — typical EOD-pending state
 *   >=2 sessions -> stale (red)
 * Missing freshness (empty table) -> neutral "no data".
 */
export function FreshnessBadge({
  freshness,
  "data-testid": testId = "freshness-badge",
}: {
  freshness?: Freshness;
  "data-testid"?: string;
}) {
  if (!freshness) {
    return (
      <Badge variant="muted" data-testid={testId} data-freshness="none">
        no data
      </Badge>
    );
  }
  const lag = freshness.lag_sessions;
  if (lag <= 0) {
    return (
      <Badge variant="success" data-testid={testId} data-freshness="fresh">
        fresh
      </Badge>
    );
  }
  if (lag === 1) {
    return (
      <Badge variant="warning" data-testid={testId} data-freshness="lagging">
        1 session behind
      </Badge>
    );
  }
  return (
    <Badge variant="destructive" data-testid={testId} data-freshness="stale">
      {lag} sessions behind
    </Badge>
  );
}
