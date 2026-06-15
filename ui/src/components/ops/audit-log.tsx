"use client";

import { useState } from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { useAudit } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import { formatRelative, formatTs } from "@/lib/format";
import type { AuditEntry } from "@/lib/api/types";

/** Compose the target label from an audit row's entity + entity_id. */
function target(e: AuditEntry): string {
  if (!e.entity) return "—";
  return e.entity_id ? `${e.entity} #${e.entity_id}` : e.entity;
}

/**
 * AUDIT LOG: the append-only operational trail (tms.audit_log), newest-first,
 * with actor + action filters. A 503 (no audit reader wired into the API) is an
 * expected degraded state rendered as a "not configured" empty panel.
 */
export function AuditLog() {
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");
  const { data, isLoading, error, refetch } = useAudit({
    actor: actor.trim() || undefined,
    action: action.trim() || undefined,
  });

  const notConfigured = error instanceof ApiError && error.status === 503;
  const entries = data?.entries ?? [];

  return (
    <Card data-testid="audit-log-card">
      <CardHeader>
        <div className="space-y-1">
          <CardTitle>Audit log</CardTitle>
          <CardDescription>
            Append-only operational trail — newest first.
          </CardDescription>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <Input
            className="w-48"
            placeholder="Filter actor…"
            value={actor}
            onChange={(e) => setActor(e.target.value)}
            data-testid="audit-actor-filter"
            aria-label="Filter by actor"
          />
          <Input
            className="w-48"
            placeholder="Filter action…"
            value={action}
            onChange={(e) => setAction(e.target.value)}
            data-testid="audit-action-filter"
            aria-label="Filter by action"
          />
          <span
            className="ml-auto text-xs text-muted-foreground"
            data-testid="audit-count"
          >
            {entries.length} {entries.length === 1 ? "entry" : "entries"}
          </span>
        </div>

        {isLoading ? (
          <LoadingRows rows={6} data-testid="audit-loading" />
        ) : notConfigured ? (
          <EmptyState
            title="Audit log not available"
            hint="The API has no audit reader configured."
            data-testid="audit-not-configured"
          />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="audit-error"
          />
        ) : entries.length === 0 ? (
          <EmptyState
            title="No audit entries"
            hint={
              actor || action
                ? "No entries match the current filters."
                : "Operational actions (enqueues, promotions, halts) appear here."
            }
            data-testid="audit-empty"
          />
        ) : (
          <div className="overflow-x-auto">
            <Table data-testid="audit-table">
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Actor</TableHead>
                  <TableHead>Action</TableHead>
                  <TableHead>Target</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {entries.map((e) => (
                  <TableRow key={e.id} data-testid={`audit-row-${e.id}`}>
                    <TableCell
                      className="whitespace-nowrap text-xs text-muted-foreground"
                      title={formatTs(e.ts)}
                    >
                      {formatRelative(e.ts)}
                    </TableCell>
                    <TableCell className="font-mono text-[11px]">
                      {e.actor}
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline" data-testid={`audit-action-${e.id}`}>
                        {e.action}
                      </Badge>
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-muted-foreground">
                      {target(e)}
                    </TableCell>
                    <TableCell className="max-w-xs">
                      {e.details && Object.keys(e.details).length > 0 ? (
                        <code
                          className="block truncate text-[11px] text-muted-foreground"
                          title={JSON.stringify(e.details)}
                          data-testid={`audit-details-${e.id}`}
                        >
                          {JSON.stringify(e.details)}
                        </code>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
