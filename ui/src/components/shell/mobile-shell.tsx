"use client";

import { Suspense } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { CircleDot } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  NAV_SECTIONS,
  isSectionActive,
  activeSectionLabel,
} from "@/components/shell/nav";
import { ThemeToggle } from "@/components/shell/theme-toggle";
import { ModeToggle } from "@/components/shell/mode-toggle";
import { AccountSelector } from "@/components/portfolio/account-selector";

/**
 * <MobileShell> — the touch-first chrome that wraps the shared content area when
 * `useUiMode().mode === "mobile"` (the layout branches on it; the content tree is
 * identical to desktop — LOCKED DECISION 2: full feature set, nothing hidden).
 *
 * Two fixed bars frame a scrolling content column:
 *   - TOP APP BAR — the current page title (derived from the active top-level
 *     route), the global account selector (top-right, as the design has it — it
 *     drives the unified /trade surface's paper|live binding) and the
 *     desktop/mobile/auto <ModeToggle> + theme toggle.
 *   - BOTTOM TAB BAR — the four top-levels (Systems & Data / Strategies /
 *     Compositions / Trade) as icon+label tabs; the active route is highlighted.
 *
 * Every tap target is ≥44px (Apple HIG / WCAG 2.5.5). The content area is padded
 * top + bottom to clear the two fixed bars, and bottom-inset-safe for notched
 * devices.
 */
export function MobileShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col" data-testid="mobile-shell">
      <MobileAppBar />
      {/* pt clears the fixed app bar (h-14); pb clears the fixed tab bar (h-16)
          plus the safe-area inset. */}
      <div
        className="flex min-w-0 flex-1 flex-col pt-14 pb-[calc(4rem+env(safe-area-inset-bottom))]"
        data-testid="mobile-content"
      >
        {children}
      </div>
      <MobileTabBar />
    </div>
  );
}

/** Fixed top app bar: title + account selector + mode/theme toggles. */
function MobileAppBar() {
  const pathname = usePathname();
  const title = activeSectionLabel(pathname);
  return (
    <header
      data-testid="mobile-app-bar"
      className="fixed inset-x-0 top-0 z-30 flex h-14 items-center gap-2 border-b border-border bg-background/90 px-3 backdrop-blur"
    >
      <CircleDot className="size-5 shrink-0 text-sidebar-primary" />
      <h1 className="min-w-0 flex-1 truncate text-sm font-semibold" data-testid="mobile-title">
        {title}
      </h1>
      {/* The account selector reads `?account=` via useSearchParams, so it must
          sit behind a Suspense boundary for clean prerender. `compact` lets it
          shrink + cap its width so the bar never overflows on a narrow phone. */}
      <Suspense fallback={null}>
        <AccountSelector compact />
      </Suspense>
      {/* `iconOnly` keeps the three-way toggle compact even when a desktop is
          forced into the mobile shell (sm: would otherwise reveal all labels). */}
      <ModeToggle iconOnly className="shrink-0" />
      <ThemeToggle variant="bar" />
    </header>
  );
}

/** Fixed bottom tab bar: the four top-levels as icon+label tabs. */
function MobileTabBar() {
  const pathname = usePathname();
  return (
    <nav
      data-testid="mobile-tab-bar"
      aria-label="Primary"
      className="fixed inset-x-0 bottom-0 z-30 flex h-16 items-stretch border-t border-border bg-background/95 pb-[env(safe-area-inset-bottom)] backdrop-blur"
    >
      {NAV_SECTIONS.map((s) => {
        const active = isSectionActive(pathname, s.href);
        const Icon = s.icon;
        return (
          <Link
            key={s.href}
            href={s.href}
            data-testid={s.testid}
            data-active={active ? "true" : "false"}
            aria-current={active ? "page" : undefined}
            className={cn(
              // flex-1 splits the bar four ways; min-h-11 (44px) guarantees the
              // touch target even before the bar's own h-16.
              "flex min-h-11 flex-1 flex-col items-center justify-center gap-0.5 px-1 text-[11px] font-medium transition-colors",
              active
                ? "text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <Icon className={cn("size-5", active && "text-sidebar-primary")} />
            <span className="truncate">{s.shortLabel}</span>
          </Link>
        );
      })}
    </nav>
  );
}
