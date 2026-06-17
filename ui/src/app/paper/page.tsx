"use client";

import { TradeModule } from "@/components/portfolio/trade-module";

/**
 * Paper Trade (#4) — `<TradeModule env="paper"/>` bound to the SIMULATE book
 * (sim/simulate accounts). Identical to Live Trade (#5) apart from the bound env
 * and its relaxed gating (docs/concept-alignment.md §3.4).
 */
export default function PaperTradePage() {
  return <TradeModule env="paper" />;
}
