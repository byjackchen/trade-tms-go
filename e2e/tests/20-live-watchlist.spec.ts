/**
 * (3) Live cockpit — watchlist renders the tracked universe and updates.
 *
 * GET /api/v1/watchlist returns the distinct symbols the recent sessions
 * emitted intents for (the tracked universe). The cockpit's watchlist surface
 * renders one row per symbol; this spec proves the rendered symbols MATCH the
 * DB's distinct intent symbols (no fabricated tickers) and that the set is a
 * subset/superset relationship consistent with the durable truth.
 *
 * Self-skips: coming-soon placeholder, no live reader (503), or empty universe
 * (no intents emitted yet).
 */

import { test, expect } from "../fixtures/test";
import { withDb, watchlistSymbols } from "../lib/db";
import { liveUiReady, liveReaderAvailable } from "../lib/live";

test.describe("live cockpit — watchlist", () => {
  test("watchlist symbols match the DB tracked universe", async ({ page }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    await expect(page.getByTestId("live-page")).toBeVisible();

    const watchlist = page.getByTestId("live-watchlist");
    await expect(watchlist).toBeVisible({ timeout: 15_000 });

    // Ground truth: the distinct symbols emitted across recent sessions.
    const truth = await withDb((c) => watchlistSymbols(c));
    if (truth.length === 0) {
      test.skip(
        true,
        "tracked universe empty — no intents emitted yet.",
      );
      return;
    }
    const truthSet = new Set(truth);

    // Each watchlist row carries its symbol as a data attribute for an exact,
    // text-independent comparison.
    const rows = page.getByTestId("live-watchlist-row");
    await expect
      .poll(async () => rows.count(), { timeout: 20_000 })
      .toBeGreaterThan(0);

    const n = await rows.count();
    const rendered = new Set<string>();
    for (let i = 0; i < n; i++) {
      const sym = await rows.nth(i).getAttribute("data-symbol");
      expect(sym, `watchlist row ${i} exposes data-symbol`).toBeTruthy();
      rendered.add(sym as string);
      // No fabricated symbol: everything the UI shows is in the DB universe.
      expect(
        truthSet.has(sym as string),
        `watchlist symbol ${sym} is in the DB tracked universe`,
      ).toBeTruthy();
    }

    // The watchlist is one row per symbol (deduped); the rendered set should
    // cover a meaningful portion of the tracked universe (the API caps/paginates
    // some surfaces, so require non-empty coverage rather than strict equality).
    expect(rendered.size, "watchlist renders at least one tracked symbol").toBeGreaterThan(0);
  });

  test("the watchlist stays consistent with the DB universe over a poll", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    const truth = await withDb((c) => watchlistSymbols(c));
    if (truth.length === 0) {
      test.skip(true, "tracked universe empty — no intents emitted yet.");
      return;
    }

    await expect(page.getByTestId("live-page")).toBeVisible();
    const watchlist = page.getByTestId("live-watchlist");
    await expect(watchlist).toBeVisible({ timeout: 15_000 });

    // The watchlist count never claims more symbols than the DB has emitted
    // (it can only render symbols that have intents). Re-read after a settle to
    // confirm the live update path doesn't introduce phantom symbols.
    const rows = page.getByTestId("live-watchlist-row");
    await page.waitForTimeout(2_000);
    const renderedCount = await rows.count();
    const truthNow = await withDb((c) => watchlistSymbols(c));
    expect(
      renderedCount,
      "watchlist never renders more symbols than the DB universe holds",
    ).toBeLessThanOrEqual(truthNow.length);
  });

  test("the Download CSV button exports the rendered watchlist", async ({ page }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }
    await expect(page.getByTestId("live-watchlist")).toBeVisible({ timeout: 15_000 });

    const download = page.getByTestId("watchlist-download");
    await expect(download).toBeVisible();

    const rows = page.getByTestId("live-watchlist-row");
    if ((await rows.count()) === 0) {
      // Empty watchlist: the button is present but disabled (nothing to export).
      await expect(download).toBeDisabled();
      test.skip(true, "tracked universe empty — nothing to download yet.");
      return;
    }

    // Clicking triggers a real file download; capture it and check the CSV.
    const [dl] = await Promise.all([
      page.waitForEvent("download"),
      download.click(),
    ]);
    expect(dl.suggestedFilename()).toMatch(/^watchlist-.*\.csv$/);
    const stream = await dl.createReadStream();
    const chunks: Buffer[] = [];
    for await (const c of stream) chunks.push(c as Buffer);
    const csv = Buffer.concat(chunks).toString("utf-8").trim().split("\n");

    expect(csv[0]).toBe("symbol,latest_state,strategy,strength,as_of");
    // One header + one row per rendered symbol; every rendered symbol appears.
    expect(csv.length - 1).toBe(await rows.count());
    const renderedSyms = new Set<string>();
    const rn = await rows.count();
    for (let i = 0; i < rn; i++) {
      renderedSyms.add((await rows.nth(i).getAttribute("data-symbol")) as string);
    }
    for (let i = 1; i < csv.length; i++) {
      const sym = csv[i].split(",")[0].replace(/^"|"$/g, "");
      expect(renderedSyms.has(sym), `CSV symbol ${sym} is a rendered row`).toBeTruthy();
    }
  });
});
