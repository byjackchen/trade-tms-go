"use client";

import { AlertTriangle, RotateCw } from "lucide-react";
import { Button } from "@/components/ui/button";

/** Route-level error boundary. Catches render/runtime errors in the segment. */
export default function Error({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <div
        className="flex max-w-md flex-col items-center gap-3 rounded-xl border border-destructive/30 bg-destructive/5 px-8 py-10 text-center"
        data-testid="route-error"
      >
        <AlertTriangle className="size-7 text-destructive" />
        <h2 className="text-base font-medium">Something went wrong</h2>
        <p className="text-sm text-muted-foreground">{error.message}</p>
        <Button onClick={reset} variant="outline" data-testid="route-error-retry">
          <RotateCw />
          Try again
        </Button>
      </div>
    </main>
  );
}
