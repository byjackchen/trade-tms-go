import type { TradeAccountInfo } from "@/lib/api/types";

/**
 * The two Portfolio environments the `<TradeModule>` binds to. `paper` runs
 * against the SIMULATE book (broker env `sim`/`simulate`); `live` runs against
 * the REAL-money book (broker env `real`). This is the ENVIRONMENT axis of the
 * 2D session model (docs/concept-alignment.md §1.3, C6) — distinct from the
 * EXECUTION axis (`exec_policy`).
 */
export type TradeEnv = "paper" | "live";

/** True when a registry account's broker env belongs to the given Portfolio env. */
export function accountMatchesEnv(a: TradeAccountInfo, env: TradeEnv): boolean {
  const e = (a.env || "").toLowerCase();
  if (env === "live") return e === "real";
  // paper: the SIMULATE book — moomoo exposes it as `sim` or `simulate`.
  return e === "sim" || e === "simulate";
}

/** Filter the account registry to the accounts bound to a Portfolio env. */
export function accountsForEnv(
  accounts: TradeAccountInfo[],
  env: TradeEnv,
): TradeAccountInfo[] {
  return accounts.filter((a) => accountMatchesEnv(a, env));
}
