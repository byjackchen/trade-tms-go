/**
 * (4) Trade cockpit — the ACCOUNT SELECTOR lists the registry and filters the book.
 *
 * The live→trade refactor made accounts first-class (migration 000014_accounts):
 * the cockpit/desk grew an account selector whose dropdown lists the registered
 * trading accounts from GET /api/v1/trade/accounts. Selecting an account writes
 * `?account=<id>` to the URL (shareable + sticky across the cockpit/desk tabs),
 * which the positions panel / blotter / account panel read back as their
 * `account_id` filter; "All accounts" clears the filter (and the query param).
 *
 * This spec proves the two halves of that contract:
 *   1. the dropdown's options MATCH the registry GET /api/v1/trade/accounts
 *      returns (which itself agrees with the `tms.accounts` DB rows) — no
 *      fabricated accounts, plus the sentinel "All accounts" entry; and
 *   2. selecting a concrete account sets `?account=<id>` AND threads that id into
 *      the positions read as `account_id=<id>` (so the book is per-account
 *      filtered), and re-selecting "All accounts" clears both.
 *
 * Self-skips cleanly: coming-soon placeholder, no trade reader (accounts 503),
 * or an empty registry — exactly like the sibling cockpit specs, so the gate
 * stays green until the account dimension is wired end-to-end.
 */

import { test, expect } from "../fixtures/test";
import { getAuthed } from "../lib/api";
import { withDb, tradingAccounts, type AccountTruth } from "../lib/db";
import { liveUiReady, liveReaderAvailable } from "../lib/live";

/** The account-registry shape GET /api/v1/trade/accounts returns. */
type AccountsResponse = {
  accounts: Array<{
    id: string;
    venue: string;
    env: string;
    broker_acc_id: number;
    label: string;
  }>;
};

test.describe("trade cockpit — account selector", () => {
  test("the dropdown lists the accounts from GET /api/v1/trade/accounts", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Trade cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(
        true,
        "API started without a trade reader (trade endpoints 503).",
      );
      return;
    }

    // Contract source: the registry endpoint the selector renders.
    const res = await getAuthed("trade/accounts");
    if (res.status === 503) {
      test.skip(true, "trade reader has no account registry (accounts 503).");
      return;
    }
    expect(
      res.status,
      "GET /api/v1/trade/accounts is 200 when a reader is present",
    ).toBe(200);
    const api = res.body as AccountsResponse;
    const apiIds = (api.accounts ?? []).map((a) => a.id).sort();
    if (apiIds.length === 0) {
      test.skip(true, "account registry empty — nothing to select.");
      return;
    }

    // Ground truth: the API must agree with the tms.accounts registry rows.
    const truth: AccountTruth[] = await withDb((c) => tradingAccounts(c));
    const truthIds = truth.map((a) => a.id).sort();
    expect(
      apiIds,
      "GET /api/v1/trade/accounts mirrors the tms.accounts registry",
    ).toEqual(truthIds);

    // The selector mounts and is enabled (a non-empty registry).
    await expect(page.getByTestId("live-page")).toBeVisible();
    const selector = page.getByTestId("account-selector");
    await expect(selector).toBeVisible({ timeout: 15_000 });
    const input = page.getByTestId("account-selector-input");
    await expect(input).toBeEnabled();

    // The rendered <option>s == the sentinel "All accounts" + one per API account
    // (text-independent, by value): no fabricated account, none missing.
    const optionValues = await input
      .locator("option")
      .evaluateAll((opts) => (opts as HTMLOptionElement[]).map((o) => o.value));
    expect(optionValues, "an 'All accounts' sentinel option exists").toContain(
      "all",
    );
    const renderedIds = optionValues.filter((v) => v !== "all").sort();
    expect(renderedIds, "dropdown options match the API account ids").toEqual(
      apiIds,
    );
  });

  test("selecting an account sets ?account= and filters the positions read", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Trade cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(
        true,
        "API started without a trade reader (trade endpoints 503).",
      );
      return;
    }

    const res = await getAuthed("trade/accounts");
    if (res.status === 503) {
      test.skip(true, "trade reader has no account registry (accounts 503).");
      return;
    }
    const api = res.body as AccountsResponse;
    const accounts = api.accounts ?? [];
    if (accounts.length === 0) {
      test.skip(true, "account registry empty — nothing to select.");
      return;
    }
    const target = accounts[0].id;

    await expect(page.getByTestId("live-page")).toBeVisible();
    const input = page.getByTestId("account-selector-input");
    await expect(input).toBeEnabled({ timeout: 15_000 });

    // Selecting a concrete account threads its id into the per-account positions
    // read (the UI proxies GET /api/v1/trade/positions?account_id=<id>). Arm the
    // request wait BEFORE selecting so we capture the refetch it triggers.
    const positionsReq = page.waitForRequest(
      (req) =>
        req.url().includes("/trade/positions") &&
        req.url().includes(`account_id=${encodeURIComponent(target)}`),
      { timeout: 15_000 },
    );
    await input.selectOption(target);

    // 1) the URL carries the selection (shareable + sticky).
    await expect
      .poll(() => new URL(page.url()).searchParams.get("account"), {
        timeout: 10_000,
      })
      .toBe(target);

    // 2) the positions panel refetched WITH the account_id filter.
    const req = await positionsReq;
    expect(
      req.url(),
      "positions read is filtered by the selected account_id",
    ).toContain(`account_id=${encodeURIComponent(target)}`);

    // The selector's visible value reflects the active filter (not silently "all").
    await expect(input).toHaveValue(target);

    // Selecting "All accounts" clears both the filter and the query param: the
    // positions read goes back to the unfiltered (no account_id) form.
    const unfilteredReq = page.waitForRequest(
      (r) =>
        r.url().includes("/trade/positions") &&
        !r.url().includes("account_id="),
      { timeout: 15_000 },
    );
    await input.selectOption("all");
    await expect
      .poll(() => new URL(page.url()).searchParams.get("account"), {
        timeout: 10_000,
      })
      .toBeNull();
    const cleared = await unfilteredReq;
    expect(
      cleared.url(),
      "clearing to All accounts drops the account_id filter",
    ).not.toContain("account_id=");
  });
});
