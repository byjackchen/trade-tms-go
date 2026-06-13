"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { cn } from "@/lib/utils";

type Tab = { href: string; label: string; testid: string; exact?: boolean };

const TABS: Tab[] = [
  { href: "/live", label: "Cockpit", testid: "live-tab-cockpit", exact: true },
  { href: "/live/watchlist", label: "Watchlist", testid: "live-tab-watchlist" },
  { href: "/live/strategies", label: "Strategies", testid: "live-tab-strategies" },
  { href: "/live/system", label: "System", testid: "live-tab-system" },
];

/** Sub-navigation across the four live cockpit views. */
export function LiveTabs() {
  const pathname = usePathname();
  return (
    <nav
      className="flex items-center gap-1 border-b border-border px-6"
      data-testid="live-tabs"
    >
      {TABS.map((t) => {
        const active = t.exact
          ? pathname === t.href
          : pathname === t.href || pathname.startsWith(`${t.href}/`);
        return (
          <Link
            key={t.href}
            href={t.href}
            data-testid={t.testid}
            data-active={active ? "true" : "false"}
            aria-current={active ? "page" : undefined}
            className={cn(
              "border-b-2 px-3 py-2 text-sm font-medium transition-colors",
              active
                ? "border-primary text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {t.label}
          </Link>
        );
      })}
    </nav>
  );
}

/** The canonical live strategy ids + labels (loader stems; ORB = intraday_breakout). */
export const LIVE_STRATEGIES: { id: string; label: string }[] = [
  { id: "sepa", label: "SEPA" },
  { id: "sector_rotation", label: "Sector Rotation" },
  { id: "pairs", label: "Pairs" },
  { id: "intraday_breakout", label: "Intraday Breakout (ORB)" },
];
