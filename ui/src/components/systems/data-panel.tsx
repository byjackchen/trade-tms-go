"use client";

import { useState } from "react";
import { RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { CoverageTable } from "@/components/systems/coverage-table";
import { GapHeatmap } from "@/components/systems/gap-heatmap";
import { SyncRunsTable } from "@/components/systems/sync-runs-table";
import { UniverseCard } from "@/components/systems/universe-card";
import { JobsPanel } from "@/components/systems/jobs-panel";
import { RefreshDialog } from "@/components/systems/refresh-dialog";

/**
 * Data tab body — market-data coverage, freshness, gaps, universe and sync. The
 * exact layout relocated from the retired /data page: a coverage table whose
 * ticker drill-down feeds the gap heatmap, the universe card, and the sync-runs
 * + recent-jobs row. The "Refresh data" action and the ticker-inspect
 * cross-link are local to this panel.
 */
export function DataPanel() {
  const [refreshOpen, setRefreshOpen] = useState(false);
  const [inspectTicker, setInspectTicker] = useState<string | null>(null);

  return (
    <div className="space-y-4" data-testid="systems-data">
      <div className="flex justify-end">
        <Button
          size="sm"
          onClick={() => setRefreshOpen(true)}
          data-testid="open-refresh-dialog"
        >
          <RefreshCw />
          Refresh data
        </Button>
      </div>

      <CoverageTable onInspectTicker={setInspectTicker} />

      {/* Multi-column on desktop; single stacked column on mobile (LOCKED
          DECISION 4 — the switch follows the ui-mode cookie, not a CSS
          breakpoint, so a forced-mobile desktop also stacks). */}
      <div className="grid grid-cols-1 gap-4 ui-desktop:lg:grid-cols-3">
        <div className="ui-desktop:lg:col-span-2">
          <GapHeatmap initialTicker={inspectTicker} />
        </div>
        <UniverseCard />
      </div>

      <div className="grid grid-cols-1 gap-4 ui-desktop:lg:grid-cols-2">
        <SyncRunsTable />
        <JobsPanel />
      </div>

      <RefreshDialog open={refreshOpen} onClose={() => setRefreshOpen(false)} />
    </div>
  );
}
