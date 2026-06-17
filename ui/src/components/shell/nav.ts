import { ServerCog, Boxes, Layers, CandlestickChart } from "lucide-react";

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

// The four top-level sections, in pipeline order (docs/concept-alignment.md §3.4,
// C7): Systems & Data → Strategies → Compositions → Trade. Backtest lives under
// Compositions (a backtest's object is always a Composition); Hyperopt lives under
// Strategies (single-strategy tuning). Trade is the UNIFIED former Paper + Live:
// ONE account-driven surface whose paper-vs-live treatment follows the selected
// account, not a separate page.
//
// Shared by BOTH shells — the desktop <Sidebar> and the mobile bottom-tab bar
// render from this single source so the IA never drifts between surfaces.
export const NAV_SECTIONS: NavSection[] = [
  { href: "/systems", label: "Systems & Data", shortLabel: "Systems", icon: ServerCog, testid: "nav-systems", ready: true },
  { href: "/strategies", label: "Strategies", shortLabel: "Strategies", icon: Boxes, testid: "nav-strategies", ready: true },
  { href: "/compositions", label: "Compositions", shortLabel: "Compose", icon: Layers, testid: "nav-compositions", ready: true },
  { href: "/trade", label: "Trade", shortLabel: "Trade", icon: CandlestickChart, testid: "nav-trade", ready: true },
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
