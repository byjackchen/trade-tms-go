import { defineConfig, devices } from "@playwright/test";
import { UI_BASE_URL, STACK_READY_TIMEOUT_MS } from "./lib/env";

/**
 * Permanent Playwright configuration for the TMS control-plane e2e suite.
 *
 * The suite runs against an already-up compose stack (the gate runs
 * `compose up --wait`); it does NOT start the stack itself — see `globalSetup`,
 * which blocks until the API + UI are application-ready.
 *
 * Reports land in e2e/report/ (HTML) so the gate can archive a single artifact
 * directory, plus a `line` reporter for live console output.
 */
export default defineConfig({
  testDir: "./tests",
  // The stack is shared mutable state (one active refresh at a time, a global
  // jobs table); run serially so the refresh/cancel specs don't race each other.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 1,
  // Generous per-test timeout: refresh/cancel flows wait on a real worker.
  timeout: 90_000,
  expect: { timeout: 15_000 },

  globalSetup: require.resolve("./fixtures/global-setup"),

  reporter: [
    ["line"],
    ["html", { outputFolder: "report/html", open: "never" }],
    ["json", { outputFile: "report/results.json" }],
  ],

  use: {
    baseURL: UI_BASE_URL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "off",
    actionTimeout: 15_000,
    navigationTimeout: STACK_READY_TIMEOUT_MS,
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],

  outputDir: "test-results",
});
