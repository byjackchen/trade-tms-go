"use client";

import { useState } from "react";
import { Hammer } from "lucide-react";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { RebuildDialog } from "./rebuild-dialog";
import { ApiError } from "@/lib/api/client";
import { useUniverseLatest } from "@/lib/api/hooks";
import { formatInt, formatRelative, formatTs } from "@/lib/format";

function Stat({
  label,
  value,
  testid,
}: {
  label: string;
  value: React.ReactNode;
  testid: string;
}) {
  return (
    <div className="rounded-lg border border-border px-3 py-2">
      <div className="text-[11px] uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div className="mt-0.5 text-sm font-medium tabular-nums" data-testid={testid}>
        {value}
      </div>
    </div>
  );
}

export function UniverseCard() {
  const [dialogOpen, setDialogOpen] = useState(false);
  const { data, isLoading, error, refetch } = useUniverseLatest();

  const snapshot = data?.snapshot;
  const noSnapshot = error instanceof ApiError && error.status === 404;

  return (
    <Card data-testid="universe-card">
      <CardHeader>
        <div>
          <CardTitle>Universe</CardTitle>
          <CardDescription>Latest membership snapshot.</CardDescription>
        </div>
        <CardAction>
          <Button
            size="sm"
            onClick={() => setDialogOpen(true)}
            data-testid="universe-rebuild"
          >
            <Hammer />
            Rebuild
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={3} data-testid="universe-loading" />
        ) : noSnapshot ? (
          <EmptyState
            title="No universe snapshot yet"
            hint="Rebuild to compute the first membership snapshot."
            data-testid="universe-empty"
          />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="universe-error"
          />
        ) : snapshot ? (
          <div className="space-y-3" data-testid="universe-summary">
            <div className="flex flex-wrap items-center gap-2">
              <Badge variant="secondary" data-testid="universe-kind">
                {snapshot.kind}
              </Badge>
              <Badge variant="outline" data-testid="universe-asof">
                as of {snapshot.as_of}
              </Badge>
              <span
                className="text-xs text-muted-foreground"
                title={formatTs(snapshot.created_at)}
              >
                built {formatRelative(snapshot.created_at)}
              </span>
            </div>
            <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
              <Stat
                label="Members"
                value={formatInt(snapshot.tickers.length)}
                testid="universe-members-count"
              />
              <Stat
                label="Limit"
                value={snapshot.limit_n ? formatInt(snapshot.limit_n) : "—"}
                testid="universe-limit"
              />
              <Stat
                label="Excluded"
                value={formatInt(snapshot.excluded.length)}
                testid="universe-excluded-count"
              />
            </div>
            {snapshot.members.length > 0 ? (
              <div>
                <div className="mb-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">
                  Top members
                </div>
                <div className="flex flex-wrap gap-1" data-testid="universe-top-members">
                  {snapshot.members.slice(0, 12).map((m) => (
                    <Badge
                      key={m.ticker}
                      variant="muted"
                      className="font-mono"
                      title={`rank ${m.rank} · score ${m.score}`}
                    >
                      {m.ticker}
                    </Badge>
                  ))}
                  {snapshot.members.length > 12 ? (
                    <Badge variant="outline">
                      +{snapshot.members.length - 12}
                    </Badge>
                  ) : null}
                </div>
              </div>
            ) : null}
          </div>
        ) : (
          <EmptyState title="No universe data" data-testid="universe-nodata" />
        )}
      </CardContent>
      <RebuildDialog open={dialogOpen} onClose={() => setDialogOpen(false)} />
    </Card>
  );
}
