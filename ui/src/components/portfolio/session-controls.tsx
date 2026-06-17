"use client";

import { useState } from "react";
import {
  Play,
  Square,
  RotateCcw,
  Power,
  OctagonX,
  ShieldAlert,
  Check,
  Flame,
  Scale,
  Siren,
} from "lucide-react";
import { useLiveSession, useLiveCommand } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { ConfirmActionDialog } from "./confirm-action-dialog";
import { ModeBadge, sessionModeLabel } from "./live-badges";
import type { CommandName, LiveMode, ExecPolicy } from "@/lib/api/types";

type DialogKind =
  | { kind: "halt" }
  | { kind: "kill" }
  | { kind: "stop" }
  | { kind: "arm" }
  | { kind: "flatten" }
  | { kind: "emergency_kill" }
  | null;

/**
 * Live session controls — the new write capability.
 *
 * Every action posts to POST /api/v1/live/commands via the server proxy (the
 * bearer token never reaches the browser); tms-live applies it idempotently
 * under audit. Safe actions (start/resume, switch to signal) fire directly;
 * dangerous ones (stop, halt, kill, switch to paper/live) route through a typed
 * confirmation dialog. paper/live additionally require a confirm_token, which is
 * the typed phrase (consumed at the API boundary, never persisted).
 */
export function SessionControls({ env }: { env: "paper" | "live" }) {
  const sessionQ = useLiveSession();
  const cmd = useLiveCommand();
  const [dialog, setDialog] = useState<DialogKind>(null);
  // The confirmation inputs live HERE (not inside the dialog) so the session
  // poll re-rendering this component can never clear what the operator typed.
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  // A transient "command accepted" toast keyed by the command we just sent.
  const [accepted, setAccepted] = useState<string | null>(null);

  // Open a confirmation dialog with a clean slate (resets a prior typed phrase).
  function openDialog(d: NonNullable<DialogKind>) {
    setTyped("");
    setReason("");
    cmd.reset();
    setDialog(d);
  }
  function closeDialog() {
    setDialog(null);
  }

  const session = hasSession(sessionQ.data) ? sessionQ.data : null;
  const status = session?.status;
  // `mode` is gone server-side: derive the active paper/live/signal label from
  // the authoritative exec_policy + account_env (§1.3, C6).
  const mode: LiveMode = sessionModeLabel(
    session?.exec_policy,
    session?.account_env,
  );
  // The only switchable axis inside a per-env module is the EXECUTION POLICY:
  // SIGNAL (emit-only) <-> AUTO (auto-submit). The account env is fixed by which
  // module this is (paper -> simulate, live -> real); you never switch env here.
  const execPolicy: ExecPolicy = session?.exec_policy === "auto" ? "auto" : "signal";
  const accountEnv = env === "live" ? "real" : "simulate";
  const halted = session?.halt != null;
  const running = status === "RUNNING";
  const traderId = session?.trader_id ?? null;
  // The live target account is server-side only (the real acc id never reaches
  // the browser — safety). We surface the trader-id namespace the live node
  // runs under (TMS-LIVE-REAL-001 for real money) plus any non-secret target
  // label the session config carries, so the operator can verify what they are
  // about to arm without ever exposing the account number.
  const targetAccountLabel =
    typeof session?.config?.["target_account"] === "string"
      ? (session.config["target_account"] as string)
      : null;

  const noReader =
    sessionQ.error instanceof ApiError && sessionQ.error.status === 503;

  const errorMessage =
    cmd.error instanceof ApiError
      ? `${cmd.error.code}: ${cmd.error.message}`
      : cmd.error
        ? cmd.error.message
        : null;

  function send(
    name: CommandName,
    extra?: {
      exec_policy?: ExecPolicy;
      env?: string;
      reason?: string;
      confirm_token?: string;
    },
    label?: string,
  ) {
    cmd.mutate(
      { name, ...extra },
      {
        onSuccess: () => {
          closeDialog();
          setAccepted(label ?? name);
          window.setTimeout(() => setAccepted(null), 4000);
        },
      },
    );
  }

  function setExec(target: ExecPolicy) {
    if (target === "signal") {
      // SIGNAL (emit-only) needs no confirmation.
      send("set_mode", { exec_policy: "signal" }, "switch to SIGNAL");
    } else {
      // AUTO arms auto-submission against the module's bound account env; both
      // paper and live require a typed confirm token (live is real money).
      openDialog({ kind: "arm" });
    }
  }

  if (sessionQ.isLoading) {
    return (
      <Card data-testid="session-controls">
        <CardHeader>
          <CardTitle className="text-sm">Controls</CardTitle>
        </CardHeader>
        <CardContent>
          <Skeleton className="h-24 w-full" />
        </CardContent>
      </Card>
    );
  }

  return (
    <Card data-testid="session-controls" data-disabled={noReader ? "true" : "false"}>
      <CardHeader>
        <CardTitle className="text-sm">Controls</CardTitle>
        {session ? (
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
            mode <ModeBadge mode={mode} />
          </span>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-4">
        {noReader ? (
          <Alert variant="warning" data-testid="controls-no-reader">
            <AlertDescription>
              The API has no command enqueuer / live reader configured. Controls
              are disabled until a live node is wired up.
            </AlertDescription>
          </Alert>
        ) : null}

        {accepted ? (
          <Alert data-testid="controls-accepted">
            <Check className="size-4" />
            <AlertDescription>
              Command accepted ({accepted}) — pending. tms-live applies it under
              audit; the session will reflect it shortly.
            </AlertDescription>
          </Alert>
        ) : null}

        {errorMessage && !dialog ? (
          <Alert variant="destructive" data-testid="controls-error">
            <AlertDescription>{errorMessage}</AlertDescription>
          </Alert>
        ) : null}

        {/* Session lifecycle */}
        <div className="space-y-2">
          <p className="text-[10px] uppercase tracking-wide text-muted-foreground">
            Session
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              variant="default"
              size="sm"
              disabled={noReader || cmd.isPending || running}
              onClick={() => send("start", undefined, "start")}
              data-testid="control-start"
            >
              <Play /> Start
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={noReader || cmd.isPending || !running}
              onClick={() => openDialog({ kind: "stop" })}
              data-testid="control-stop"
            >
              <Square /> Stop
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={noReader || cmd.isPending || !halted}
              onClick={() => send("resume", undefined, "resume")}
              data-testid="control-resume"
            >
              <RotateCcw /> Resume
            </Button>
          </div>
        </div>

        {/* Execution policy — SIGNAL (emit-only) <-> AUTO (auto-submit). The
            account env is fixed by the module (paper -> simulate, live -> real);
            there is no paper/live switch here. */}
        <div className="space-y-2">
          <p className="text-[10px] uppercase tracking-wide text-muted-foreground">
            Execution
          </p>
          <div className="flex flex-wrap gap-2">
            {(["signal", "auto"] as const).map((p) => (
              <Button
                key={p}
                variant={execPolicy === p ? "secondary" : "outline"}
                size="sm"
                disabled={noReader || cmd.isPending || execPolicy === p}
                onClick={() => setExec(p)}
                data-testid={`control-exec-${p}`}
                data-control={`control-exec-${p}`}
                data-active={execPolicy === p ? "true" : "false"}
                // Arming AUTO on the LIVE module is real money: destructive accent.
                className={
                  p === "auto" && env === "live" && execPolicy !== "auto"
                    ? "border-destructive/50 text-destructive hover:bg-destructive/10"
                    : undefined
                }
              >
                {p.toUpperCase()}
              </Button>
            ))}
          </div>
          <p className="text-[11px] text-muted-foreground">
            {env === "live"
              ? "AUTO arms auto-submission of REAL orders against the bound real-money account (requires a typed confirmation token; audited). SIGNAL emits intents only — no orders."
              : "AUTO auto-submits orders against the bound SIMULATE account — no real money (requires a typed confirmation token; audited). SIGNAL emits intents only — no orders."}
          </p>
        </div>

        {/* Position controls (paper/live) */}
        <div className="space-y-2">
          <p className="flex items-center gap-1.5 text-[10px] uppercase tracking-wide text-muted-foreground">
            <Scale className="size-3.5" /> Positions
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              variant="destructive"
              size="sm"
              disabled={noReader || cmd.isPending}
              onClick={() => openDialog({ kind: "flatten" })}
              // `live-flatten-button` is the e2e contract (spec 26).
              data-testid="live-flatten-button"
              data-control="control-flatten"
            >
              <Flame /> Flatten all
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={noReader || cmd.isPending}
              onClick={() =>
                send("reconcile", undefined, "reconcile")
              }
              data-testid="control-reconcile"
            >
              <Scale /> Reconcile
            </Button>
          </div>
          <p className="text-[11px] text-muted-foreground">
            FLATTEN submits market orders closing every open position (idempotent,
            confirmation-gated). Reconcile compares broker positions to the
            strategy books — read-only, never auto-corrects.
          </p>
        </div>

        {/* Safety actions */}
        <div className="space-y-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
          <p className="flex items-center gap-1.5 text-[10px] uppercase tracking-wide text-destructive">
            <ShieldAlert className="size-3.5" /> Safety
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              variant="destructive"
              size="sm"
              disabled={noReader || cmd.isPending || halted}
              onClick={() => openDialog({ kind: "halt" })}
              data-testid="live-halt-button"
              data-control="control-halt"
            >
              <OctagonX /> Halt
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={noReader || cmd.isPending}
              onClick={() => openDialog({ kind: "kill" })}
              data-testid="control-kill"
            >
              <Power /> Kill switch
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={noReader || cmd.isPending}
              onClick={() => openDialog({ kind: "emergency_kill" })}
              data-testid="control-emergency-kill"
            >
              <Siren /> Emergency kill
            </Button>
          </div>
          <p className="text-[11px] text-muted-foreground">
            Halt and kill stop new-intent emission + opening orders and set halt
            state. EMERGENCY KILL additionally flattens ALL positions then stops —
            the all-stop. In signal mode there are no positions to flatten.
          </p>
        </div>
      </CardContent>

      {/* ---- Confirmation dialogs (one open at a time; share the typed/reason
           state, which lives in this component so polling can't clear it) ---- */}
      <ConfirmActionDialog
        open={dialog?.kind === "halt"}
        onClose={closeDialog}
        title="Halt the live session"
        description="Stops emitting new signal intents and sets the session to HALTED. Reversible via Resume."
        confirmPhrase="HALT"
        confirmLabel="Halt session"
        requireReason
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "halt" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() =>
          send("halt", { reason: reason.trim() || "operator halt" }, "halt")
        }
        data-testid="live-halt-confirm"
        reasonTestId="live-halt-reason"
      />
      <ConfirmActionDialog
        open={dialog?.kind === "kill"}
        onClose={closeDialog}
        title="Kill switch"
        description="Emergency stop: halts new-intent emission and flips the kill state. Use when something is wrong."
        confirmPhrase="KILL"
        confirmLabel="Engage kill switch"
        requireReason
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "kill" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() =>
          send("kill", { reason: reason.trim() || "operator kill" }, "kill")
        }
        data-testid="kill-dialog"
      />
      <ConfirmActionDialog
        open={dialog?.kind === "stop"}
        onClose={closeDialog}
        title="Stop the live session"
        description="Gracefully stops the session. Start again to resume a fresh session."
        confirmPhrase="STOP"
        confirmLabel="Stop session"
        requireReason
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "stop" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() =>
          send("stop", { reason: reason.trim() || "operator stop" }, "stop")
        }
        data-testid="stop-dialog"
      />
      <ConfirmActionDialog
        open={dialog?.kind === "arm"}
        onClose={closeDialog}
        title={env === "live" ? "Arm AUTO — LIVE (real money)" : "Arm AUTO — paper"}
        description={
          env === "live" ? (
            <span className="space-y-2">
              <span className="block font-medium text-destructive">
                Arming AUTO lets the session place REAL orders against the bound
                real-money account.
              </span>
              <span
                className="block rounded-md border border-destructive/40 bg-destructive/5 px-2.5 py-2 text-xs"
                data-testid="live-arm-target-account"
              >
                Target account:{" "}
                <span className="font-mono font-medium">
                  {targetAccountLabel ?? traderId ?? "the configured real account"}
                </span>
                <span className="mt-1 block text-muted-foreground">
                  The real account id stays server-side and is never shown in the
                  browser. The live node must already have it configured plus a
                  successful UnlockTrade — there is no path to a real order
                  otherwise.
                </span>
              </span>
              <span className="block text-xs text-muted-foreground">
                This requires the typed confirm token below; the switch is
                audited.
              </span>
            </span>
          ) : (
            "AUTO auto-submits orders against the bound SIMULATE account (no real money). This requires a confirm token; the switch is audited."
          )
        }
        confirmPhrase={env === "live" ? "ARM LIVE" : "ARM PAPER"}
        confirmLabel={env === "live" ? "Arm AUTO (LIVE)" : "Arm AUTO (paper)"}
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "arm" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() => {
          if (dialog?.kind !== "arm") return;
          // The typed phrase is the confirm_token the API requires to arm AUTO.
          // env is fixed by the module (paper -> simulate, live -> real).
          send(
            "set_mode",
            { exec_policy: "auto", env: accountEnv, confirm_token: typed.trim() },
            env === "live" ? "arm AUTO (LIVE)" : "arm AUTO (paper)",
          );
        }}
        data-testid="live-arm-confirm"
      />
      <ConfirmActionDialog
        open={dialog?.kind === "flatten"}
        onClose={closeDialog}
        title="Flatten ALL positions"
        description="Submits market orders that close EVERY open position. Idempotent — re-running is safe. FLAT/closing orders bypass the allocator budget and are allowed even under a daily-loss halt."
        confirmPhrase="FLATTEN"
        confirmLabel="Flatten all positions"
        requireReason
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "flatten" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() =>
          send(
            "flatten",
            { reason: reason.trim() || "operator flatten", confirm_token: typed.trim() },
            "flatten",
          )
        }
        data-testid="live-flatten-confirm"
        reasonTestId="flatten-reason"
      />
      <ConfirmActionDialog
        open={dialog?.kind === "emergency_kill"}
        onClose={closeDialog}
        title="EMERGENCY KILL"
        description="The all-stop: halts new-intent emission + opening orders, FLATTENS every open position with market orders, then stops the session. Use when something is wrong and the book must be closed now."
        confirmPhrase="EMERGENCY"
        confirmLabel="Halt, flatten & stop"
        requireReason
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "emergency_kill" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() =>
          send(
            "emergency_kill",
            {
              reason: reason.trim() || "operator emergency kill",
              confirm_token: typed.trim(),
            },
            "emergency kill",
          )
        }
        data-testid="emergency-kill-dialog"
        reasonTestId="emergency-kill-reason"
      />
    </Card>
  );
}
