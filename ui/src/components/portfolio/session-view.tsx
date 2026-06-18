"use client";

import { Suspense } from "react";
import Link from "next/link";
import { Wallet } from "lucide-react";
import { cn } from "@/lib/utils";
import { LiveIndicator } from "./live-indicator";
import { ExecBanner } from "./exec-banner";
import { SessionPanel } from "./session-panel";
import { HealthStrip } from "./health-strip";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import type { TradeEnv } from "./trade-env";

/**
 * `<SessionView />` — the SESSION top-level: RUNTIME control. It surfaces the
 * session lifecycle (start/stop, exec policy), the running Composition, the
 * session's bound Account (READ-ONLY — account selection lives in the Account
 * top-level), and the live BAR tape (inside <SessionPanel>).
 *
 * The session's env (paper vs LIVE-real) is derived from its bound account's
 * `account_env`; a SMALLER live indicator lives here (the loud LIVE-red treatment
 * belongs to the Account view, which owns account selection). When no session is
 * running the env defaults to paper and the panel falls back to a start prompt.
 */
export function SessionView() {
  return (
    <Suspense fallback={<SessionModule env="paper" accountId={null} />}>
      <SessionViewInner />
    </Suspense>
  );
}

function SessionViewInner() {
  const q = useLiveSession();
  const session = hasSession(q.data) ? q.data : null;
  const env: TradeEnv =
    session && session.account_id
      ? session.account_env === "real"
        ? "live"
        : "paper"
      : "paper";
  const accountId = session?.account_id || null;

  return <SessionModule env={env} accountId={accountId} />;
}

function SessionModule({
  env,
  accountId,
}: {
  env: TradeEnv;
  accountId: string | null;
}) {
  return (
    <div data-env={env} data-testid="session-module">
      <main
        className={cn(
          "mx-auto w-full max-w-7xl flex-1 space-y-4 p-6 ui-mobile:p-4",
        )}
        data-testid="session-view"
        data-env={env}
      >
        {/* A small right-aligned inline row: the read-only bound-account link to
            the Account top-level (account selection does NOT happen here) +
            the live indicator. */}
        <div className="flex items-center justify-end gap-3">
          <Link
            href={
              accountId
                ? `/account?account=${encodeURIComponent(accountId)}`
                : "/account"
            }
            data-testid="session-account-link"
            data-env={env}
            className={cn(
              "flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium transition-colors",
              env === "live"
                ? "border-destructive/60 bg-destructive/10 text-destructive hover:bg-destructive/15"
                : "border-border bg-card text-muted-foreground hover:text-foreground",
            )}
          >
            <Wallet className="size-3.5" />
            <span className="font-mono">{accountId ?? "no account"}</span>
          </Link>
          <LiveIndicator />
        </div>

        {/* A smaller exec + env banner: the loud LIVE-red treatment lives in the
            Account view (which owns account selection). */}
        <ExecBanner env={env} />

        {/* The apex runtime control: session status + its Composition + Account +
            lifecycle/exec controls + the live tape. */}
        <SessionPanel env={env} />

        {/* Portfolio health (daily P&L, daily-loss-halt headroom, concentration) —
            session-runtime risk, so it lives here, not on the Account book. */}
        <HealthStrip />
      </main>
    </div>
  );
}
