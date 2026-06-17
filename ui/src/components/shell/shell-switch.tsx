"use client";

import { useUiMode } from "@/components/shell/ui-mode-provider";
import { Sidebar } from "@/components/shell/sidebar";
import { MobileShell } from "@/components/shell/mobile-shell";

/**
 * Branches the app chrome on the resolved UI mode (docs/concept-alignment.md,
 * LOCKED DECISIONS 3 & 4): the desktop <Sidebar> shell vs. the <MobileShell>
 * (fixed top app bar + bottom tab bar). The CONTENT tree (`children`) is shared
 * verbatim across both — only the chrome differs.
 *
 * `useUiMode().mode` is seeded from the server-resolved `initialMode` (read from
 * the `ui-mode` cookie / User-Agent in the root layout and reflected on
 * <html data-ui-mode>), so the first client render matches SSR exactly — no
 * hydration mismatch, no shell flash.
 */
export function ShellSwitch({ children }: { children: React.ReactNode }) {
  const { mode } = useUiMode();

  if (mode === "mobile") {
    return <MobileShell>{children}</MobileShell>;
  }

  return (
    <div className="flex min-h-screen" data-testid="app-shell">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">{children}</div>
    </div>
  );
}
