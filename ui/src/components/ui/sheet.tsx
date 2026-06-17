"use client";

import * as React from "react";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "./button";
import { useUiMode } from "@/components/shell/ui-mode-provider";

/**
 * <Sheet> — a bottom sheet built on the same native <dialog> primitive as
 * <Dialog> (no extra deps; matches the codebase — radix is not installed).
 *
 * On MOBILE it is full-width and slides up from the bottom (`md` rounded top
 * corners, content scrolls). On DESKTOP it falls back to the centered modal —
 * visually identical to <Dialog> — so the same component works on both shells.
 * The surface is chosen from `useUiMode().mode` (the explicit `ui-mode` cookie),
 * NOT a CSS breakpoint, so a forced-mobile desktop also gets the bottom sheet.
 *
 * ── API ──────────────────────────────────────────────────────────────────────
 * Intentionally the SAME controlled shape as <Dialog>:
 *   open, onClose, title, description?, children, footer?, className?, data-testid?
 *
 * The 18 existing dialogs swap to a bottom sheet by changing only the import +
 * tag name (`Dialog` -> `Sheet`); every prop carries over unchanged.
 *
 * ── Usage ────────────────────────────────────────────────────────────────────
 *   <Sheet open={open} onClose={onClose} title="Refresh market data"
 *     description="Enqueue a data.refresh job." footer={<Button…/>}>
 *     {form}
 *   </Sheet>
 */
function Sheet({
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
  const { mode } = useUiMode();
  const bottom = mode === "mobile";
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
      data-slot="sheet"
      data-side={bottom ? "bottom" : "center"}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      onClick={(e) => {
        // Backdrop click: the dialog element itself is the backdrop target.
        if (e.target === ref.current) onClose();
      }}
      className={cn(
        "bg-card p-0 text-card-foreground ring-1 ring-foreground/10 backdrop:bg-black/60",
        bottom
          ? // Bottom sheet: pinned to the bottom edge, full width, slides up.
            "mt-auto mb-0 mr-0 ml-0 max-h-[90vh] w-screen max-w-none translate-y-0 sheet-slide-up rounded-t-2xl rounded-b-none"
          : // Desktop: centered modal, identical to <Dialog>.
            "m-auto w-[min(40rem,calc(100vw-2rem))] rounded-xl",
        className,
      )}
    >
      <div className="flex max-h-[inherit] flex-col">
        {bottom ? (
          <div className="flex justify-center pt-2.5">
            <span
              aria-hidden
              className="h-1 w-9 rounded-full bg-foreground/20"
            />
          </div>
        ) : null}
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
            aria-label="Close sheet"
            data-testid="sheet-close"
          >
            <X />
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          {children}
        </div>
        {footer ? (
          <div className="flex items-center justify-end gap-2 border-t bg-muted/40 px-5 py-3">
            {footer}
          </div>
        ) : null}
      </div>
    </dialog>
  );
}

export { Sheet };
