"use client";

import { Layers, Wallet } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import { SessionBar } from "./session-bar";
import { SessionControls } from "./session-controls";
import type { TradeEnv } from "./trade-env";

/**
 * <SessionPanel> — the FOREGROUNDED session control: the apex of the trade
 * cockpit's Session → Composition → Account → Positions hierarchy. The session
 * is the runtime entity that ties a Composition (its strategies + weights + risk)
 * to the Account it executes on; this panel surfaces that binding plus the
 * lifecycle / execution-policy controls, ABOVE the account's book.
 *
 * When NO session is running (the dev default), it falls back to a bound-account
 * browser so an operator can still inspect any account's book below — the account
 * book is account-scoped and queryable without a session.
 */
export function SessionPanel({ env }: { env: TradeEnv }) {
  const q = useLiveSession();
  const session = hasSession(q.data) ? q.data : null;

  return (
    <Card data-testid="session-panel">
      <CardHeader>
        <CardTitle className="text-sm">Session</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <SessionBar />

        {session ? (
          // A live session runs exactly one Composition and (in paper/live)
          // executes on one Account — show both, since they are the session's,
          // not separately picked.
          <div
            className="grid grid-cols-1 gap-2 ui-desktop:sm:grid-cols-2"
            data-testid="session-bindings"
          >
            <BindingRow
              icon={Layers}
              label="Composition"
              value={session.composition_name || session.composition_id || "—"}
              testid="session-composition"
            />
            <BindingRow
              icon={Wallet}
              label="Account"
              value={session.account_id || "signal — no account bound"}
              env={
                session.account_id
                  ? session.account_env === "real"
                    ? "live"
                    : "paper"
                  : undefined
              }
              testid="session-account"
            />
          </div>
        ) : (
          // No running session: the book below shows whichever account is picked
          // in the header (account-scoped, queryable without a session).
          <div
            className="rounded-lg border border-dashed border-border px-3 py-2 text-xs text-muted-foreground"
            data-testid="session-none"
          >
            No active session — start one below, or inspect the account picked in
            the header.
          </div>
        )}

        <SessionControls env={env} />
      </CardContent>
    </Card>
  );
}

/** One Session → {Composition|Account} binding row: icon + label + value (+ env badge). */
function BindingRow({
  icon: Icon,
  label,
  value,
  env,
  testid,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: string;
  env?: TradeEnv;
  testid: string;
}) {
  return (
    <div
      className="flex items-center gap-2 rounded-lg border border-border px-3 py-2"
      data-testid={testid}
    >
      <Icon className="size-4 shrink-0 text-muted-foreground" />
      <span className="shrink-0 text-xs text-muted-foreground">{label}</span>
      <span className="min-w-0 flex-1 truncate text-right font-mono text-sm">
        {value}
      </span>
      {env ? (
        <span
          data-testid={`${testid}-env`}
          data-kind={env}
          className={
            env === "live"
              ? "shrink-0 rounded-full border border-destructive/60 bg-destructive/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-destructive"
              : "shrink-0 rounded-full border border-amber-500/50 bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-amber-700 dark:text-amber-300"
          }
        >
          {env}
        </span>
      ) : null}
    </div>
  );
}
