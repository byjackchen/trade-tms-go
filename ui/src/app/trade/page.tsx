"use client";

import { TradeModule } from "@/components/portfolio/trade-module";

/**
 * Trade (#4) — the UNIFIED former Paper + Live surface. ONE `<TradeModule/>` with
 * NO fixed env: the bound account is chosen by the top-right account selector
 * (which lists ALL accounts, each badged paper|live). The selected account's env
 * drives the LIVE-red treatment + SIGNAL/AUTO arm-confirm + the 4-factor/confirm
 * gate (docs/concept-alignment.md §3.4).
 */
export default function TradePage() {
  return <TradeModule />;
}
