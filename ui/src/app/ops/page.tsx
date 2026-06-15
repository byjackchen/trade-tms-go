"use client";

import { useState } from "react";
import { PageHeader } from "@/components/shell/page-header";
import { OpsTabs, type OpsTab } from "@/components/ops/ops-tabs";
import { JobQueue } from "@/components/ops/job-queue";
import { AuditLog } from "@/components/ops/audit-log";
import { SystemHealth } from "@/components/ops/system-health";

/**
 * Ops workspace: the operational layer of the control plane.
 *
 *   - Job queue  — live ops.jobs table + detail drawer (cancel / retry).
 *   - Audit log  — append-only ops.audit_log stream.
 *   - System health — component status + metrics from GET /api/v1/system.
 *
 * Live job progress streams via the existing SSE/WS bridge (use-job-stream);
 * every panel has loading / empty / error states and stable data-testids.
 */
export default function OpsPage() {
  const [tab, setTab] = useState<OpsTab>("queue");

  return (
    <>
      <PageHeader
        title="Ops"
        subtitle="Job queue, audit trail and system health."
        data-testid="ops-header"
      />
      <OpsTabs active={tab} onChange={setTab} />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="ops-page"
        data-active-tab={tab}
      >
        {tab === "queue" ? <JobQueue /> : null}
        {tab === "audit" ? <AuditLog /> : null}
        {tab === "health" ? <SystemHealth /> : null}
      </main>
    </>
  );
}
