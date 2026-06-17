"use client";

import { useRouter } from "next/navigation";
import { Monitor, Smartphone } from "lucide-react";
import { cn } from "@/lib/utils";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import type { UiMode } from "@/lib/ui-mode";

// Only the two explicit surfaces are selectable. There is no "Auto" button:
// with no stored preference the mode is auto-resolved from the device, and the
// moment the user picks Desktop/Mobile that choice is remembered (cookie). The
// active segment tracks the *resolved* mode, so on a phone "Mobile" lights up
// even before any manual pick.
const OPTIONS: {
  pref: UiMode;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
}[] = [
  { pref: "desktop", label: "Desktop", icon: Monitor },
  { pref: "mobile", label: "Mobile", icon: Smartphone },
];

/**
 * A compact desktop / mobile segmented control (top utility bar on desktop, app
 * bar on mobile). Selecting an option sets the `ui-mode` preference (persisted
 * to the cookie by the provider) and calls `router.refresh()` so the server
 * re-seeds `<html data-ui-mode>` from the new cookie — keeping SSR and CSR in
 * agreement on the very next render.
 *
 * There is no explicit "auto" segment: an unset preference auto-resolves from
 * the device, and a manual pick is what gets remembered (LOCKED DECISION 3/4).
 */
export function ModeToggle({
  className,
  iconOnly = false,
  fullWidth = false,
}: {
  className?: string;
  /**
   * Force the three options to icon-only (no text labels), regardless of the
   * viewport width. The mobile app bar passes this so the toggle stays compact
   * even when a desktop is forced into the mobile shell (`sm:` would otherwise
   * reveal all three labels and overflow the bar — LOCKED DECISION 4).
   */
  iconOnly?: boolean;
  /**
   * Stretch the control to fill its container with three EQUAL-width segments
   * (each `flex-1`) instead of the compact inline group. The desktop sidebar
   * footer passes this so the three options divide the row evenly and line up
   * with the theme-toggle row below, rather than spreading unevenly.
   */
  fullWidth?: boolean;
}) {
  const { mode, pref, setPref } = useUiMode();
  const router = useRouter();

  const choose = (next: UiMode) => {
    if (next !== pref) {
      setPref(next);
      // Re-seed the server-rendered <html data-ui-mode>; the cookie was just
      // written synchronously above, so the RSC refresh reads the new value.
      router.refresh();
    }
  };

  return (
    <div
      role="radiogroup"
      aria-label="Display mode"
      data-testid="mode-toggle"
      className={cn(
        "items-center gap-0.5 rounded-lg border border-border bg-background p-0.5",
        fullWidth ? "flex w-full" : "inline-flex",
        className,
      )}
    >
      {OPTIONS.map((o) => {
        const Icon = o.icon;
        // Highlight the RESOLVED surface (so device-auto shows the right one),
        // not the stored preference (which may be unset/auto).
        const active = mode === o.pref;
        return (
          <button
            key={o.pref}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={o.label}
            title={o.label}
            data-testid={`mode-toggle-${o.pref}`}
            data-active={active ? "true" : "false"}
            onClick={() => choose(o.pref)}
            className={cn(
              "inline-flex h-7 items-center justify-center gap-1.5 rounded-md px-2 text-xs font-medium transition-colors",
              fullWidth && "flex-1",
              active
                ? "bg-muted text-foreground"
                : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
            )}
          >
            <Icon className="size-4" />
            {!iconOnly ? (
              <span className="hidden sm:inline">{o.label}</span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}
