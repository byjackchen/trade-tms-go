import * as React from "react";
import { AlertTriangle, Inbox, RotateCw } from "lucide-react";
import { ApiError } from "@/lib/api/client";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";

/** Inline error panel for a failed data fetch. Surfaces the API error code. */
export function ErrorState({
  error,
  onRetry,
  "data-testid": testId = "error-state",
}: {
  error: unknown;
  onRetry?: () => void;
  "data-testid"?: string;
}) {
  const code = error instanceof ApiError ? error.code : "error";
  const message =
    error instanceof Error ? error.message : "Something went wrong.";
  return (
    <Alert variant="destructive" data-testid={testId}>
      <AlertTriangle />
      <AlertTitle data-testid={`${testId}-code`}>
        Failed to load ({code})
      </AlertTitle>
      <AlertDescription>
        <p data-testid={`${testId}-message`}>{message}</p>
        {onRetry ? (
          <Button
            variant="outline"
            size="sm"
            className="mt-2"
            onClick={onRetry}
            data-testid={`${testId}-retry`}
          >
            <RotateCw />
            Retry
          </Button>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

/** Empty-state placeholder (no rows / no data yet). */
export function EmptyState({
  title,
  hint,
  "data-testid": testId = "empty-state",
}: {
  title: string;
  hint?: string;
  "data-testid"?: string;
}) {
  return (
    <div
      data-testid={testId}
      className="flex flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border px-6 py-10 text-center"
    >
      <Inbox className="size-6 text-muted-foreground" />
      <p className="text-sm font-medium">{title}</p>
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

/** Generic skeleton block for a loading region. */
export function LoadingRows({
  rows = 4,
  "data-testid": testId = "loading-state",
}: {
  rows?: number;
  "data-testid"?: string;
}) {
  return (
    <div className="space-y-2" data-testid={testId}>
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-8 w-full" />
      ))}
    </div>
  );
}
