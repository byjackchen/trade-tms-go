"use client";

import { useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  CloudDownload,
  RefreshCw,
} from "lucide-react";
import { useBrokerSync } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import type { BrokerSyncResponse } from "@/lib/api/types";
import { formatInt, formatRelative } from "@/lib/format";

/**
 * SYNC FROM BROKER — DIRECTION 2 (broker → TMS), the operator's PRIMARY case.
 *
 * The operator trades DIRECTLY in the moomoo app (no order placed via TMS), then
 * returns here and clicks "Sync from broker" to pull the account's ACTUAL state
 * (Trd_GetPositionList + Trd_GetOrderList + Trd_GetOrderFillList + Trd_GetFunds)
 * and REFLECT it into TMS so the positions/orders/fills/account panels show what
 * was actually done. The synced broker truth is reflected under the MANUAL/EXTERNAL
 * book (so strategy books are not corrupted) and reconciled vs the strategy books.
 *
 * SAFETY: this is READ-ONLY at the broker (POST /api/v1/trade/sync; `read_only`),
 * it places NO orders, so it needs NO confirm token and NO risk gate — and is
 * therefore safe in ALL modes, INCLUDING signal (a signal-mode operator can sync to
 * see / manage what they actually hold). The hook invalidates the trading reads, so
 * the panels converge to broker truth right after the sync.
 *
 * Surfaces a result toast (positions/orders/fills synced, reflected count, and any
 * reconciliation drift) plus a "last synced" timestamp.
 */
export function SyncFromBroker() {
  const sync = useBrokerSync();
  const [result, setResult] = useState<BrokerSyncResponse | null>(null);
  const [lastSyncedAt, setLastSyncedAt] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());

  function doSync() {
    sync.mutate(undefined, {
      onSuccess: (res) => {
        setResult(res);
        const ts = new Date().toISOString();
        setLastSyncedAt(ts);
        setNow(Date.now());
      },
    });
  }

  const noDesk = sync.error instanceof ApiError && sync.error.status === 503;
  const errorMsg =
    sync.error instanceof ApiError
      ? noDesk
        ? "No broker connection — start a paper/live trade node (the sync surface binds a broker account) to enable broker sync."
        : `${sync.error.code}: ${sync.error.message}`
      : sync.error
        ? "Sync failed; see the UI server logs."
        : null;

  const recon = result?.reconciliation;
  const hasDrift = recon?.has_issues ?? false;

  return (
    <Card
      data-testid="manual-sync"
      data-panel="manual-sync"
      data-last-synced={lastSyncedAt ?? ""}
    >
      <CardContent className="space-y-3 pt-6">
        <div className="flex flex-wrap items-center gap-3">
          <div className="flex items-center gap-2">
            <CloudDownload className="size-5 text-sky-600 dark:text-sky-400" />
            <div className="flex flex-col">
              <span className="text-sm font-semibold">Sync from broker</span>
              <span className="text-xs text-muted-foreground">
                Pull what you traded directly in moomoo into TMS. Read-only — places
                no orders. Safe in every mode.
              </span>
            </div>
          </div>

          <div className="ml-auto flex items-center gap-3">
            <span
              className="text-xs text-muted-foreground"
              data-testid="manual-sync-last"
              title={lastSyncedAt ?? undefined}
            >
              {lastSyncedAt
                ? `Last synced ${formatRelative(lastSyncedAt, now)}`
                : "Never synced"}
            </span>
            <Button
              onClick={doSync}
              disabled={sync.isPending}
              aria-disabled={sync.isPending}
              data-testid="manual-sync-button"
            >
              <RefreshCw className={sync.isPending ? "animate-spin" : undefined} />
              {sync.isPending ? "Syncing…" : "Sync from broker"}
            </Button>
          </div>
        </div>

        {/* Error toast (e.g. 503 no desk connected). */}
        {errorMsg ? (
          <Alert variant="destructive" data-testid="manual-sync-error">
            <AlertTriangle className="size-4" />
            <AlertDescription>{errorMsg}</AlertDescription>
          </Alert>
        ) : null}

        {/* Result toast — counts + reconciliation drift. */}
        {result ? (
          <Alert
            variant={hasDrift ? "warning" : "default"}
            data-testid="manual-sync-result"
            data-read-only={result.read_only ? "true" : "false"}
            data-has-drift={hasDrift ? "true" : "false"}
            data-reflected={result.reflected}
          >
            {hasDrift ? (
              <AlertTriangle className="size-4" />
            ) : (
              <CheckCircle2 className="size-4" />
            )}
            <AlertTitle className="flex items-center gap-2">
              Synced from broker
              {result.read_only ? (
                <Badge variant="secondary" data-testid="manual-sync-read-only">
                  read-only
                </Badge>
              ) : null}
            </AlertTitle>
            <AlertDescription>
              <div
                className="flex flex-wrap gap-x-4 gap-y-1 text-xs"
                data-testid="manual-sync-counts"
                data-positions={result.positions_observed}
                data-orders={result.orders_observed}
                data-fills={result.fills_observed}
              >
                <span>
                  Positions:{" "}
                  <span className="font-mono font-medium">
                    {formatInt(result.positions_observed)}
                  </span>
                </span>
                <span>
                  Orders:{" "}
                  <span className="font-mono font-medium">
                    {formatInt(result.orders_observed)}
                  </span>
                </span>
                <span>
                  Fills:{" "}
                  <span className="font-mono font-medium">
                    {formatInt(result.fills_observed)}
                  </span>
                </span>
                <span>
                  Reflected:{" "}
                  <span className="font-mono font-medium">
                    {formatInt(result.reflected)}
                  </span>
                </span>
              </div>
              {hasDrift ? (
                <p
                  className="mt-2 text-xs"
                  data-testid="manual-sync-drift"
                  data-mismatch-count={recon?.mismatches.length ?? 0}
                >
                  Reconciliation drift vs strategy books:{" "}
                  {recon?.summary ??
                    `${recon?.mismatches.length ?? 0} mismatch(es)`}
                  . The node halts on a mismatch — review the reconciliation panel
                  below. Drift is never auto-corrected by trading.
                </p>
              ) : (
                <p
                  className="mt-2 text-xs text-muted-foreground"
                  data-testid="manual-sync-no-drift"
                >
                  Reconciled clean against the strategy books
                  {typeof recon?.matched === "number"
                    ? ` (${formatInt(recon.matched)} matched)`
                    : ""}
                  . The panels below now reflect broker truth.
                </p>
              )}
            </AlertDescription>
          </Alert>
        ) : null}
      </CardContent>
    </Card>
  );
}
