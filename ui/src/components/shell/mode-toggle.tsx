"use client";

import { useRouter } from "next/navigation";
import { Monitor, Smartphone, Sparkles } from "lucide-react";
import { cn } from "@/lib/utils";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import type { UiModePref } from "@/lib/ui-mode";

const OPTIONS: {
  pref: UiModePref;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
}[] = [
  { pref: "desktop", label: "Desktop", icon: Monitor },
  { pref: "mobile", label: "Mobile", icon: Smartphone },
  { pref: "auto", label: "Auto", icon: Sparkles },
];

/**
 * A compact desktop / mobile / auto segmented control. Selecting an option sets
 * the `ui-mode` preference (persisted to the cookie by the provider) and calls
 * `router.refresh()` so the server re-seeds `<html data-ui-mode>` from the new
 * cookie — keeping SSR and CSR in agreement on the very next render.
 *
 * Lives in `components/shell` because both shells will host it (app bar on
 * mobile, sidebar footer on desktop). Wiring into MobileShell/Sidebar lands in
 * a later phase; for now it is exported and self-contained.
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
  const { pref, setPref } = useUiMode();
  const router = useRouter();

  const choose = (next: UiModePref) => {
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
        const active = pref === o.pref;
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
