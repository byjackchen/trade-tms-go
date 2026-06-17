"use client";

import { useState } from "react";
import { Banknote, FlaskConical, ShieldAlert } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet } from "@/components/ui/sheet";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { MANUAL_LIVE_CONFIRM_PHRASE } from "@/lib/api/types";

/**
 * The desk's LIVE-arm switch — the ONLY path that flips the manual desk's UI into
 * real-money mode. SAFETY (top criterion): clicking the switch can NEVER auto-arm
 * live; it only opens a guarded confirmation whose submit stays disabled until the
 * operator types the EXACT phrase `I CONFIRM THIS REAL MONEY MANUAL ORDER`. A
 * wrong/near-miss phrase never arms it. There is deliberately NO direct
 * "place live order" affordance anywhere — arming live is a separate, explicit,
 * guarded step, and every subsequent live order STILL carries the per-order
 * confirm phrase that the server re-checks (412 on any mismatch).
 *
 * In the gate the desk is paper/mock, so arming is a UI guard: even when armed,
 * the server enforces the full 4-factor live activation, and a paper/signal desk
 * can never reach a real account. Disarming returns the desk to paper instantly.
 */
export function LiveArmSwitch({
  armed,
  onArm,
  onDisarm,
  locked = false,
}: {
  armed: boolean;
  onArm: () => void;
  onDisarm: () => void;
  /**
   * When the Portfolio is bound to a genuinely real account (Live Trade), the
   * desk is unconditionally armed and there is no "disarm to paper" — `locked`
   * renders the armed acknowledgement bar WITHOUT the disarm affordance.
   */
  locked?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [phrase, setPhrase] = useState("");

  const exact = phrase.trim() === MANUAL_LIVE_CONFIRM_PHRASE;

  function openDialog() {
    setPhrase("");
    setOpen(true);
  }
  function closeDialog() {
    setOpen(false);
    setPhrase("");
  }
  function confirm() {
    if (!exact) return;
    onArm();
    closeDialog();
  }

  if (armed) {
    return (
      <div
        className="flex flex-wrap items-center gap-3 rounded-lg border border-destructive/60 bg-destructive/10 px-3 py-2 text-destructive"
        data-testid="manual-live-armed-bar"
      >
        <Banknote className="size-4 shrink-0" />
        <span className="text-sm font-semibold">
          LIVE armed — orders target the real-money account
        </span>
        {locked ? null : (
          <Button
            variant="outline"
            size="sm"
            className="ml-auto"
            onClick={onDisarm}
            data-testid="manual-mode-paper"
          >
            <FlaskConical /> Disarm (back to paper)
          </Button>
        )}
      </div>
    );
  }

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          onClick={openDialog}
          className="border-destructive/50 text-destructive hover:bg-destructive/10"
          data-testid="manual-mode-live"
        >
          <Banknote /> Arm LIVE (real money)
        </Button>
        <span className="text-[11px] text-muted-foreground">
          Arming requires the exact typed phrase. No order reaches a real account
          without the full server-side live gate.
        </span>
      </div>

      <Sheet
        open={open}
        onClose={closeDialog}
        data-testid="manual-live-confirm"
        title="Arm REAL-MONEY manual trading"
        description={
          <span className="block font-medium text-destructive">
            This switches the desk to LIVE. Subsequent manual orders place REAL
            orders against the real-money account. Type the exact phrase to arm.
          </span>
        }
        footer={
          <>
            <Button
              variant="outline"
              onClick={closeDialog}
              data-testid="manual-live-confirm-cancel"
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={!exact}
              aria-disabled={!exact}
              onClick={confirm}
              data-testid="manual-live-confirm-submit"
            >
              Arm LIVE
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <Alert variant="destructive">
            <ShieldAlert className="size-4" />
            <AlertDescription>
              The live desk must already have the real account configured plus a
              successful UnlockTrade and the {`TMS-LIVE-REAL-001`} trader id — there
              is no path to a real order otherwise. The real account id stays
              server-side and is never shown here.
            </AlertDescription>
          </Alert>
          <div className="space-y-1.5">
            <Label htmlFor="live-arm-phrase">
              Type{" "}
              <span className="rounded bg-muted px-1 font-mono text-foreground">
                {MANUAL_LIVE_CONFIRM_PHRASE}
              </span>{" "}
              to confirm
            </Label>
            <Input
              id="live-arm-phrase"
              value={phrase}
              onChange={(e) => setPhrase(e.target.value)}
              placeholder={MANUAL_LIVE_CONFIRM_PHRASE}
              autoComplete="off"
              data-testid="manual-live-confirm-phrase"
            />
          </div>
        </div>
      </Sheet>
    </>
  );
}
