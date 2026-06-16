import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/live/live-indicator";
import { LiveTabs } from "@/components/live/live-tabs";
import { ManualDesk } from "@/components/live/trade/manual-desk";

/**
 * The MANUAL trading desk route (`/live/desk`).
 *
 * The desk lets the operator place / cancel orders and close positions BY HAND
 * against a paper or live account, in ANY strategy mode (in signal mode the
 * operator IS the executor; in paper/live it is an override alongside the auto
 * book). Manual orders are attributed to the MANUAL book.
 *
 * SAFETY is paramount and surfaced loudly: a distinct SIGNAL / PAPER / LIVE-REAL
 * banner; a per-order confirm token (paper password / live phrase); a guarded
 * LIVE-arm switch as the only path to real-money mode; and the server as the
 * authoritative gate. The bearer token stays server-side (every call goes through
 * the proxy).
 */
export default function ManualDeskPage() {
  return (
    <>
      <PageHeader
        title="Manual trading desk"
        subtitle="Place / cancel orders and close positions by hand — paper or live, server-gated, fully audited."
        data-testid="trade-header"
        actions={<LiveIndicator />}
      />
      <LiveTabs />
      <ManualDesk />
    </>
  );
}
