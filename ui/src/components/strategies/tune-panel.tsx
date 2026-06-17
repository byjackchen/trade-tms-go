"use client";

import { useState } from "react";
import { Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";
import { StreamIndicator } from "@/components/systems/stream-indicator";
import { StudiesTable } from "./hyperopt/studies-table";
import { NewStudyDialog } from "./hyperopt/new-study-dialog";
import { StudyPanel } from "./hyperopt/study-panel";
import type { HyperoptStrategy } from "@/lib/api/types";

/**
 * The HYPEROPT surface for ONE strategy tab (SEPA / Sector / Pairs). This is the
 * ONLY place params are tuned in the concept-aligned IA (docs/concept-alignment.md
 * §3.4 ②): the studies list is scoped to this strategy, "Run Hyperopt" launches a
 * study locked to it, and selecting a study row opens its detail INLINE below —
 * promote a completed trial to write its params back as the strategy's active set.
 *
 * Models do NOT tune params — they COMPOSE already-tuned strategies and are
 * VALIDATED by Backtest. ORB has no Hyperopt panel (it is intraday; §3.4 A4).
 */
export function TunePanel({ strategy }: { strategy: HyperoptStrategy }) {
  const [newOpen, setNewOpen] = useState(false);
  const [selectedTs, setSelectedTs] = useState<string | null>(null);

  return (
    <div className="space-y-4" data-testid="tune-panel" data-strategy={strategy}>
      <div className="flex items-center justify-between gap-2">
        <div>
          <h3 className="text-sm font-medium">Hyperopt</h3>
          <p className="text-xs text-muted-foreground">
            Seeded NSGA-II walk-forward studies for this strategy. Promote a
            trial to write its params back as the strategy&apos;s active set.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <StreamIndicator />
          <Button
            size="sm"
            onClick={() => setNewOpen(true)}
            data-testid="open-hyperopt-dialog"
          >
            <Sparkles />
            Run Hyperopt
          </Button>
        </div>
      </div>

      <StudiesTable
        strategy={strategy}
        selectedTs={selectedTs}
        onSelect={(ts) => setSelectedTs(ts)}
      />

      {selectedTs ? (
        <StudyPanel ts={selectedTs} onClose={() => setSelectedTs(null)} />
      ) : null}

      {newOpen ? (
        <NewStudyDialog
          open={newOpen}
          onClose={() => setNewOpen(false)}
          defaultStrategy={strategy}
          lockStrategy
          onView={(ts) => setSelectedTs(ts)}
        />
      ) : null}
    </div>
  );
}
