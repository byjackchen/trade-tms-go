"use client";

import { useState } from "react";
import { Sparkles, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { StreamIndicator } from "@/components/systems/stream-indicator";
import { StudyPanel } from "@/components/strategies/hyperopt/study-panel";
import { CompositionHyperoptDialog } from "./composition-hyperopt-dialog";
import { CompositionPromoteDialog } from "./composition-promote-dialog";
import type { Composition } from "@/lib/api/types";

/**
 * The Composition Hyperopt surface (weights & risk), opened from a Composition's
 * "Hyperopt" action. This is the COMPOSITION counterpart to the Strategies module's
 * per-strategy Tune panel:
 *
 *  - Strategy Hyperopt tunes a strategy's SIGNAL params.
 *  - Composition Hyperopt tunes this Composition's WEIGHTS & RISK — every member's
 *    strategy params stay FIXED at its active set (LOCKED DECISION 4).
 *
 * It reuses the SAME NSGA-II + walk-forward machinery (LOCKED DECISION 6) and the
 * SHARED StudyPanel (trials / Pareto / convergence) for results — only the launch
 * search space and the in-place Promote differ. Promotion OVERWRITES the
 * Composition's weights + cash + risk in place (LOCKED DECISION 3) via
 * CompositionPromoteDialog, wired in through StudyPanel's `renderPromote`.
 */
export function CompositionHyperoptPanel({
  composition,
  onClose,
}: {
  composition: Composition;
  onClose: () => void;
}) {
  const [launchOpen, setLaunchOpen] = useState(false);
  const [studyTs, setStudyTs] = useState<string | null>(null);

  return (
    <div
      className="space-y-4 rounded-lg border border-border p-4"
      data-testid="composition-hyperopt-panel"
      data-composition={composition.id}
    >
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="space-y-1">
          <h3 className="flex items-center gap-2 text-sm font-medium">
            Composition Hyperopt — weights &amp; risk
            <Badge variant="outline" className="font-mono text-[10px]">
              {composition.id}
            </Badge>
          </h3>
          <p className="max-w-2xl text-xs text-muted-foreground">
            Searches this Composition&apos;s member weights + cash + the three risk
            caps with seeded NSGA-II walk-forward. Member strategy params stay FIXED
            at their active set — retune those in the strategy&apos;s{" "}
            <span className="font-medium">
              Strategy Hyperopt — signal params
            </span>{" "}
            panel. Promote a trial to overwrite this Composition&apos;s weights &amp;
            risk in place.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <StreamIndicator />
          <Button
            size="sm"
            onClick={() => setLaunchOpen(true)}
            data-testid="composition-open-hyperopt-dialog"
          >
            <Sparkles />
            Run Hyperopt
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={onClose}
            data-testid="composition-hyperopt-panel-close"
          >
            <X />
            Close
          </Button>
        </div>
      </div>

      {studyTs ? (
        <StudyPanel
          ts={studyTs}
          onClose={() => setStudyTs(null)}
          promoteCopy="Promote to overwrite this Composition's weights & risk in place."
          renderPromote={({ trial, studyTS, onClose: closePromote }) => (
            <CompositionPromoteDialog
              open
              onClose={closePromote}
              compositionId={composition.id}
              studyTS={studyTS}
              trial={trial}
            />
          )}
        />
      ) : (
        <Alert data-testid="composition-hyperopt-empty">
          <AlertDescription>
            No study selected. Click <strong>Run Hyperopt</strong> to launch a
            weights &amp; risk search; the resulting study, trials and Pareto front
            render here.
          </AlertDescription>
        </Alert>
      )}

      {launchOpen ? (
        <CompositionHyperoptDialog
          open
          onClose={() => setLaunchOpen(false)}
          composition={composition}
          onView={(ts) => setStudyTs(ts)}
        />
      ) : null}
    </div>
  );
}
