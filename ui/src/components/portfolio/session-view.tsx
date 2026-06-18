"use client";

import { Suspense } from "react";
import Link from "next/link";
import { Wallet } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { cn } from "@/lib/utils";
import { LiveIndicator } from "./live-indicator";
import { ExecBanner } from "./exec-banner";
import { SessionPanel } from "./session-panel";
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
    <Suspense fallback={<SessionHeader env="paper" accountId={null} />}>
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

  return (
    <div data-env={env} data-testid="session-module">
      <SessionHeader env={env} accountId={accountId} />
      <main
        className={cn(
          "mx-auto w-full max-w-7xl flex-1 space-y-4 p-6 ui-mobile:p-4",
        )}
        data-testid="session-view"
        data-env={env}
      >
        {/* A smaller exec + env banner: the loud LIVE-red treatment lives in the
            Account view (which owns account selection). */}
        <ExecBanner env={env} />

        {/* The apex runtime control: session status + its Composition + Account +
            lifecycle/exec controls + the live tape. */}
        <SessionPanel env={env} />
      </main>
    </div>
  );
}

const ENV_COPY: Record<TradeEnv, { title: string; subtitle: string }> = {
  paper: {
    title: "Session",
    subtitle:
      "Runtime control — the session lifecycle, the Composition it runs, and the live tape. The session's bound account is shown read-only; manage accounts in the Account top-level.",
  },
  live: {
    title: "Session — LIVE",
    subtitle:
      "Runtime control for a LIVE (real-money) session. Lifecycle + exec policy here; the account itself lives in the Account top-level.",
  },
};

function SessionHeader({
  env,
  accountId,
}: {
  env: TradeEnv;
  accountId: string | null;
}) {
  const copy = ENV_COPY[env];
  return (
    <PageHeader
      title={copy.title}
      subtitle={copy.subtitle}
      data-testid="session-header"
      actions={
        <div className="flex items-center gap-3">
          {/* Read-only bound-account link to the Account top-level (account
              selection does NOT happen on the Session page). */}
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
      }
    />
  );
}
