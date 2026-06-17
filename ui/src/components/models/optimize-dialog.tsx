"use client";

import { useMemo, useState } from "react";
import { Dialog } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { JobProgress } from "@/components/systems/job-progress";
import { useOptimizeModel, useCancelJob } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";
import type { OptimizeModelRequest, Model } from "@/lib/api/types";

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

const STRATEGY_SEPA = "sepa";

/**
 * The Models-module "Optimize" — a JOINT (multi-strategy) hyperopt over a Model's
 * members (POST /models/{id}/optimize; docs/concept-alignment.md §3.3 A2). The
 * Model's active members define the joint search, so there is NO strategy
 * selector here. A study tagged `strategy=joint` is produced; on success the
 * caller can open it in the inline study panel (`onView`).
 */
export function OptimizeDialog({
  open,
  onClose,
  model,
  onView,
}: {
  open: boolean;
  onClose: () => void;
  /** The Model being optimised (its members drive the joint search). */
  model: Model;
  /** Open the freshly-launched study in the inline study panel. */
  onView?: (ts: string) => void;
}) {
  const [start, setStart] = useState("2023-01-02");
  const [end, setEnd] = useState("2023-12-29");
  const [population, setPopulation] = useState("50");
  const [generations, setGenerations] = useState("5");
  const [seed, setSeed] = useState("42");
  const [workers, setWorkers] = useState("");
  const [walkForward, setWalkForward] = useState(true);
  const [folds, setFolds] = useState("5");
  const [embargoDays, setEmbargoDays] = useState("5");
  const [tickers, setTickers] = useState("AAPL MSFT");
  const [universeMode, setUniverseMode] = useState<"tickers" | "window">("tickers");
  const [universeTable, setUniverseTable] = useState("SF1");
  const [balance, setBalance] = useState("100000");
  const [localError, setLocalError] = useState<string | null>(null);

  const optimize = useOptimizeModel(model.id);
  const cancel = useCancelJob();
  const { tracked, track, reset } = useJobTracker();

  // A joint study tuning a SEPA member needs a stock universe to search over.
  const activeMembers = useMemo(
    () => model.members.filter((m) => m.active),
    [model.members],
  );
  const needsUniverse = useMemo(
    () => activeMembers.some((m) => m.strategy_id === STRATEGY_SEPA),
    [activeMembers],
  );

  const tickerList = useMemo(
    () =>
      tickers
        .split(/[,\s]+/)
        .map((t) => t.trim().toUpperCase())
        .filter(Boolean),
    [tickers],
  );

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

  // On success the hyperopt.run job result carries study_ts.
  const rawTs =
    tracked?.status === "succeeded" ? tracked.result?.study_ts : undefined;
  const completedTs = typeof rawTs === "string" && rawTs ? rawTs : undefined;

  const submit = async () => {
    setLocalError(null);

    if (!DATE_RE.test(start.trim())) {
      setLocalError("Start date must be YYYY-MM-DD.");
      return;
    }
    if (!DATE_RE.test(end.trim())) {
      setLocalError("End date must be YYYY-MM-DD.");
      return;
    }
    if (end.trim() < start.trim()) {
      setLocalError("End date must be on or after start date.");
      return;
    }
    const pop = Number(population);
    const gen = Number(generations);
    if (!Number.isInteger(pop) || pop <= 0) {
      setLocalError("Population must be a positive integer.");
      return;
    }
    if (!Number.isInteger(gen) || gen <= 0) {
      setLocalError("Generations must be a positive integer.");
      return;
    }
    const seedNum = Number(seed);
    if (!Number.isInteger(seedNum)) {
      setLocalError("Seed must be an integer.");
      return;
    }
    const bal = Number(balance);
    if (!Number.isFinite(bal) || bal <= 0) {
      setLocalError("Starting balance must be a positive number.");
      return;
    }

    const body: OptimizeModelRequest = {
      start: start.trim(),
      end: end.trim(),
      population: pop,
      generations: gen,
      seed: seedNum,
      walk_forward: walkForward,
      starting_balance: bal,
      actor: "ui",
    };
    if (workers.trim() !== "") {
      const w = Number(workers);
      if (!Number.isInteger(w) || w < 0) {
        setLocalError("Parallelism must be a non-negative integer (0 = auto).");
        return;
      }
      if (w > 0) body.workers = w;
    }
    if (walkForward) {
      const f = Number(folds);
      const emb = Number(embargoDays);
      if (!Number.isInteger(f) || f <= 0) {
        setLocalError("Folds must be a positive integer.");
        return;
      }
      if (!Number.isInteger(emb) || emb < 0) {
        setLocalError("Embargo days must be a non-negative integer.");
        return;
      }
      body.folds = f;
      body.embargo_days = emb;
    }

    if (needsUniverse) {
      if (universeMode === "window") {
        body.universe = { start: start.trim(), end: end.trim(), table: universeTable };
      } else {
        if (tickerList.length === 0) {
          setLocalError(
            "This Model has a SEPA member — supply a stock universe (tickers or a window).",
          );
          return;
        }
        body.tickers = tickerList;
      }
    }

    try {
      const { job } = await optimize.mutateAsync(body);
      track(job);
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to enqueue optimisation.",
      );
    }
  };

  const submitting = optimize.isPending;

  return (
    <Dialog
      open={open}
      onClose={close}
      title={`Optimize ${model.name}`}
      description="Joint (multi-strategy) NSGA-II walk-forward tuning over this Model's members; progress streams live."
      data-testid="optimize-dialog"
      footer={
        tracked ? (
          <>
            {tracked.done && completedTs != null && onView ? (
              <Button
                onClick={() => {
                  onView(completedTs);
                  onClose();
                }}
                data-testid="optimize-detail-link"
              >
                View study
              </Button>
            ) : null}
            {tracked.done ? (
              <Button
                variant="outline"
                onClick={resetForm}
                data-testid="optimize-run-another"
              >
                Run another
              </Button>
            ) : null}
            <Button
              variant={tracked.done && completedTs != null ? "ghost" : "default"}
              onClick={close}
              data-testid="optimize-dialog-done"
            >
              {tracked.done ? "Close" : "Run in background"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={close} data-testid="optimize-cancel">
              Cancel
            </Button>
            <Button onClick={submit} disabled={submitting} data-testid="optimize-submit">
              {submitting ? "Enqueuing…" : "Launch optimisation"}
            </Button>
          </>
        )
      }
    >
      {tracked ? (
        <div className="space-y-3">
          <JobProgress
            tracked={tracked}
            onCancel={() => cancel.mutate({ id: tracked.id, actor: "ui" })}
            canceling={cancel.isPending}
          />
          {tracked.done && tracked.status === "succeeded" && completedTs == null ? (
            <Alert data-testid="optimize-complete-no-ts">
              <AlertDescription>
                Optimisation finished. Find the study in the studies list below.
              </AlertDescription>
            </Alert>
          ) : null}
        </div>
      ) : (
        <div className="space-y-4" data-testid="optimize-form">
          {/* joint members summary */}
          <div className="space-y-1.5" data-testid="optimize-members">
            <Label>Joint search members</Label>
            <div className="flex flex-wrap gap-2">
              {activeMembers.length === 0 ? (
                <Alert variant="warning" data-testid="optimize-no-members">
                  <AlertDescription>
                    This Model has no active members — activate at least one to
                    optimise.
                  </AlertDescription>
                </Alert>
              ) : (
                activeMembers.map((m) => (
                  <Badge key={m.strategy_id} variant="secondary">
                    {m.strategy_id} · {(m.weight * 100).toFixed(0)}%
                  </Badge>
                ))
              )}
            </div>
            <p className="text-xs text-muted-foreground">
              Each active member contributes its own bounded parameters; the
              optimiser searches them jointly and (on promote) writes the tuned
              params back to the Model&apos;s members.
            </p>
          </div>

          {/* objectives */}
          <div className="space-y-1.5" data-testid="optimize-objectives">
            <Label>Objectives</Label>
            <div className="flex flex-wrap gap-2">
              <Badge variant="secondary">sharpe ↑ maximize</Badge>
              <Badge variant="secondary">calmar ↑ maximize</Badge>
            </div>
          </div>

          {/* date range */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="op-start">Start (YYYY-MM-DD)</Label>
              <Input
                id="op-start"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                placeholder="2023-01-02"
                className="font-mono"
                data-testid="optimize-start"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="op-end">End (YYYY-MM-DD)</Label>
              <Input
                id="op-end"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                placeholder="2023-12-29"
                className="font-mono"
                data-testid="optimize-end"
              />
            </div>
          </div>

          {/* GA knobs */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="op-pop">Population</Label>
              <Input
                id="op-pop"
                value={population}
                onChange={(e) => setPopulation(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="optimize-population"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="op-gen">Generations</Label>
              <Input
                id="op-gen"
                value={generations}
                onChange={(e) => setGenerations(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="optimize-generations"
              />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="op-seed">Seed (deterministic)</Label>
              <Input
                id="op-seed"
                value={seed}
                onChange={(e) => setSeed(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="optimize-seed"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="op-workers">Parallelism (0 = auto)</Label>
              <Input
                id="op-workers"
                value={workers}
                onChange={(e) => setWorkers(e.target.value)}
                inputMode="numeric"
                placeholder="auto"
                className="font-mono"
                data-testid="optimize-workers"
              />
            </div>
          </div>

          {/* walk-forward */}
          <div className="space-y-1.5 rounded-lg border border-border p-3">
            <label
              className="flex items-center gap-2 text-sm font-medium select-none"
              data-testid="optimize-wf-toggle-label"
            >
              <input
                type="checkbox"
                checked={walkForward}
                onChange={(e) => setWalkForward(e.target.checked)}
                data-testid="optimize-walk-forward"
                className="size-4 accent-[var(--primary)]"
              />
              Walk-forward validation
            </label>
            {walkForward ? (
              <div className="grid grid-cols-2 gap-3 pt-1">
                <div className="space-y-1.5">
                  <Label htmlFor="op-folds">Folds</Label>
                  <Input
                    id="op-folds"
                    value={folds}
                    onChange={(e) => setFolds(e.target.value)}
                    inputMode="numeric"
                    className="font-mono"
                    data-testid="optimize-folds"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="op-embargo">Embargo (days)</Label>
                  <Input
                    id="op-embargo"
                    value={embargoDays}
                    onChange={(e) => setEmbargoDays(e.target.value)}
                    inputMode="numeric"
                    className="font-mono"
                    data-testid="optimize-embargo"
                  />
                </div>
              </div>
            ) : (
              <p className="text-xs text-muted-foreground">
                Disabled: each candidate is scored on a single full-window
                backtest (no anchored-expanding folds / embargo).
              </p>
            )}
          </div>

          {/* universe (only when a SEPA member is active) */}
          {needsUniverse ? (
            <div className="space-y-1.5" data-testid="optimize-universe-section">
              <Label htmlFor="op-universe-mode">Stock universe</Label>
              <Select
                id="op-universe-mode"
                value={universeMode}
                onChange={(e) => setUniverseMode(e.target.value as "tickers" | "window")}
                data-testid="optimize-universe-mode"
              >
                <option value="tickers">Explicit tickers</option>
                <option value="window">Point-in-time universe window</option>
              </Select>
              {universeMode === "tickers" ? (
                <>
                  <Input
                    value={tickers}
                    onChange={(e) => setTickers(e.target.value)}
                    placeholder="AAPL MSFT"
                    className="font-mono uppercase"
                    data-testid="optimize-tickers"
                  />
                  {tickerList.length > 0 ? (
                    <div className="flex flex-wrap gap-1 pt-1">
                      {tickerList.map((t) => (
                        <Badge key={t} variant="secondary">
                          {t}
                        </Badge>
                      ))}
                    </div>
                  ) : null}
                </>
              ) : (
                <Select
                  value={universeTable}
                  onChange={(e) => setUniverseTable(e.target.value)}
                  data-testid="optimize-universe-table"
                >
                  <option value="SF1">SF1 (common stocks)</option>
                  <option value="SFP">SFP (ETFs / funds)</option>
                </Select>
              )}
              <p className="text-xs text-muted-foreground">
                The SEPA member requires a stock universe. Sector Rotation / Pairs
                derive instruments from params.
              </p>
            </div>
          ) : (
            <Alert data-testid="optimize-derived-universe">
              <AlertDescription>
                No active SEPA member — the members resolve instruments from their
                params, so there is no universe to select.
              </AlertDescription>
            </Alert>
          )}

          {/* starting balance */}
          <div className="space-y-1.5">
            <Label htmlFor="op-balance">Starting balance (USD)</Label>
            <Input
              id="op-balance"
              value={balance}
              onChange={(e) => setBalance(e.target.value)}
              inputMode="decimal"
              className="max-w-44 font-mono"
              data-testid="optimize-balance"
            />
          </div>

          {localError ? (
            <Alert variant="destructive" data-testid="optimize-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Dialog>
  );
}
