# TMS UI — multi-stage build producing a self-contained Next.js standalone
# server. Final image is non-root and ships only the standalone bundle + static
# assets (no full node_modules). Host port 13000 maps to the container's 3000.

# ---- deps: install production + build dependencies against the lockfile ----
FROM node:25-slim AS deps
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
# node:25-slim ships no corepack; install the pinned pnpm globally instead.
RUN npm install -g pnpm@9.15.0
# Copy only manifests first for a cached dependency layer.
COPY package.json pnpm-lock.yaml* ./
# Use the lockfile when present (frozen); otherwise resolve fresh.
RUN if [ -f pnpm-lock.yaml ]; then pnpm install --frozen-lockfile; \
    else pnpm install --no-frozen-lockfile; fi

# ---- builder: compile the Next.js app to a standalone bundle ----
FROM node:25-slim AS builder
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm install -g pnpm@9.15.0
COPY --from=deps /app/node_modules ./node_modules
COPY . .
# `output: standalone` emits .next/standalone/server.js + minimal node_modules.
RUN pnpm run build

# ---- runner: minimal non-root runtime ----
FROM node:25-slim AS runner
WORKDIR /app
ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    PORT=3000 \
    HOSTNAME=0.0.0.0

# Create an unprivileged user to run the server.
RUN groupadd --system --gid 1001 nodejs \
    && useradd --system --uid 1001 --gid nodejs nextjs

# The standalone bundle includes the trimmed node_modules and server.js.
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static
COPY --from=builder --chown=nextjs:nodejs /app/public ./public

USER nextjs
EXPOSE 3000

# Container healthcheck hits the UI's own liveness route (always 200 when up).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:'+(process.env.PORT||3000)+'/api/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

CMD ["node", "server.js"]
