"use client";

import { Suspense, useCallback } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { PageHeader } from "@/components/shell/page-header";
import {
  SystemsTabs,
  asSystemsTab,
  type SystemsTab,
} from "@/components/systems/systems-tabs";
import { SystemHealth } from "@/components/systems/health";
import { DataPanel } from "@/components/systems/data-panel";
import { JobQueue } from "@/components/systems/job-queue";
import { AuditLog } from "@/components/systems/audit-log";
import { StreamIndicator } from "@/components/systems/stream-indicator";

const SUBTITLE: Record<SystemsTab, string> = {
  health: "Component status, connections and control-plane metrics.",
  data: "Market-data coverage, freshness, gaps and sync.",
  jobs: "Background job queue — live ops.jobs table with cancel / retry.",
  audit: "Append-only audit trail of every control-plane action.",
};

/**
 * Systems & Data (top-level #1) — the operational + data layer of the control
 * plane, in one tabbed workspace driven by `?tab=` (default `health`):
 *
 *   - Health — the merged system-health view (component grid + connections +
 *     metrics) from GET /api/v1/system and the /healthz proxy.
 *   - Data   — coverage / freshness / gap heatmap / universe / sync runs.
 *   - Jobs   — the background job queue with the detail drawer.
 *   - Audit  — the append-only audit log.
 *
 * The retired /data and /ops routes 301-redirect here (see next.config.ts) onto
 * `?tab=data` and `?tab=jobs`. `useSearchParams` is read behind a Suspense
 * boundary because Next requires it for clean prerender fallback.
 */
export default function SystemsPage() {
  return (
    <Suspense fallback={<SystemsBody tab="health" onChange={() => {}} />}>
      <SystemsInner />
    </Suspense>
  );
}

function SystemsInner() {
  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();
  const tab = asSystemsTab(search.get("tab"));

  const onChange = useCallback(
    (next: SystemsTab) => {
      const params = new URLSearchParams(search.toString());
      // `health` is the default; keep the URL clean by dropping the param.
      if (next === "health") params.delete("tab");
      else params.set("tab", next);
      const qs = params.toString();
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    },
    [router, pathname, search],
  );

  return <SystemsBody tab={tab} onChange={onChange} />;
}

function SystemsBody({
  tab,
  onChange,
}: {
  tab: SystemsTab;
  onChange: (t: SystemsTab) => void;
}) {
  return (
    <>
      <PageHeader
        title="Systems & Data"
        subtitle={SUBTITLE[tab]}
        data-testid="systems-header"
        actions={tab === "data" ? <StreamIndicator /> : undefined}
      />
      <SystemsTabs active={tab} onChange={onChange} />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="systems-page"
        data-active-tab={tab}
      >
        {tab === "health" ? <SystemHealth /> : null}
        {tab === "data" ? <DataPanel /> : null}
        {tab === "jobs" ? <JobQueue /> : null}
        {tab === "audit" ? <AuditLog /> : null}
      </main>
    </>
  );
}
