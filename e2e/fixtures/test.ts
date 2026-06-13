/**
 * Shared test fixtures for the TMS e2e suite.
 *
 * Extends Playwright's base `test` with:
 *   - `consoleErrors`: a live array of *severe* browser console messages and
 *     uncaught page errors collected over the test. Specs can assert it is
 *     empty (the "zero severe console errors" requirement) or inspect it.
 *   - `apiToken`: the bearer token (for tests that hit the API directly).
 *
 * Severity policy: we count `console.error` and uncaught exceptions /
 * unhandled rejections. We deliberately ignore:
 *   - non-error console levels (log/info/debug/warning),
 *   - network 4xx/5xx surfaced as console errors by the browser (the app
 *     renders these as UI error states; they are asserted by dedicated specs,
 *     not by the console gate),
 *   - a tiny allowlist of framework noise (see IGNORE_PATTERNS) that is not an
 *     application defect.
 * Every allowlist entry is documented; keep the list minimal.
 */

import { test as base, expect, type ConsoleMessage } from "@playwright/test";
import { API_TOKEN } from "../lib/env";

/**
 * Substrings of console-error text that are framework/runtime noise rather than
 * application defects. Keep this list short and justified.
 */
const IGNORE_PATTERNS: RegExp[] = [
  // Browsers log failed fetches (e.g. the SSE bridge reconnecting, or an API
  // 4xx the app renders as a UI error state) at error level. Those are covered
  // by dedicated specs; they are not React/JS defects.
  /Failed to load resource/i,
  /net::ERR_/i,
  /the server responded with a status of/i,
  // EventSource/SSE reconnect chatter from the live job stream bridge.
  /EventSource/i,
];

function isSevere(text: string): boolean {
  return !IGNORE_PATTERNS.some((re) => re.test(text));
}

export type SevereEvent = {
  kind: "console.error" | "pageerror";
  text: string;
  location?: string;
};

type Fixtures = {
  consoleErrors: SevereEvent[];
  apiToken: string;
};

export const test = base.extend<Fixtures>({
  apiToken: async ({}, use) => {
    await use(API_TOKEN);
  },

  consoleErrors: async ({ page }, use) => {
    const errors: SevereEvent[] = [];

    const onConsole = (msg: ConsoleMessage): void => {
      if (msg.type() !== "error") return;
      const text = msg.text();
      if (!isSevere(text)) return;
      const loc = msg.location();
      errors.push({
        kind: "console.error",
        text,
        location: loc.url ? `${loc.url}:${loc.lineNumber}` : undefined,
      });
    };

    const onPageError = (err: Error): void => {
      const text = `${err.name}: ${err.message}`;
      if (!isSevere(text)) return;
      errors.push({ kind: "pageerror", text });
    };

    page.on("console", onConsole);
    page.on("pageerror", onPageError);

    await use(errors);

    page.off("console", onConsole);
    page.off("pageerror", onPageError);
  },
});

export { expect };
