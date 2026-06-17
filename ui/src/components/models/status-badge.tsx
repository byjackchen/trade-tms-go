import { Loader2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import type { BacktestStatus } from "@/lib/api/types";

const VARIANT: Record<
  BacktestStatus,
  "muted" | "secondary" | "success" | "destructive" | "warning"
> = {
  RUNNING: "secondary",
  COMPLETE: "success",
  INTERRUPTED: "warning",
  FAIL: "destructive",
};

/** Status badge for a backtest run; spins a loader while RUNNING. */
export function BacktestStatusBadge({
  status,
  "data-testid": testId = "backtest-status-badge",
}: {
  status: BacktestStatus;
  "data-testid"?: string;
}) {
  return (
    <Badge variant={VARIANT[status]} data-testid={testId} data-status={status}>
      {status === "RUNNING" ? (
        <Loader2 className="animate-spin" />
      ) : null}
      {status}
    </Badge>
  );
}
