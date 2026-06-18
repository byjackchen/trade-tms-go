"use client";

import { usePathname } from "next/navigation";
import { NAV_SECTIONS, isSectionActive, type NavSection } from "@/components/shell/nav";

/** A nav section paired with its resolved active state for the current route. */
export type ResolvedNavItem = { section: NavSection; active: boolean };

/**
 * The single source of nav state shared by BOTH shells (desktop <Sidebar> +
 * mobile bottom-tab bar): reads the current pathname once and resolves each
 * NAV_SECTIONS entry's active flag. Layout (label vs shortLabel, icon size,
 * the P2+ badge, the className) is intentionally NOT shared — the two rails are
 * a vertical labelled rail and a horizontal icon tab and legitimately differ —
 * so callers map over this and lay out their own item; only the active-state
 * computation and the identity-bearing <Link> attributes are common.
 */
export function useNavItems(): ResolvedNavItem[] {
  const pathname = usePathname();
  return NAV_SECTIONS.map((section) => ({
    section,
    active: isSectionActive(pathname, section.href),
  }));
}

/**
 * The <Link> attributes both shells render identically for a nav section: the
 * routing href, the per-section test id, and the active-state hooks
 * (`data-active` + `aria-current`). Callers spread this and supply their own
 * `className` + children. `key` is intentionally omitted (React-reserved —
 * the caller sets it).
 */
export function navLinkProps(item: ResolvedNavItem) {
  const { section, active } = item;
  return {
    href: section.href,
    "data-testid": section.testid,
    "data-active": active ? "true" : ("false" as const),
    "aria-current": active ? ("page" as const) : undefined,
  };
}
