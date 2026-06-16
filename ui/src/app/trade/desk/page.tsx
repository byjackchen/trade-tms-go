"use client";

import { Suspense } from "react";
import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/trade/live-indicator";
import { TradeTabs } from "@/components/trade/trade-tabs";
import {
  AccountSelector,
  useSelectedAccount,
} from "@/components/trade/account-selector";
import { ManualDesk } from "@/components/trade/desk/manual-desk";

/**
 * The MANUAL trading desk route (`/trade/desk`).
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
  // The account selector + desk reads behind a Suspense boundary because they
  // read the `?account=` query (useSearchParams), which Next requires be
  // suspense-wrapped so prerender can fall back cleanly.
  return (
    <Suspense fallback={<DeskBody accountId={undefined} selector={null} />}>
      <DeskInner />
    </Suspense>
  );
}

function DeskInner() {
  const { accountId } = useSelectedAccount();
  return <DeskBody accountId={accountId} selector={<AccountSelector />} />;
}

function DeskBody({
  accountId,
  selector,
}: {
  accountId: string | undefined;
  selector: React.ReactNode;
}) {
  return (
    <>
      <PageHeader
        title="Manual trading desk"
        subtitle="Place / cancel orders and close positions by hand — paper or live, server-gated, fully audited."
        data-testid="trade-header"
        actions={
          <div className="flex items-center gap-3">
            {selector}
            <LiveIndicator />
          </div>
        }
      />
      <TradeTabs />
      <ManualDesk accountId={accountId} />
    </>
  );
}
