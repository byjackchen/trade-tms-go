"use client";

import { useState } from "react";
import { Dialog } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Select } from "@/components/ui/select";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { JobProgress } from "./job-progress";
import type { UniverseKind } from "@/lib/api/types";
import { useRebuildUniverse, useCancelJob } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";

const KINDS: UniverseKind[] = ["manual", "eod", "live", "backtest"];

export function RebuildDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const [kind, setKind] = useState<UniverseKind>("manual");
  const [limit, setLimit] = useState<string>("");
  const [uncapped, setUncapped] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);

  const rebuild = useRebuildUniverse();
  const cancel = useCancelJob();
  const { tracked, track, reset } = useJobTracker();

  const close = () => {
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
  };

  const submit = async () => {
    setLocalError(null);
    let limitVal: number | null | undefined;
    if (limit.trim() === "") {
      limitVal = undefined;
    } else {
      const n = Number(limit.trim());
      if (!Number.isInteger(n) || n < 0) {
        setLocalError("`limit` must be a non-negative integer (or blank).");
        return;
      }
      limitVal = n;
    }
    try {
      const { job, deduped } = await rebuild.mutateAsync({
        kind,
        limit: limitVal,
        uncapped,
        actor: "ui",
      });
      track(job);
      if (deduped) {
        setLocalError(
          "A rebuild is already in progress; tracking the existing job.",
        );
      }
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to enqueue rebuild.",
      );
    }
  };

  return (
    <Dialog
      open={open}
      onClose={close}
      title="Rebuild universe"
      description="Enqueue a universe.rebuild job. At most one runs at a time."
      data-testid="rebuild-dialog"
      footer={
        tracked ? (
          <>
            {tracked.done ? (
              <Button variant="outline" onClick={resetForm} data-testid="rebuild-again">
                Run another
              </Button>
            ) : null}
            <Button onClick={close} data-testid="rebuild-dialog-done">
              {tracked.done ? "Done" : "Run in background"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={close} data-testid="rebuild-cancel">
              Cancel
            </Button>
            <Button
              onClick={submit}
              disabled={rebuild.isPending}
              data-testid="rebuild-submit"
            >
              {rebuild.isPending ? "Enqueuing…" : "Start rebuild"}
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
        <div className="space-y-4" data-testid="rebuild-form">
          <div className="space-y-1.5">
            <Label htmlFor="rebuild-kind">Kind</Label>
            <Select
              id="rebuild-kind"
              value={kind}
              onChange={(e) => setKind(e.target.value as UniverseKind)}
              data-testid="rebuild-kind"
            >
              {KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="rebuild-limit">
              Limit{" "}
              <span className="font-normal text-muted-foreground">
                (blank = worker default; 0 = empty)
              </span>
            </Label>
            <Input
              id="rebuild-limit"
              value={limit}
              onChange={(e) => setLimit(e.target.value)}
              placeholder="85"
              inputMode="numeric"
              className="max-w-32 font-mono"
              data-testid="rebuild-limit"
            />
          </div>
          <label className="flex cursor-pointer items-center gap-2 text-sm">
            <Checkbox
              checked={uncapped}
              onChange={(e) => setUncapped(e.target.checked)}
              data-testid="rebuild-uncapped"
            />
            <span>Uncapped (ignore the cap)</span>
          </label>

          {localError ? (
            <Alert variant="destructive" data-testid="rebuild-form-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Dialog>
  );
}
