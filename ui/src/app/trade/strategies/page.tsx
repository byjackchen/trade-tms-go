"use client";

import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/trade/live-indicator";
import { TradeTabs, TRADE_STRATEGIES } from "@/components/trade/trade-tabs";
import { SessionBar } from "@/components/trade/session-bar";
import { StrategyLiveCard } from "@/components/trade/strategy-live-card";
import { IntentsStream } from "@/components/trade/intents-stream";

export default function TradeStrategiesPage() {
  return (
    <>
      <PageHeader
        title="Strategies — live"
        subtitle="Per-strategy live state summaries and decision counts."
        data-testid="live-strategies-header"
        actions={<LiveIndicator />}
      />
      <TradeTabs />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="live-strategies-page"
      >
        <SessionBar />

        <div
          className="grid grid-cols-1 gap-4 md:grid-cols-2"
          data-testid="strategy-live-grid"
        >
          {TRADE_STRATEGIES.map((s) => (
            <StrategyLiveCard key={s.id} strategyId={s.id} label={s.label} />
          ))}
        </div>

        <IntentsStream title="All live intents" />
      </main>
    </>
  );
}
