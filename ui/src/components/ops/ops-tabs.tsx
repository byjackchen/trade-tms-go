"use client";

import { cn } from "@/lib/utils";

export type OpsTab = "queue" | "audit" | "health";

const TABS: { id: OpsTab; label: string; testid: string }[] = [
  { id: "queue", label: "Job queue", testid: "ops-tab-queue" },
  { id: "audit", label: "Audit log", testid: "ops-tab-audit" },
  { id: "health", label: "System health", testid: "ops-tab-health" },
];

/** In-page sub-navigation across the three Ops views. */
export function OpsTabs({
  active,
  onChange,
}: {
  active: OpsTab;
  onChange: (t: OpsTab) => void;
}) {
  return (
    <nav
      className="flex items-center gap-1 border-b border-border px-6"
      data-testid="ops-tabs"
    >
      {TABS.map((t) => {
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
              "border-b-2 px-3 py-2 text-sm font-medium transition-colors",
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
