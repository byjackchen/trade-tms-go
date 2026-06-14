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
import { ModeBadge } from "./live-badges";
import type { CommandName, LiveMode } from "@/lib/api/types";

type DialogKind =
  | { kind: "halt" }
  | { kind: "kill" }
  | { kind: "stop" }
  | { kind: "mode"; mode: "paper" | "live" }
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
export function SessionControls() {
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
  const mode: LiveMode = session?.mode ?? "signal";
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
    extra?: { mode?: LiveMode; reason?: string; confirm_token?: string },
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

  function setMode(target: LiveMode) {
    if (target === "signal") {
      // signal needs no confirmation.
      send("set_mode", { mode: "signal" }, "switch to SIGNAL");
    } else {
      openDialog({ kind: "mode", mode: target as "paper" | "live" });
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

        {/* Mode switch */}
        <div className="space-y-2">
          <p className="text-[10px] uppercase tracking-wide text-muted-foreground">
            Mode
          </p>
          <div className="flex flex-wrap gap-2">
            {(["signal", "paper", "live"] as const).map((m) => (
              <Button
                key={m}
                variant={mode === m ? "secondary" : "outline"}
                size="sm"
                disabled={noReader || cmd.isPending || mode === m}
                onClick={() => setMode(m)}
                // The e2e suite (spec 22) keys the paper switch off
                // `live-mode-switch-paper`; the other modes keep `control-mode-*`.
                data-testid={m === "paper" ? "live-mode-switch-paper" : `control-mode-${m}`}
                data-control={`control-mode-${m}`}
                data-active={mode === m ? "true" : "false"}
                // Live (real money) gets a destructive accent to make it
                // unmistakable from signal/paper.
                className={
                  m === "live" && mode !== "live"
                    ? "border-destructive/50 text-destructive hover:bg-destructive/10"
                    : undefined
                }
              >
                {m.toUpperCase()}
              </Button>
            ))}
          </div>
          <p className="text-[11px] text-muted-foreground">
            paper / live require a typed confirmation phrase + token (consumed at
            the API boundary, never persisted). LIVE places real orders — it
            shows the target account and demands an explicit confirmation. In
            signal mode no orders are ever submitted.
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
        open={dialog?.kind === "mode"}
        onClose={closeDialog}
        title={
          dialog?.kind === "mode"
            ? `Switch to ${dialog.mode.toUpperCase()} mode`
            : "Switch mode"
        }
        description={
          dialog?.kind === "mode" && dialog.mode === "live" ? (
            <span className="space-y-2">
              <span className="block font-medium text-destructive">
                LIVE mode places REAL orders against the real-money account.
              </span>
              <span
                className="block rounded-md border border-destructive/40 bg-destructive/5 px-2.5 py-2 text-xs"
                data-testid="live-mode-target-account"
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
            "PAPER mode simulates fills against the SIMULATE account (no real money). This requires a confirm token; the switch is audited."
          )
        }
        confirmPhrase={
          dialog?.kind === "mode" ? `SET ${dialog.mode.toUpperCase()}` : "SET"
        }
        confirmLabel={
          dialog?.kind === "mode"
            ? `Switch to ${dialog.mode.toUpperCase()}`
            : "Switch"
        }
        pending={cmd.isPending}
        errorMessage={dialog?.kind === "mode" ? errorMessage : null}
        typed={typed}
        onTypedChange={setTyped}
        reason={reason}
        onReasonChange={setReason}
        onConfirm={() => {
          if (dialog?.kind !== "mode") return;
          // The typed phrase is the confirm_token the API requires for paper/live.
          send(
            "set_mode",
            { mode: dialog.mode, confirm_token: typed.trim() },
            `switch to ${dialog.mode.toUpperCase()}`,
          );
        }}
        data-testid="live-mode-confirm"
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
