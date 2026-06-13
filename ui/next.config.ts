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
};

export default nextConfig;
