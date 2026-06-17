"use client";

import { TradeModule } from "@/components/portfolio/trade-module";

/**
 * Live Trade (#5) — `<TradeModule env="live"/>` bound to the REAL-money book
 * (real accounts). Identical to Paper Trade (#4) apart from the bound env and its
 * gating: Live keeps the loud LIVE-REAL banner + per-order confirm gating
 * (docs/concept-alignment.md §3.4).
 */
export default function LiveTradePage() {
  return <TradeModule env="live" />;
}
