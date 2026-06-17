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
  // Concept-alignment (docs/concept-alignment.md §3.4, C7) collapsed the IA into 4
  // pipeline sections. These permanent (301) redirects keep every old bookmark /
  // deep-link alive by mapping retired routes onto their new homes: Data + Ops fold
  // into Systems & Data (as tabs); Backtests folds into Compositions (a backtest's
  // object is always a Composition); Hyperopt folds into Strategies (single-strategy
  // tuning). Next preserves the query string across a redirect, so `?tab=` /
  // `?backtest=` / `?study=` hints survive the hop.
  //
  // Paper Trade + Live Trade are now ONE account-driven `/trade` module, so the
  // former /paper and /live pages redirect there — Next carries the `?account=`
  // binding across, so a deep-linked paper/live account lands selected. /trade
  // itself is now a REAL route (do NOT redirect it); only its legacy SUB-paths hop.
  async redirects() {
    return [
      { source: "/data", destination: "/systems?tab=data", permanent: true },
      { source: "/ops", destination: "/systems?tab=jobs", permanent: true },
      { source: "/backtests", destination: "/compositions", permanent: true },
      { source: "/backtests/:id", destination: "/compositions?backtest=:id", permanent: true },
      { source: "/hyperopt", destination: "/strategies", permanent: true },
      { source: "/hyperopt/:id", destination: "/strategies?study=:id", permanent: true },
      { source: "/paper", destination: "/trade", permanent: true },
      { source: "/live", destination: "/trade", permanent: true },
      { source: "/trade/desk", destination: "/trade?view=desk", permanent: true },
      { source: "/trade/watchlist", destination: "/strategies", permanent: true },
      { source: "/trade/strategies", destination: "/strategies", permanent: true },
      { source: "/trade/system", destination: "/systems", permanent: true },
    ];
  },
};

export default nextConfig;
