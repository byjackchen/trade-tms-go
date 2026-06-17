"use client";

import { AlertOctagon } from "lucide-react";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import { ApiError } from "@/lib/api/client";
import { Card } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  ModeBadge,
  SessionStatusBadge,
  StatusDot,
  sessionModeLabel,
} from "./live-badges";
import { formatRelative, formatTs } from "@/lib/format";

function Field({
  label,
  children,
  testid,
}: {
  label: string;
  children: React.ReactNode;
  testid?: string;
}) {
  return (
    <div className="flex flex-col gap-1" data-testid={testid}>
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <div className="flex items-center gap-1.5 text-sm">{children}</div>
    </div>
  );
}

/**
 * The Portfolio view's at-a-glance session strip: mode, status, trader, start time and
 * the active halt (prominent destructive callout when halted). PG-backed and
 * short-polled so it converges after a command without a manual reload.
 */
export function SessionBar() {
  const q = useLiveSession();

  if (q.isLoading) {
    return (
      <Card className="px-4" data-testid="live-session" data-panel="session-bar">
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  }

  // 503 = API started without a live reader (expected degraded state).
  const noReader = q.error instanceof ApiError && q.error.status === 503;
  if (noReader) {
    return (
      <Card
        className="flex-row items-center gap-2 px-4 py-3"
        data-testid="live-session"
        data-panel="session-bar"
        data-state="no-reader"
      >
        <StatusDot color="gray" />
        <span className="text-sm text-muted-foreground">
          Live reader not configured on the API — the Portfolio view is read-only and
          idle. Start a live session to populate it.
        </span>
      </Card>
    );
  }

  if (q.error) {
    return (
      <Card
        className="flex-row items-center gap-2 px-4 py-3"
        data-testid="live-session"
        data-panel="session-bar"
        data-state="error"
      >
        <StatusDot color="red" />
        <span className="text-sm text-destructive">
          Failed to load session: {q.error.message}
        </span>
      </Card>
    );
  }

  const data = q.data;
  if (!hasSession(data)) {
    return (
      <Card
        className="flex-row items-center gap-2 px-4 py-3"
        data-testid="live-session"
        data-panel="session-bar"
        data-state="no-session"
      >
        <StatusDot color="gray" />
        <span className="text-sm text-muted-foreground">
          No live session has run yet. Start one from the System tab.
        </span>
      </Card>
    );
  }

  const halted = data.halt != null;
  return (
    <Card
      className={
        halted
          ? "gap-3 py-3 ring-2 ring-destructive/60"
          : "gap-3 py-3"
      }
      data-testid="live-session"
      data-panel="session-bar"
      data-state={data.status}
      data-status={data.status}
      data-mode={sessionModeLabel(data.exec_policy, data.account_env)}
      data-halted={halted ? "true" : "false"}
    >
      <div className="flex flex-wrap items-center gap-x-8 gap-y-3 px-4">
        <Field label="Mode" testid="session-mode">
          {/* `mode` is gone server-side: derive the paper/live/signal label from
              the authoritative exec_policy + account_env (§1.3, C6). */}
          <ModeBadge mode={sessionModeLabel(data.exec_policy, data.account_env)} />
        </Field>
        <Field label="Status" testid="session-status">
          <SessionStatusBadge status={data.status} />
        </Field>
        <Field label="Trader" testid="session-trader">
          <span className="font-mono">{data.trader_id}</span>
        </Field>
        <Field label="Started" testid="session-started">
          <span title={formatTs(data.started_at)}>
            {formatRelative(data.started_at)}
          </span>
        </Field>
        {data.ended_at ? (
          <Field label="Ended" testid="session-ended">
            <span title={formatTs(data.ended_at)}>
              {formatRelative(data.ended_at)}
            </span>
          </Field>
        ) : null}
        <Field label="Halt" testid="session-halt">
          {halted ? (
            <Badge variant="destructive" data-testid="halt-badge">
              HALTED
            </Badge>
          ) : (
            <Badge variant="success" data-testid="halt-badge">
              clear
            </Badge>
          )}
        </Field>
      </div>

      {halted && data.halt ? (
        <div
          className="mx-4 flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
          data-testid="live-halted-banner"
          data-detail="halt-detail"
        >
          <AlertOctagon className="mt-0.5 size-4 shrink-0" />
          <div>
            <span className="font-medium capitalize">{data.halt.kind}</span>{" "}
            halt — {data.halt.reason || "no reason given"}{" "}
            <span className="text-destructive/70">
              ({formatRelative(data.halt.triggered_at)})
            </span>
          </div>
        </div>
      ) : null}
    </Card>
  );
}
