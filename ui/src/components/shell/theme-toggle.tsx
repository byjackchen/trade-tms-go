"use client";

import { useTheme } from "next-themes";
import { useEffect, useState } from "react";
import { Moon, Sun } from "lucide-react";
import { cn } from "@/lib/utils";

/**
 * Light/dark theme toggle, shared by both shells (sidebar footer on desktop,
 * app bar on mobile). `variant="bar"` renders an icon-only ≥44px touch target
 * for the mobile app bar; the default renders the full-width labelled row used
 * in the desktop sidebar footer.
 *
 * Hydration guard: the resolved theme is only known client-side, so the
 * icon/label are deferred until mounted to avoid an SSR/CSR mismatch
 * (the canonical next-themes pattern).
 */
export function ThemeToggle({ variant = "row" }: { variant?: "row" | "bar" }) {
  const { resolvedTheme, setTheme } = useTheme();
  const [mounted, setMounted] = useState(false);
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => setMounted(true), []);
  const isDark = resolvedTheme === "dark";

  const icon = mounted ? (
    isDark ? <Moon className="size-4" /> : <Sun className="size-4" />
  ) : (
    <Moon className="size-4" />
  );

  if (variant === "bar") {
    return (
      <button
        type="button"
        data-testid="theme-toggle"
        aria-label="Toggle theme"
        onClick={() => setTheme(isDark ? "light" : "dark")}
        className="inline-flex size-11 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
      >
        {icon}
      </button>
    );
  }

  return (
    <button
      type="button"
      data-testid="theme-toggle"
      aria-label="Toggle theme"
      onClick={() => setTheme(isDark ? "light" : "dark")}
      className={cn(
        "flex h-8 w-full items-center gap-2 rounded-lg px-3 text-sm text-sidebar-foreground/70 transition-colors hover:bg-sidebar-accent hover:text-sidebar-foreground",
      )}
    >
      {icon}
      <span>{mounted && !isDark ? "Light" : "Dark"} theme</span>
    </button>
  );
}
