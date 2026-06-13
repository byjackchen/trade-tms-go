"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "next-themes";
import { useState } from "react";
import { ApiError } from "@/lib/api/client";

export function Providers({ children }: { children: React.ReactNode }) {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            // Modest polling for non-WS REST endpoints; coverage/sync-runs are
            // cheap and freshness matters. WS/SSE drives job progress live.
            refetchInterval: 15000,
            staleTime: 5000,
            retry: (failureCount, error) => {
              // Don't retry deterministic client errors (validation / 404 /
              // unauthorized); they won't succeed on retry.
              if (error instanceof ApiError && error.status < 500) return false;
              return failureCount < 2;
            },
          },
        },
      }),
  );
  return (
    <QueryClientProvider client={client}>
      <ThemeProvider attribute="class" defaultTheme="dark" enableSystem>
        {children}
      </ThemeProvider>
    </QueryClientProvider>
  );
}
