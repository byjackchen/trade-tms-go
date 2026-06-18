"use client";

import { Sidebar } from "@/components/shell/sidebar";
import { ModeToggle } from "@/components/shell/mode-toggle";
import { ThemeToggle } from "@/components/shell/theme-toggle";

/**
 * <DesktopShell> — the pointer-first chrome that wraps the shared content area
 * when `useUiMode().mode !== "mobile"`. A persistent vertical <Sidebar> rail on
 * the left, plus a thin top utility bar holding the display-mode toggle + theme
 * toggle TOP-RIGHT (icon-only), kept consistent with the mobile app bar. The
 * CONTENT tree (`children`) is identical to the mobile shell — only the chrome
 * differs (the symmetric counterpart of <MobileShell>).
 */
export function DesktopShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen" data-testid="app-shell">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        {/* Top utility bar: the display-mode toggle + theme live TOP-RIGHT,
            consistent with the mobile app bar (icon-only). */}
        <header className="flex h-12 shrink-0 items-center justify-end gap-1 border-b border-sidebar-border px-4">
          <ModeToggle iconOnly />
          <ThemeToggle variant="bar" />
        </header>
        <div className="flex min-w-0 flex-1 flex-col">{children}</div>
      </div>
    </div>
  );
}
