import { Badge } from "@/components/ui/badge";
import type { StudyStatus, TrialState } from "@/lib/api/types";

const STUDY_VARIANT: Record<
  StudyStatus,
  "default" | "success" | "warning" | "destructive" | "muted"
> = {
  RUNNING: "default",
  COMPLETE: "success",
  INTERRUPTED: "warning",
  FAIL: "destructive",
};

/** Lifecycle badge for a study (RUNNING/COMPLETE/INTERRUPTED/FAIL). */
export function StudyStatusBadge({
  status,
  "data-testid": testId,
}: {
  status: StudyStatus;
  "data-testid"?: string;
}) {
  return (
    <Badge
      variant={STUDY_VARIANT[status] ?? "muted"}
      data-testid={testId}
      data-status={status}
      className={status === "RUNNING" ? "animate-pulse" : undefined}
    >
      {status}
    </Badge>
  );
}

const TRIAL_VARIANT: Record<
  string,
  "default" | "success" | "warning" | "destructive" | "muted"
> = {
  COMPLETE: "success",
  RUNNING: "default",
  FAIL: "destructive",
  PRUNED: "warning",
};

/** Per-trial completion-state badge. */
export function TrialStateBadge({
  state,
  "data-testid": testId,
}: {
  state: TrialState;
  "data-testid"?: string;
}) {
  return (
    <Badge
      variant={TRIAL_VARIANT[state] ?? "muted"}
      data-testid={testId}
      data-state={state}
    >
      {state}
    </Badge>
  );
}
