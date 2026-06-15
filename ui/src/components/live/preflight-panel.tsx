"use client";

import { useState } from "react";
import { useLivePreflight } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "./live-badges";
import type { PreflightResult } from "@/lib/api/types";

type Dot = "green" | "yellow" | "red" | "gray";

/** Map a preflight check status to a status dot color. */
function statusToDot(status: string): Dot {
  switch (status) {
    case "pass":
      return "green";
    case "warn":
      return "yellow";
    case "fail":
      return "red";
    default:
      // skip
      return "gray";
  }
}

function CheckRow({ c }: { c: PreflightResult }) {
  const dot = statusToDot(c.status);
  return (
    <div
      className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2"
      data-testid={`preflight-check-${c.check}`}
      data-status={c.status}
      data-severity={c.severity}
    >
      <div className="flex min-w-0 items-center gap-2">
        <StatusDot color={dot} pulse={false} />
        <span className="truncate text-sm font-medium" title={c.check}>
          {c.check}
        </span>
        {c.severity === "blocker" ? (
          <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] uppercase text-muted-foreground">
            blocker
          </span>
        ) : null}
      </div>
      <span
        className="truncate text-xs text-muted-foreground"
        title={c.detail}
      >
        {c.detail}
      </span>
    </div>
  );
}

const MODES = ["signal", "paper", "live"] as const;

/**
 * Go-live preflight panel: runs the precondition checks for the selected mode
 * and renders each pass/warn/fail line, with an overall PASS/FAIL verdict. The
 * operator picks the target mode (signal treats data freshness + OpenD as
 * advisory; paper/live require all blockers). A 503 means the API has no
 * preflight runner wired (older / signal-only deployment).
 */
export function PreflightPanel() {
  const [mode, setMode] = useState<(typeof MODES)[number]>("signal");
  const q = useLivePreflight({ mode, strategy: "multi", check_opend: mode !== "signal" });

  const notConfigured = q.error instanceof ApiError && q.error.status === 503;
  const report = q.data;

  return (
    <Card data-testid="preflight-panel">
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <CardTitle className="text-sm">Go-live preflight</CardTitle>
          <div className="flex gap-1" data-testid="preflight-mode-switch">
            {MODES.map((m) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                data-testid={`preflight-mode-${m}`}
                data-active={m === mode}
                className={`rounded px-2 py-0.5 text-xs ${
                  m === mode
                    ? "bg-primary text-primary-foreground"
                    : "bg-muted text-muted-foreground"
                }`}
              >
                {m}
              </button>
            ))}
          </div>
        </div>
        {report ? (
          <span
            className="text-xs font-medium"
            data-testid="preflight-verdict"
            data-ok={report.ok}
          >
            {report.ok ? "PASS — preconditions met" : "FAIL — blockers present"}
          </span>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-2">
        {q.isLoading ? (
          <Skeleton className="h-40 w-full" />
        ) : notConfigured ? (
          <p
            className="text-xs text-muted-foreground"
            data-testid="preflight-unavailable"
          >
            Preflight not configured on this API deployment.
          </p>
        ) : q.error ? (
          <p className="text-xs text-red-500" data-testid="preflight-error">
            {q.error.message}
          </p>
        ) : report ? (
          <div className="space-y-2" data-testid="preflight-checks">
            {report.checks.map((c) => (
              <CheckRow key={c.check} c={c} />
            ))}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
