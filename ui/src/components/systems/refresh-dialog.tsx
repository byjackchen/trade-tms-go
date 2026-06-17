"use client";

import { useState } from "react";
import { Sheet } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Select } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { JobProgress } from "./job-progress";
import { DATASETS, type RefreshSource } from "@/lib/api/types";
import { useRefreshData, useCancelJob } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

export function RefreshDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const [source, setSource] = useState<RefreshSource>("parquet");
  const [tables, setTables] = useState<Set<string>>(new Set());
  const [tickers, setTickers] = useState("");
  const [since, setSince] = useState("");
  const [localError, setLocalError] = useState<string | null>(null);

  const refresh = useRefreshData();
  const cancel = useCancelJob();
  const { tracked, track, reset } = useJobTracker();

  const toggleTable = (t: string) => {
    setTables((prev) => {
      const next = new Set(prev);
      if (next.has(t)) next.delete(t);
      else next.add(t);
      return next;
    });
  };

  const close = () => {
    // Allow closing mid-job; the job keeps running server-side. Reset only when
    // there is no active (non-terminal) job to avoid losing live progress.
    if (tracked && !tracked.done) {
      onClose();
      return;
    }
    reset();
    setLocalError(null);
    onClose();
  };

  const resetForm = () => {
    reset();
    setLocalError(null);
    setTickers("");
    setSince("");
    setTables(new Set());
  };

  const submit = async () => {
    setLocalError(null);
    if (since && !DATE_RE.test(since.trim())) {
      setLocalError("`since` must be YYYY-MM-DD.");
      return;
    }
    const tickerList = tickers
      .split(/[,\s]+/)
      .map((t) => t.trim().toUpperCase())
      .filter(Boolean);

    try {
      const { job, deduped } = await refresh.mutateAsync({
        source,
        tables: tables.size ? [...tables] : undefined,
        tickers: tickerList.length ? tickerList : undefined,
        since: since.trim() || undefined,
        actor: "ui",
      });
      track(job);
      if (deduped) {
        setLocalError(
          "A refresh is already in progress; tracking the existing job.",
        );
      }
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to enqueue refresh.",
      );
    }
  };

  const submitting = refresh.isPending;

  return (
    <Sheet
      open={open}
      onClose={close}
      title="Refresh market data"
      description="Enqueue a data.refresh job. At most one runs at a time."
      data-testid="refresh-dialog"
      footer={
        tracked ? (
          <>
            {tracked.done ? (
              <Button
                variant="outline"
                onClick={resetForm}
                data-testid="refresh-again"
              >
                Run another
              </Button>
            ) : null}
            <Button onClick={close} data-testid="refresh-dialog-done">
              {tracked.done ? "Done" : "Run in background"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={close} data-testid="refresh-cancel">
              Cancel
            </Button>
            <Button
              onClick={submit}
              disabled={submitting}
              data-testid="refresh-submit"
            >
              {submitting ? "Enqueuing…" : "Start refresh"}
            </Button>
          </>
        )
      }
    >
      {tracked ? (
        <JobProgress
          tracked={tracked}
          onCancel={() => cancel.mutate({ id: tracked.id, actor: "ui" })}
          canceling={cancel.isPending}
        />
      ) : (
        <div className="space-y-4" data-testid="refresh-form">
          <div className="space-y-1.5">
            <Label htmlFor="refresh-source">Source</Label>
            <Select
              id="refresh-source"
              value={source}
              onChange={(e) => setSource(e.target.value as RefreshSource)}
              data-testid="refresh-source"
            >
              <option value="parquet">parquet (local cache)</option>
              <option value="api">api (Nasdaq Data Link)</option>
            </Select>
          </div>

          <div className="space-y-1.5">
            <Label>
              Datasets{" "}
              <span className="font-normal text-muted-foreground">
                (none = all)
              </span>
            </Label>
            <div className="flex flex-wrap gap-3" data-testid="refresh-tables">
              {DATASETS.map((t) => (
                <label
                  key={t}
                  className="flex cursor-pointer items-center gap-1.5 text-sm"
                >
                  <Checkbox
                    checked={tables.has(t)}
                    onChange={() => toggleTable(t)}
                    data-testid={`refresh-table-${t}`}
                  />
                  <span className="font-mono text-xs">{t}</span>
                </label>
              ))}
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="refresh-tickers">
              Tickers{" "}
              <span className="font-normal text-muted-foreground">
                (optional, space/comma separated)
              </span>
            </Label>
            <Input
              id="refresh-tickers"
              value={tickers}
              onChange={(e) => setTickers(e.target.value)}
              placeholder="AAPL MSFT NVDA"
              className="font-mono uppercase"
              data-testid="refresh-tickers"
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="refresh-since">
              Since{" "}
              <span className="font-normal text-muted-foreground">
                (optional, YYYY-MM-DD)
              </span>
            </Label>
            <Input
              id="refresh-since"
              value={since}
              onChange={(e) => setSince(e.target.value)}
              placeholder="2024-01-01"
              className="max-w-40 font-mono"
              data-testid="refresh-since"
            />
          </div>

          {tables.size > 0 ? (
            <div className="flex flex-wrap gap-1">
              {[...tables].map((t) => (
                <Badge key={t} variant="secondary">
                  {t}
                </Badge>
              ))}
            </div>
          ) : null}

          {localError ? (
            <Alert variant="destructive" data-testid="refresh-form-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Sheet>
  );
}
