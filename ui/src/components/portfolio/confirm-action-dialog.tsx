"use client";

import { Sheet } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";

type Props = {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** The exact phrase the operator must type to arm the action. */
  confirmPhrase: string;
  confirmLabel: string;
  destructive?: boolean;
  requireReason?: boolean;
  pending?: boolean;
  errorMessage?: string | null;
  /** Controlled typed-phrase value + setter (owned by the parent so it survives
   * the parent's polling re-renders). */
  typed: string;
  onTypedChange: (v: string) => void;
  /** Controlled reason value + setter. */
  reason: string;
  onReasonChange: (v: string) => void;
  /** Fired when the armed confirm button is clicked. */
  onConfirm: () => void;
  "data-testid"?: string;
  /** Override the reason input's testid. Defaults to `${testId}-reason`; the
   * halt control overrides it to the flat `live-halt-reason` the e2e suite
   * (spec 21) keys off. */
  reasonTestId?: string;
};

/**
 * A guarded confirmation dialog for a dangerous live control action (mode switch
 * to paper/live, kill switch, halt). The operator must type an exact phrase to
 * arm the action; an optional reason is captured for the audit trail. The typed
 * phrase doubles as the `confirm_token` the API requires for paper/live mode
 * switches (consumed at the boundary, never persisted).
 *
 * Fully controlled: the typed-phrase / reason state is owned by the caller, so
 * the caller's background polling (session/health refetch) can re-render this
 * dialog without ever clearing what the operator has typed. The caller resets
 * the fields when it opens the dialog.
 */
export function ConfirmActionDialog({
  open,
  onClose,
  title,
  description,
  confirmPhrase,
  confirmLabel,
  destructive = true,
  requireReason = false,
  pending = false,
  errorMessage,
  typed,
  onTypedChange,
  reason,
  onReasonChange,
  onConfirm,
  "data-testid": testId = "confirm-action-dialog",
  reasonTestId,
}: Props) {
  const phraseOk = typed.trim() === confirmPhrase;
  const reasonOk = !requireReason || reason.trim().length > 0;
  const canConfirm = phraseOk && reasonOk && !pending;

  return (
    <Sheet
      open={open}
      onClose={onClose}
      title={title}
      description={description}
      data-testid={testId}
      footer={
        <>
          <Button
            variant="outline"
            onClick={onClose}
            disabled={pending}
            data-testid={`${testId}-cancel`}
          >
            Cancel
          </Button>
          <Button
            variant={destructive ? "destructive" : "default"}
            disabled={!canConfirm}
            aria-disabled={!canConfirm}
            onClick={onConfirm}
            // The e2e contract (specs 21/22) keys the arm-action button off
            // `${testId}-submit`; `${testId}-confirm` is kept as a back-compat
            // alias. aria-disabled mirrors `disabled` so the spec's guard check
            // (either attribute) holds before the phrase is typed.
            data-testid={`${testId}-submit`}
          >
            {pending ? "Working…" : confirmLabel}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {requireReason ? (
          <div className="space-y-1.5">
            <Label htmlFor="confirm-reason">Reason (audited)</Label>
            <Input
              id="confirm-reason"
              value={reason}
              onChange={(e) => onReasonChange(e.target.value)}
              placeholder="e.g. operator stop — close of session"
              disabled={pending}
              data-testid={reasonTestId ?? `${testId}-reason`}
              autoComplete="off"
            />
          </div>
        ) : null}

        <div className="space-y-1.5">
          <Label htmlFor="confirm-phrase">
            Type{" "}
            <span className="rounded bg-muted px-1 font-mono text-foreground">
              {confirmPhrase}
            </span>{" "}
            to confirm
          </Label>
          <Input
            id="confirm-phrase"
            value={typed}
            onChange={(e) => onTypedChange(e.target.value)}
            placeholder={confirmPhrase}
            disabled={pending}
            data-testid={`${testId}-phrase`}
            autoComplete="off"
          />
        </div>

        {errorMessage ? (
          <Alert variant="destructive" data-testid={`${testId}-error`}>
            <AlertDescription>{errorMessage}</AlertDescription>
          </Alert>
        ) : null}
      </div>
    </Sheet>
  );
}
