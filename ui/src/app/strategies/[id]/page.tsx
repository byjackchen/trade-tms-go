"use client";

import { use } from "react";
import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { StrategyDetails } from "@/components/strategies/strategy-details";
import { useStrategy } from "@/lib/api/hooks";

/**
 * The single-strategy DETAILS deep-link (`/strategies/{id}`). The primary surface
 * for a strategy is now the tabbed Strategies page (details + watchlist + live +
 * tune); this route stays available as a focused, linkable details view backed by
 * the same `StrategyDetails` component (GET /strategies/{id}).
 */
export default function StrategyDetailPage(props: {
  params: Promise<{ id: string }>;
}) {
  const { id } = use(props.params);
  const query = useStrategy(id);
  const m = query.data?.strategy;

  return (
    <main
      className="mx-auto w-full max-w-5xl flex-1 space-y-4 p-6"
      data-testid="strategy-detail-route"
    >
      {/* Inline heading — the STRATEGY name (not the module name), so it isn't
          redundant with the top app bar. */}
      <div className="flex items-center gap-3">
        <Link
          href="/strategies"
          className="text-muted-foreground hover:text-foreground"
          data-testid="strategy-back"
        >
          <ArrowLeft className="size-4" />
        </Link>
        <h1 className="text-base font-semibold">{m?.label ?? "Strategy"}</h1>
        <span className="text-sm text-muted-foreground">{m?.id ?? id}</span>
      </div>

      <StrategyDetails strategyId={id} />
    </main>
  );
}
