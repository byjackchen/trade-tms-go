"use client";

import { cn } from "@/lib/utils";

export type SystemsTab = "health" | "data" | "jobs" | "audit";

export const SYSTEMS_TABS: { id: SystemsTab; label: string; testid: string }[] = [
  { id: "health", label: "Health", testid: "systems-tab-health" },
  { id: "data", label: "Data", testid: "systems-tab-data" },
  { id: "jobs", label: "Jobs", testid: "systems-tab-jobs" },
  { id: "audit", label: "Audit", testid: "systems-tab-audit" },
];

/** Parse the `?tab=` query value into a known tab, defaulting to `health`. */
export function asSystemsTab(value: string | null | undefined): SystemsTab {
  switch (value) {
    case "data":
    case "jobs":
    case "audit":
      return value;
    default:
      return "health";
  }
}

/**
 * In-page sub-navigation across the four Systems & Data views. Selecting a tab
 * is a URL change (`?tab=`) so deep-links and the next.config redirects
 * (/data → ?tab=data, /ops → ?tab=jobs) land on the right view.
 */
export function SystemsTabs({
  active,
  onChange,
}: {
  active: SystemsTab;
  onChange: (t: SystemsTab) => void;
}) {
  return (
    <nav
      className="flex items-center gap-1 overflow-x-auto border-b border-border px-4 [scrollbar-width:none] md:px-6 [&::-webkit-scrollbar]:hidden"
      data-testid="systems-tabs"
    >
      {SYSTEMS_TABS.map((t) => {
        const isActive = active === t.id;
        return (
          <button
            key={t.id}
            type="button"
            data-testid={t.testid}
            data-active={isActive ? "true" : "false"}
            aria-current={isActive ? "page" : undefined}
            onClick={() => onChange(t.id)}
            className={cn(
              "flex min-h-11 shrink-0 items-center whitespace-nowrap border-b-2 px-3 text-sm font-medium transition-colors md:min-h-0 md:py-2",
              isActive
                ? "border-primary text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {t.label}
          </button>
        );
      })}
    </nav>
  );
}
