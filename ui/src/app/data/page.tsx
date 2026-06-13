"use client";

import { useState } from "react";
import { RefreshCw } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { Button } from "@/components/ui/button";
import { CoverageTable } from "@/components/data/coverage-table";
import { GapHeatmap } from "@/components/data/gap-heatmap";
import { SyncRunsTable } from "@/components/data/sync-runs-table";
import { UniverseCard } from "@/components/data/universe-card";
import { JobsPanel } from "@/components/data/jobs-panel";
import { RefreshDialog } from "@/components/data/refresh-dialog";
import { StreamIndicator } from "@/components/data/stream-indicator";

export default function DataPage() {
  const [refreshOpen, setRefreshOpen] = useState(false);
  const [inspectTicker, setInspectTicker] = useState<string | null>(null);

  return (
    <>
      <PageHeader
        title="Data"
        subtitle="Market-data coverage, freshness, gaps and sync."
        data-testid="data-header"
        actions={
          <>
            <StreamIndicator />
            <Button
              size="sm"
              onClick={() => setRefreshOpen(true)}
              data-testid="open-refresh-dialog"
            >
              <RefreshCw />
              Refresh data
            </Button>
          </>
        }
      />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="data-page"
      >
        <CoverageTable onInspectTicker={setInspectTicker} />

        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          <div className="lg:col-span-2">
            <GapHeatmap initialTicker={inspectTicker} />
          </div>
          <UniverseCard />
        </div>

        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <SyncRunsTable />
          <JobsPanel />
        </div>
      </main>

      <RefreshDialog open={refreshOpen} onClose={() => setRefreshOpen(false)} />
    </>
  );
}
