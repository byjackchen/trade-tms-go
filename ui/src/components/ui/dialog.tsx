"use client";

import * as React from "react";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "./button";

/**
 * Minimal modal dialog built on the native <dialog> element (no extra deps).
 * Controlled via `open` / `onClose`. Renders a backdrop, traps focus via the
 * platform modal, and closes on Escape and backdrop click.
 */
function Dialog({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  className,
  "data-testid": testId,
}: {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
  className?: string;
  "data-testid"?: string;
}) {
  const ref = React.useRef<HTMLDialogElement>(null);

  React.useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  }, [open]);

  if (!open) return null;

  return (
    <dialog
      ref={ref}
      data-testid={testId}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      onClick={(e) => {
        // Backdrop click: the dialog element itself is the backdrop target.
        if (e.target === ref.current) onClose();
      }}
      className={cn(
        "m-auto w-[min(40rem,calc(100vw-2rem))] rounded-xl bg-card p-0 text-card-foreground ring-1 ring-foreground/10 backdrop:bg-black/60",
        className,
      )}
    >
      <div className="flex items-start justify-between gap-4 border-b px-5 py-4">
        <div className="space-y-1">
          <h2 className="text-base font-medium leading-snug">{title}</h2>
          {description ? (
            <p className="text-sm text-muted-foreground">{description}</p>
          ) : null}
        </div>
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={onClose}
          aria-label="Close dialog"
          data-testid="dialog-close"
        >
          <X />
        </Button>
      </div>
      <div className="px-5 py-4">{children}</div>
      {footer ? (
        <div className="flex items-center justify-end gap-2 border-t bg-muted/40 px-5 py-3">
          {footer}
        </div>
      ) : null}
    </dialog>
  );
}

export { Dialog };
