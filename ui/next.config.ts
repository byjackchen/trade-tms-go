import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Emit a self-contained server bundle at .next/standalone so the prod Docker
  // image can ship `node server.js` without copying node_modules.
  output: "standalone",
  // The UI proxies the TMS API server-side; the browser never talks to the API
  // directly, so no rewrites/CORS are needed here. Keep React strict mode on.
  reactStrictMode: true,
  // Silence the "multiple lockfiles" workspace-root inference warning when the
  // worktree sits inside a larger monorepo-like tree.
  outputFileTracingRoot: __dirname,
  // Concept-alignment (docs/concept-alignment.md §3.4, C7) collapsed the old 6-top
  // IA into 5 pipeline sections. These permanent (301) redirects keep every old
  // bookmark / deep-link alive by mapping the retired routes onto their new homes:
  // Data + Ops fold into Systems & Data (as tabs); Backtests folds into Models (a
  // backtest's object is always a Model); Hyperopt folds into Strategies
  // (single-strategy tuning); the /trade/* cockpit splits into Paper (sim) + the
  // strategies / systems sub-views. Next preserves the query string across a
  // redirect, so the `?tab=` / `?backtest=` / `?study=` hints survive the hop.
  //
  // NOTE: /live is now a REAL route (Live Trade), so the former /live -> /trade
  // alias is GONE — /trade itself now redirects forward to /paper.
  async redirects() {
    return [
      { source: "/data", destination: "/systems?tab=data", permanent: true },
      { source: "/ops", destination: "/systems?tab=jobs", permanent: true },
      { source: "/backtests", destination: "/models", permanent: true },
      { source: "/backtests/:id", destination: "/models?backtest=:id", permanent: true },
      { source: "/hyperopt", destination: "/strategies", permanent: true },
      { source: "/hyperopt/:id", destination: "/strategies?study=:id", permanent: true },
      { source: "/trade", destination: "/paper", permanent: true },
      { source: "/trade/desk", destination: "/paper", permanent: true },
      { source: "/trade/watchlist", destination: "/strategies", permanent: true },
      { source: "/trade/strategies", destination: "/strategies", permanent: true },
      { source: "/trade/system", destination: "/systems", permanent: true },
    ];
  },
};

export default nextConfig;
