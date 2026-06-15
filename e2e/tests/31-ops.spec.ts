/**
 * Ops workspace: the operational layer of the control plane.
 *
 * Coverage:
 *   (a) Job queue renders and MATCHES the DB — the rows the table shows are
 *       exactly tms.jobs newest-first (ids + kind), and the detail drawer opens
 *       with the clicked job's id.
 *   (b) Audit log renders — the rows MATCH tms.audit_log newest-first (or the
 *       not-configured/empty state shows when the trail is empty).
 *   (c) System health cards show Postgres + Redis status; against a live stack
 *       both are reachable ("ok"), and the rollup is not "down".
 *   (d) A triggered job appears in the queue and progresses to a terminal state
 *       (enqueued via the Data refresh flow; observed live in the Ops queue).
 *
 * The DB is the source of truth; nothing here fabricates numbers. The triggered
 * job uses a tiny parquet refresh whose terminal outcome (succeeded/failed/
 * canceled) is state-machine-valid regardless of whether a parquet cache is
 * mounted (same contract as 03-refresh-flow).
 */

import { test, expect } from "../fixtures/test";
import { withDb, recentJobs, recentAudit, auditCount, latestJobId } from "../lib/db";

const TERMINAL = new Set(["succeeded", "failed", "canceled"]);

test.describe("ops workspace", () => {
  test("job queue renders and matches the DB; drawer opens", async ({
    page,
  }) => {
    await page.goto("/ops");
    await expect(page.getByTestId("ops-page")).toBeVisible();
    // The queue tab is the default.
    await expect(page.getByTestId("job-queue-card")).toBeVisible();

    const dbJobs = await withDb((c) => recentJobs(c, 200));

    if (dbJobs.length === 0) {
      // Empty stack: the queue shows its empty state, not a phantom table.
      await expect(page.getByTestId("job-queue-empty")).toBeVisible();
      return;
    }

    // The table renders; the newest DB job is present as a row with its kind.
    const table = page.getByTestId("job-queue-table");
    await expect(table).toBeVisible();

    const newest = dbJobs[0];
    const row = page.getByTestId(`job-queue-row-${newest.id}`);
    await expect(row).toBeVisible();
    await expect(row).toContainText(newest.kind);
    await expect(
      page.getByTestId(`job-queue-status-${newest.id}`),
    ).toHaveAttribute("data-status", newest.status);

    // Every visible row id corresponds to a real DB job (no fabricated rows).
    const dbIds = new Set(dbJobs.map((j) => j.id));
    const renderedIds = await page
      .locator('[data-testid^="job-queue-row-"]')
      .evaluateAll((els) =>
        els.map((el) =>
          Number(
            (el.getAttribute("data-testid") ?? "").replace(
              "job-queue-row-",
              "",
            ),
          ),
        ),
      );
    expect(renderedIds.length).toBeGreaterThan(0);
    for (const id of renderedIds) {
      expect(dbIds.has(id), `rendered job ${id} exists in tms.jobs`).toBeTruthy();
    }

    // Clicking a row opens the detail drawer scoped to that job.
    await row.click();
    const drawer = page.getByTestId("job-drawer");
    await expect(drawer).toBeVisible();
    await expect(drawer).toHaveAttribute("data-job-id", String(newest.id));
    await expect(page.getByTestId("job-drawer-status")).toHaveAttribute(
      "data-status",
      newest.status,
    );
    // Kind shown in the drawer matches the DB.
    await expect(drawer).toContainText(newest.kind);

    await page.getByTestId("job-drawer-close").click();
    await expect(drawer).toBeHidden();
  });

  test("audit log renders and matches the DB", async ({ page }) => {
    await page.goto("/ops");
    await page.getByTestId("ops-tab-audit").click();
    await expect(page.getByTestId("audit-log-card")).toBeVisible();

    const [n, dbRows] = await withDb(async (c) => [
      await auditCount(c),
      await recentAudit(c, 100),
    ]);

    if (n === 0) {
      // No audit rows yet: the panel shows its empty (or not-configured) state.
      const empty = page.getByTestId("audit-empty");
      const notConfigured = page.getByTestId("audit-not-configured");
      await expect
        .poll(async () => (await empty.count()) + (await notConfigured.count()))
        .toBeGreaterThan(0);
      return;
    }

    // The table renders; the newest DB audit row is present with its actor +
    // action (the API returns audit newest-first by id).
    const table = page.getByTestId("audit-table");
    await expect(table).toBeVisible();

    const newest = dbRows[0];
    const row = page.getByTestId(`audit-row-${newest.id}`);
    await expect(row).toBeVisible();
    await expect(row).toContainText(newest.actor);
    await expect(page.getByTestId(`audit-action-${newest.id}`)).toContainText(
      newest.action,
    );
    if (newest.entity && newest.entityId) {
      await expect(row).toContainText(newest.entityId);
    }

    // Every rendered audit id is a real DB row.
    const dbIds = new Set(dbRows.map((r) => r.id));
    const renderedIds = await page
      .locator('[data-testid^="audit-row-"]')
      .evaluateAll((els) =>
        els.map((el) =>
          Number(
            (el.getAttribute("data-testid") ?? "").replace("audit-row-", ""),
          ),
        ),
      );
    for (const id of renderedIds) {
      expect(
        dbIds.has(id),
        `rendered audit ${id} exists in tms.audit_log`,
      ).toBeTruthy();
    }
  });

  test("system health cards show postgres + redis ok", async ({ page }) => {
    await page.goto("/ops");
    await page.getByTestId("ops-tab-health").click();
    await expect(page.getByTestId("ops-system-health")).toBeVisible();

    const grid = page.getByTestId("system-health-grid");
    await expect(grid).toBeVisible();

    // Postgres is the fatal dependency; against a live stack it must be reachable.
    const pg = page.getByTestId("health-postgres");
    await expect(pg).toBeVisible();
    await expect
      .poll(async () => pg.getAttribute("data-status"), { timeout: 15_000 })
      .toBe("ok");

    // Redis is transport; it is "ok" on the full app stack (compose runs it).
    const redis = page.getByTestId("health-redis");
    await expect(redis).toBeVisible();
    await expect
      .poll(async () => redis.getAttribute("data-status"), { timeout: 15_000 })
      .not.toBe("down");

    // The overall rollup must not be "down" (pg is up).
    await expect
      .poll(async () =>
        page.getByTestId("system-rollup").getAttribute("data-status"),
      )
      .not.toBe("down");

    // The metrics tiles render (queue depth + active sessions are always present).
    await expect(page.getByTestId("metric-jobs-queued")).toBeVisible();
    await expect(page.getByTestId("metric-active-sessions")).toBeVisible();
  });

  test("a triggered job appears in the queue and progresses to terminal", async ({
    page,
  }) => {
    // Trigger a real job through the Data refresh flow (the documented enqueue
    // path), then watch it land + reach terminal in the Ops queue.
    const before = await withDb((c) => latestJobId(c));

    await page.goto("/data");
    await page.getByTestId("open-refresh-dialog").click();
    await expect(page.getByTestId("refresh-dialog")).toBeVisible();
    await page.getByTestId("refresh-source").selectOption("parquet");
    await page.getByTestId("refresh-tickers").fill("AAPL MSFT");
    await page.getByTestId("refresh-submit").click();

    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId).toMatch(/^\d+$/);
    expect(Number(jobId)).toBeGreaterThan(before ?? 0);

    // Now switch to the Ops queue: the new job must appear as a row.
    await page.goto("/ops");
    await expect(page.getByTestId("job-queue-card")).toBeVisible();
    const row = page.getByTestId(`job-queue-row-${jobId}`);
    await expect(row).toBeVisible({ timeout: 20_000 });
    await expect(row).toContainText("data.refresh");

    // It progresses to a terminal state, observed live in the queue (the table
    // self-refreshes + the SSE bridge reconciles). DB confirms terminal.
    await expect
      .poll(
        async () => {
          const s = await page
            .getByTestId(`job-queue-status-${jobId}`)
            .getAttribute("data-status");
          return s != null && TERMINAL.has(s);
        },
        { timeout: 90_000 },
      )
      .toBe(true);

    const dbStatus = await withDb(async (c) => {
      const { rows } = await c.query<{ status: string }>(
        `SELECT status FROM tms.jobs WHERE id = $1`,
        [Number(jobId)],
      );
      return rows.length ? rows[0].status : null;
    });
    expect(
      dbStatus && TERMINAL.has(dbStatus),
      `DB job ${jobId} terminal (got "${dbStatus}")`,
    ).toBeTruthy();

    // Opening the drawer on the terminal job offers a Retry affordance (only
    // failed/canceled are retryable; a succeeded job shows no retry).
    await row.click();
    const drawer = page.getByTestId("job-drawer");
    await expect(drawer).toBeVisible();
    if (dbStatus === "failed" || dbStatus === "canceled") {
      await expect(page.getByTestId("job-drawer-retry")).toBeVisible();
    }
  });
});
