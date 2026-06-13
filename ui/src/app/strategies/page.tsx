"use client";

import Link from "next/link";
import { Boxes } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { ErrorState, EmptyState, LoadingRows } from "@/components/shell/states";
import { useStrategies } from "@/lib/api/hooks";
import type { StrategyMeta } from "@/lib/api/types";

/** Render allocation.capital_pct (a 0..1 fraction) as an integer percent. */
function allocLabel(m: StrategyMeta): string {
  if (m.capital_pct == null) return "—";
  return `${(m.capital_pct * 100).toFixed(0)}%`;
}

function sourceVariant(
  source: string,
): "default" | "secondary" | "muted" {
  if (source === "db") return "default";
  if (source === "file") return "secondary";
  return "muted";
}

export default function StrategiesPage() {
  const query = useStrategies();
  const strategies = query.data?.strategies ?? [];

  return (
    <>
      <PageHeader
        title="Strategies"
        subtitle="The four production strategies, their active params and allocations."
        data-testid="strategies-header"
      />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="strategies-page"
      >
        {query.isLoading ? (
          <LoadingRows rows={4} data-testid="strategies-loading" />
        ) : query.isError ? (
          <ErrorState
            error={query.error}
            onRetry={() => query.refetch()}
            data-testid="strategies-error"
          />
        ) : strategies.length === 0 ? (
          <EmptyState
            title="No strategies registered"
            hint="The engine ships SEPA, Sector Rotation, Pairs and ORB baselines."
            data-testid="strategies-empty"
          />
        ) : (
          <div
            className="overflow-hidden rounded-lg border border-border"
            data-testid="strategies-table"
          >
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Strategy</TableHead>
                  <TableHead>Description</TableHead>
                  <TableHead className="text-right">Allocation</TableHead>
                  <TableHead className="text-center">Enabled</TableHead>
                  <TableHead className="text-right">Params</TableHead>
                  <TableHead>Source</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {strategies.map((m) => (
                  <TableRow
                    key={m.id}
                    data-testid={`strategy-row-${m.id}`}
                    data-param-count={m.parameters_count}
                    data-source={m.params_source}
                  >
                    <TableCell>
                      <Link
                        href={`/strategies/${encodeURIComponent(m.id)}`}
                        className="flex items-center gap-2 font-medium hover:underline"
                        data-testid={`strategy-link-${m.id}`}
                      >
                        <Boxes className="size-4 text-muted-foreground" />
                        {m.label}
                      </Link>
                      <span className="ml-6 font-mono text-xs text-muted-foreground">
                        {m.id}
                      </span>
                    </TableCell>
                    <TableCell className="max-w-md text-sm text-muted-foreground">
                      {m.error ? (
                        <span className="text-destructive">
                          params error: {m.error}
                        </span>
                      ) : (
                        (m.description || "—")
                      )}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm">
                      {allocLabel(m)}
                    </TableCell>
                    <TableCell className="text-center">
                      {m.active ? (
                        <Badge
                          variant="success"
                          data-testid={`strategy-enabled-${m.id}`}
                        >
                          enabled
                        </Badge>
                      ) : (
                        <Badge variant="muted">disabled</Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm">
                      {m.parameters_count}
                    </TableCell>
                    <TableCell>
                      <Badge variant={sourceVariant(m.params_source)}>
                        {m.params_source}
                      </Badge>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </main>
    </>
  );
}
