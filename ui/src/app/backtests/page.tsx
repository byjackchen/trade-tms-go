"use client";

import { useState } from "react";
import { FlaskConical } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { Button } from "@/components/ui/button";
import { StreamIndicator } from "@/components/data/stream-indicator";
import { RunsTable } from "@/components/backtests/runs-table";
import { NewBacktestDialog } from "@/components/backtests/new-backtest-dialog";

export default function BacktestsPage() {
  const [newOpen, setNewOpen] = useState(false);
  const [status, setStatus] = useState("");

  return (
    <>
      <PageHeader
        title="Backtests"
        subtitle="Run, track and inspect engine backtests."
        data-testid="backtests-header"
        actions={
          <>
            <StreamIndicator />
            <Button
              size="sm"
              onClick={() => setNewOpen(true)}
              data-testid="open-backtest-dialog"
            >
              <FlaskConical />
              New backtest
            </Button>
          </>
        }
      />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="backtests-page"
      >
        <RunsTable status={status} onStatusChange={setStatus} />
      </main>

      <NewBacktestDialog open={newOpen} onClose={() => setNewOpen(false)} />
    </>
  );
}
