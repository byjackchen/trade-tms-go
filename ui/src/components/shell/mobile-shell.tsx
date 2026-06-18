"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { CircleDot } from "lucide-react";
import { cn } from "@/lib/utils";
import { activeSectionLabel } from "@/components/shell/nav";
import { useNavItems, navLinkProps } from "@/components/shell/nav-item";
import { ThemeToggle } from "@/components/shell/theme-toggle";
import { ModeToggle } from "@/components/shell/mode-toggle";

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
      {/* pt clears the fixed app bar (h-14 + top safe-area inset); pb clears the
          fixed tab bar (h-16) plus the bottom safe-area inset. */}
      <div
        className="flex min-w-0 flex-1 flex-col pt-[calc(3.5rem+env(safe-area-inset-top))] pb-[calc(4rem+env(safe-area-inset-bottom))]"
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
      // pt adds the top safe-area inset ABOVE the h-14 row so the title/toggles
      // clear the notch/status bar under `viewport-fit=cover` (PWA standalone).
      className="fixed inset-x-0 top-0 z-30 border-b border-border bg-background/90 pt-[env(safe-area-inset-top)] backdrop-blur"
    >
      <div className="flex h-14 items-center gap-2 px-3">
        <CircleDot className="size-5 shrink-0 text-sidebar-primary" />
        <h1 className="min-w-0 flex-1 truncate text-sm font-semibold" data-testid="mobile-title">
          {title}
        </h1>
        {/* Account selection is NOT global — it lives only inside /trade (bound to
            the running session there). The app bar carries just the display-mode +
            theme toggles. */}
        {/* `iconOnly` keeps the toggle compact even when a desktop is forced into
            the mobile shell (sm: would otherwise reveal labels). */}
        <ModeToggle iconOnly className="shrink-0" />
        <ThemeToggle variant="bar" />
      </div>
    </header>
  );
}

/** Fixed bottom tab bar: the four top-levels as icon+label tabs. */
function MobileTabBar() {
  const items = useNavItems();
  return (
    // The nav's own height is the 64px tab row PLUS the bottom safe-area inset,
    // added as real space BELOW the row (`pb-[env(...)]`) so the iOS home-swipe
    // bar never overlaps the tabs and the icons stay vertically centred in their
    // 64px row (rather than being squeezed by inset padding). The inset is only
    // non-zero because the viewport is `viewport-fit=cover` (see layout.tsx).
    <nav
      data-testid="mobile-tab-bar"
      aria-label="Primary"
      className="fixed inset-x-0 bottom-0 z-30 border-t border-border bg-background/95 pb-[env(safe-area-inset-bottom)] backdrop-blur"
    >
      <div className="flex h-16 items-stretch">
        {items.map((item) => {
          const { section: s, active } = item;
          const Icon = s.icon;
          return (
            <Link
              key={s.href}
              {...navLinkProps(item)}
              className={cn(
                // flex-1 splits the row four ways; min-h-11 (44px) guarantees the
                // touch target even before the row's own h-16.
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
      </div>
    </nav>
  );
}
