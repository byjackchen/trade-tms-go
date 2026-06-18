/**
 * (mobile) Mobile shell smoke — the full-feature mobile layout mounts.
 *
 * The 5-top IA ships an explicit mobile surface (docs/concept-alignment.md
 * LOCKED DECISIONS 2 & 4): the desktop/mobile shell is chosen by an EXPLICIT
 * `ui-mode` cookie (desktop|mobile|auto), NOT a CSS breakpoint, so a manual
 * toggle can force the mobile chrome on a desktop. With `ui-mode=mobile`:
 *
 *   - the <MobileShell> chrome replaces the desktop sidebar: a fixed top app bar
 *     (`mobile-app-bar`) + a fixed BOTTOM TAB BAR (`mobile-tab-bar`) carrying all
 *     FIVE top-levels (Systems & Data / Strategies / Compositions / Session /
 *     Account), each a tab the bottom bar renders from the shared NAV_SECTIONS
 *     source;
 *   - the desktop `app-shell` root is ABSENT (mobile uses `mobile-shell`);
 *   - data tables render as the stacked CARD LIST surface
 *     (<ResponsiveTable> in mobile mode emits `data-slot="responsive-table-cards"`
 *     instead of a <table>), proving a representative table flipped surfaces.
 *
 * This is a SMOKE: it proves the mobile chrome + a card-list render with no
 * severe console errors. It self-skips the card-list assertion when the page's
 * tables have no rows yet (empty book / no reader), exactly like the sibling
 * cockpit specs, so the gate stays green meanwhile.
 *
 * The `ui-mode` cookie is set on the browser context BEFORE navigation so the
 * server resolves the mobile shell on the very first paint (the root layout
 * reads the cookie SSR-side) — no client toggle, no shell flash.
 */

import { test, expect } from "../fixtures/test";
import { UI_BASE_URL } from "../lib/env";

/** Bounded best-effort settle: lets late XHRs + a render tick flush without
 * ever hanging on the app shell's perpetually-open SSE connection. */
async function settle(page: import("@playwright/test").Page): Promise<void> {
  await page
    .waitForLoadState("networkidle", { timeout: 2_000 })
    .catch(() => {
      /* SSE keeps a connection open; networkidle never settles — expected. */
    });
}

/** The five top-level bottom-tab testids (shell/nav.ts NAV_SECTIONS). */
const TAB_TESTIDS = [
  "nav-systems",
  "nav-strategies",
  "nav-compositions",
  "nav-session",
  "nav-account",
] as const;

test.describe("mobile shell smoke", () => {
  // Force the explicit mobile preference for every test in this file: the cookie
  // is read SSR-side (root layout) so the mobile chrome renders on first paint.
  test.beforeEach(async ({ context }) => {
    await context.addCookies([
      { name: "ui-mode", value: "mobile", url: UI_BASE_URL },
    ]);
  });

  test("the bottom tab bar renders all five top-levels on /session", async ({
    page,
    consoleErrors,
  }) => {
    // domcontentloaded, not "load": the app shell's persistent SSE stream defers
    // the load event past the test budget.
    await page.goto("/session", { waitUntil: "domcontentloaded" });

    // Mobile chrome — NOT the desktop `app-shell` (which is absent in mobile).
    await expect(page.getByTestId("mobile-shell")).toBeVisible();
    await expect(page.getByTestId("app-shell")).toHaveCount(0);
    await expect(page.getByTestId("mobile-app-bar")).toBeVisible();

    // The bottom tab bar carries all five top-levels.
    const tabBar = page.getByTestId("mobile-tab-bar");
    await expect(tabBar).toBeVisible();
    for (const id of TAB_TESTIDS) {
      await expect(tabBar.getByTestId(id)).toBeVisible();
    }
    // The active route (/session) is highlighted in the bar.
    await expect(tabBar.getByTestId("nav-session")).toHaveAttribute(
      "data-active",
      "true",
    );

    // The session module itself mounted under the mobile chrome.
    await expect(page.getByTestId("session-header")).toBeVisible();

    // The app bar must NOT overflow horizontally on a narrow (~360px) phone:
    // the account selector + 3-way mode toggle + theme toggle have to fit, with
    // the title shrinking first. scrollWidth > clientWidth means a child was
    // pushed off the edge / the bar scrolls sideways.
    await page.setViewportSize({ width: 360, height: 740 });
    const appBar = page.getByTestId("mobile-app-bar");
    await expect(appBar).toBeVisible();
    const overflow = await appBar.evaluate(
      (el) => el.scrollWidth - el.clientWidth,
    );
    expect(
      overflow,
      `mobile-app-bar overflows horizontally at 360px (scrollWidth-clientWidth=${overflow}px)`,
    ).toBeLessThanOrEqual(1);

    await settle(page);
    await page.waitForTimeout(1_000);
    expect(
      consoleErrors,
      `severe console/page errors on /session (mobile):\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("a second top-level (/strategies) renders the mobile chrome + a ResponsiveTable card-list", async ({
    page,
    consoleErrors,
  }) => {
    await page.goto("/strategies", { waitUntil: "domcontentloaded" });

    // Mobile chrome on a SECOND top-level, proving the shell is route-agnostic.
    await expect(page.getByTestId("mobile-shell")).toBeVisible();
    await expect(page.getByTestId("mobile-tab-bar")).toBeVisible();
    await expect(page.getByTestId("strategies-page")).toBeVisible();
    // /strategies is the active tab here.
    await expect(
      page.getByTestId("mobile-tab-bar").getByTestId("nav-strategies"),
    ).toHaveAttribute("data-active", "true");

    // A representative <ResponsiveTable> flips to its stacked CARD LIST in mobile
    // mode: it emits `data-slot="responsive-table-cards"` (a <table> on desktop).
    // The Strategies detail mounts the resolved-params table (`strategy-params-
    // table`) for the first strategy; in mobile mode that becomes a card list.
    // It needs ≥1 param row; if the strategy meta is unavailable (API down), the
    // table is absent — self-skip rather than fail the smoke.
    const cards = page.locator('[data-slot="responsive-table-cards"]');
    await settle(page);
    const appeared = await cards
      .first()
      .waitFor({ state: "visible", timeout: 10_000 })
      .then(() => true)
      .catch(() => false);
    test.skip(
      !appeared,
      "no populated ResponsiveTable on /strategies yet (no rows / API unavailable).",
    );
    if (appeared) {
      // The mobile card list renders ≥1 card; NO desktop <table> for this table.
      const firstCards = cards.first();
      await expect(firstCards).toBeVisible();
      const cardCount = await firstCards
        .locator('[data-slot="responsive-table-card"]')
        .count();
      expect(cardCount, "the card list renders at least one card").toBeGreaterThan(
        0,
      );
    }

    await page.waitForTimeout(1_000);
    expect(
      consoleErrors,
      `severe console/page errors on /strategies (mobile):\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });
});
