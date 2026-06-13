import { Badge } from "@/components/ui/badge";
import type { JobStatus } from "@/lib/api/types";

const JOB_VARIANT: Record<
  JobStatus,
  "muted" | "secondary" | "success" | "destructive" | "warning"
> = {
  queued: "muted",
  running: "secondary",
  succeeded: "success",
  failed: "destructive",
  canceled: "warning",
};

export function JobStatusBadge({
  status,
  "data-testid": testId = "job-status-badge",
}: {
  status: JobStatus;
  "data-testid"?: string;
}) {
  return (
    <Badge variant={JOB_VARIANT[status]} data-testid={testId} data-status={status}>
      {status}
    </Badge>
  );
}

/** Sync-run status: "ok" | "error" | other engine-defined strings. */
export function SyncStatusBadge({
  status,
  "data-testid": testId = "sync-status-badge",
}: {
  status: string;
  "data-testid"?: string;
}) {
  const v =
    status === "ok"
      ? "success"
      : status === "error" || status === "failed"
        ? "destructive"
        : "secondary";
  return (
    <Badge variant={v} data-testid={testId} data-status={status}>
      {status}
    </Badge>
  );
}
