/**
 * Idempotent seed runner: applies seed/seed.sql to the host-mapped postgres.
 *
 * Usage:
 *   npx tsx e2e/seed/seed.ts            # always (re-)apply the seed
 *   npx tsx e2e/seed/seed.ts --if-empty # apply only when bars_daily is empty
 *
 * Invoked by the Makefile `itest-full` target ("seed-if-empty") and runnable
 * by hand. The SQL itself is fully idempotent; --if-empty just skips the work
 * when real data is already present so a populated stack is left untouched.
 */

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { withDb, marketDataIsEmpty } from "../lib/db";

// Resolve this module's own directory in a way that works regardless of how
// the runner (tsx / ts-node / node) interprets the file's module format.
// Under ESM `__dirname` is undefined (the original bug that left the seed
// un-applied, so the CLEAN/GAPPY heatmap specs silently skipped); under CJS
// `import.meta.url` is absent. Prefer whichever the runtime provides.
function moduleDir(): string {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const meta = (import.meta as any) ?? undefined;
  if (meta && typeof meta.url === "string") {
    return dirname(fileURLToPath(meta.url));
  }
  // CommonJS fallback.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return (globalThis as any).__dirname as string;
}

async function main(): Promise<void> {
  const ifEmpty = process.argv.includes("--if-empty");
  const sql = readFileSync(join(moduleDir(), "seed.sql"), "utf8");

  await withDb(async (c) => {
    if (ifEmpty && !(await marketDataIsEmpty(c))) {
      // eslint-disable-next-line no-console
      console.log("[seed] market data already present; --if-empty -> skip");
      return;
    }
    await c.query(sql);
    // eslint-disable-next-line no-console
    console.log("[seed] applied e2e seed (idempotent)");
  });
}

main().catch((err) => {
  // eslint-disable-next-line no-console
  console.error("[seed] failed:", err);
  process.exit(1);
});
