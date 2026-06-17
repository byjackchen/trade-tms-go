"use client";

import { use } from "react";
import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
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
    <>
      <PageHeader
        title={
          <span className="flex items-center gap-2">
            <Link
              href="/strategies"
              className="text-muted-foreground hover:text-foreground"
              data-testid="strategy-back"
            >
              <ArrowLeft className="size-4" />
            </Link>
            {m?.label ?? "Strategy"}
          </span>
        }
        subtitle={m ? m.id : id}
        data-testid="strategy-detail-header"
      />

      <main
        className="mx-auto w-full max-w-5xl flex-1 space-y-4 p-6"
        data-testid="strategy-detail-route"
      >
        <StrategyDetails strategyId={id} />
      </main>
    </>
  );
}
