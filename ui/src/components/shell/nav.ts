import { ServerCog, Boxes, Layers, Activity, Wallet } from "lucide-react";

export type NavSection = {
  href: string;
  label: string;
  /** Short label for tight surfaces (mobile bottom-tab). */
  shortLabel: string;
  icon: React.ComponentType<{ className?: string }>;
  testid: string;
  /** Implemented now vs. placeholder. */
  ready: boolean;
};

// The five top-level sections, in pipeline order (docs/concept-alignment.md §3.4,
// C7): Systems & Data → Strategies → Compositions → Accounts → Sessions. Backtest
// lives under Compositions (a backtest's object is always a Composition); Hyperopt
// lives under Strategies (single-strategy tuning).
//
// The former unified "Trade" top-level is SPLIT into two focused top-levels:
//   - Session — RUNTIME control: the session lifecycle, the running Composition,
//     and the live tape. The session's bound account is shown read-only here.
//   - Account — the PERSISTENT book: account selection, positions, cash/pnl, the
//     synced EXTERNAL book, reconciliation, and Sync-from-broker. Manageable with
//     NO session running, in any mode (paper/live, signal/auto).
//
// Shared by BOTH shells — the desktop <Sidebar> and the mobile bottom-tab bar
// render from this single source so the IA never drifts between surfaces.
export const NAV_SECTIONS: NavSection[] = [
  { href: "/systems", label: "Systems & Data", shortLabel: "Systems", icon: ServerCog, testid: "nav-systems", ready: true },
  { href: "/strategies", label: "Strategies", shortLabel: "Strategies", icon: Boxes, testid: "nav-strategies", ready: true },
  { href: "/compositions", label: "Compositions", shortLabel: "Compositions", icon: Layers, testid: "nav-compositions", ready: true },
  { href: "/account", label: "Accounts", shortLabel: "Accounts", icon: Wallet, testid: "nav-account", ready: true },
  { href: "/session", label: "Sessions", shortLabel: "Sessions", icon: Activity, testid: "nav-session", ready: true },
];

/** True when `pathname` is within the section rooted at `href`. */
export function isSectionActive(pathname: string, href: string): boolean {
  return pathname === href || pathname.startsWith(`${href}/`);
}

/**
 * The current section's full label for the mobile app-bar title. Falls back to
 * "tms" outside the four sections (e.g. a not-found route).
 */
export function activeSectionLabel(pathname: string): string {
  const hit = NAV_SECTIONS.find((s) => isSectionActive(pathname, s.href));
  return hit?.label ?? "tms";
}
