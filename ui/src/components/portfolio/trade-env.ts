import type { TradeAccountInfo } from "@/lib/api/types";

/**
 * The two Portfolio environments. `paper` is the broker PAPER book (broker env
 * `paper`); `live` is the REAL-money book (broker env `real`). This is the
 * ENVIRONMENT axis of the 2D session model (docs/concept-alignment.md §1.3,
 * C6) — distinct from the EXECUTION axis (`exec_policy`).
 *
 * Paper and Live are now ONE unified `/trade` module: there is no per-page env
 * split anymore. The active env is DERIVED from the SELECTED account (its
 * server-derived `kind`, falling back to `env`), and that drives the LIVE-red
 * treatment, the SIGNAL/AUTO arm-confirm, and the 4-factor/confirm gate.
 */
export type TradeEnv = "paper" | "live";

/**
 * The env an account belongs to, derived from its server-computed `kind`
 * ("paper"|"live"), falling back to its raw broker `env` for older API builds
 * (env=real => live, else paper). This is the SINGLE source the unified /trade
 * module reads to decide LIVE-red gating once an account is selected.
 */
export function accountEnv(a: TradeAccountInfo): TradeEnv {
  if (a.kind === "live" || a.kind === "paper") return a.kind;
  const e = (a.env || "").toLowerCase();
  return e === "real" ? "live" : "paper";
}
