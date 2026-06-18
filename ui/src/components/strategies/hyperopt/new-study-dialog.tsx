"use client";

import { useMemo, useState } from "react";
import { Sheet } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { JobProgress } from "@/components/systems/job-progress";
import { cn } from "@/lib/utils";
import { useCreateStudy, useCancelJob, useStrategy } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";
import type {
  CreateStudyRequest,
  HyperoptStrategy,
  ParamSchema,
} from "@/lib/api/types";

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

// POST /hyperopt is single-strategy ONLY (concept-alignment §3.3, A2): params are
// tuned per-strategy here. Joint (multi-strategy) tuning is dropped from the
// product — Compositions compose already-tuned strategies and are validated by Backtest.
const STRATEGY_OPTIONS: { value: HyperoptStrategy; label: string }[] = [
  { value: "sepa", label: "SEPA" },
  { value: "sector_rotation", label: "Sector Rotation" },
  { value: "pairs", label: "Pairs" },
];

/** sepa requires a stock universe (handlers_hyperopt.go §POST validation). */
function needsUniverse(s: HyperoptStrategy): boolean {
  return s === "sepa";
}

/**
 * Map a hyperopt strategy to the strategy id whose param schema the search-space
 * preset is drawn from.
 */
function schemaIdFor(s: HyperoptStrategy): string | null {
  switch (s) {
    case "sepa":
      return "sepa";
    case "sector_rotation":
      return "sector_rotation";
    case "pairs":
      return "pairs";
  }
}

/** A param row is part of the search space iff it carries numeric bounds. */
function isTunable(p: ParamSchema): boolean {
  return (
    typeof p.search_low === "number" && typeof p.search_high === "number"
  );
}

export function NewStudyDialog({
  open,
  onClose,
  defaultStrategy = "pairs",
  lockStrategy = false,
  onView,
}: {
  open: boolean;
  onClose: () => void;
  /** Preselect the strategy (the per-strategy Tune panel passes its own). */
  defaultStrategy?: HyperoptStrategy;
  /** Hide the strategy selector when the dialog is scoped to one strategy. */
  lockStrategy?: boolean;
  /** Open the freshly-completed study in the inline detail panel. */
  onView?: (ts: string) => void;
}) {
  // Mobile (CSS via ui-mobile: variant): stack the two-up input rows into one
  // column so each field gets the full sheet width. Desktop is the base.
  const grid2 = "grid gap-3 grid-cols-2 ui-mobile:grid-cols-1";
  const [strategy, setStrategy] = useState<HyperoptStrategy>(defaultStrategy);
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
  const [universeMode, setUniverseMode] = useState<"tickers" | "window">(
    "tickers",
  );
  const [universeTable, setUniverseTable] = useState("SF1");
  const [balance, setBalance] = useState("100000");
  const [localError, setLocalError] = useState<string | null>(null);

  const create = useCreateStudy();
  const cancel = useCancelJob();
  const { tracked, track, reset } = useJobTracker();

  // Search-space preset: the selected strategy's tunable params (with bounds).
  const schemaId = schemaIdFor(strategy);
  const { data: stratData, isLoading: schemaLoading } = useStrategy(
    open ? schemaId : null,
  );
  const tunableParams = useMemo<ParamSchema[]>(
    () => (stratData?.strategy.parameters ?? []).filter(isTunable),
    [stratData],
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

  // On success the hyperopt.run job result carries study_ts; route to detail.
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

    const body: CreateStudyRequest = {
      strategy,
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

    if (needsUniverse(strategy)) {
      if (universeMode === "window") {
        body.universe = {
          start: start.trim(),
          end: end.trim(),
          table: universeTable,
        };
      } else {
        if (tickerList.length === 0) {
          setLocalError(
            `${strategy} requires a stock universe — enter tickers or use a window.`,
          );
          return;
        }
        body.tickers = tickerList;
      }
    }

    try {
      const { job } = await create.mutateAsync(body);
      track(job);
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to enqueue study.",
      );
    }
  };

  const submitting = create.isPending;

  return (
    <Sheet
      open={open}
      onClose={close}
      title="Run Hyperopt"
      description="Launch a seeded NSGA-II walk-forward hyperopt study; progress streams live."
      data-testid="hyperopt-dialog"
      footer={
        tracked ? (
          <>
            {tracked.done && completedTs != null && onView ? (
              <Button
                onClick={() => {
                  onView(completedTs);
                  onClose();
                }}
                data-testid="hyperopt-detail-link"
              >
                View study
              </Button>
            ) : null}
            {tracked.done ? (
              <Button
                variant="outline"
                onClick={resetForm}
                data-testid="study-run-another"
              >
                Run another
              </Button>
            ) : null}
            <Button
              variant={tracked.done && completedTs != null ? "ghost" : "default"}
              onClick={close}
              data-testid="study-dialog-done"
            >
              {tracked.done ? "Close" : "Run in background"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={close} data-testid="study-cancel">
              Cancel
            </Button>
            <Button onClick={submit} disabled={submitting} data-testid="hyperopt-submit">
              {submitting ? "Enqueuing…" : "Launch study"}
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
            <Alert data-testid="study-complete-no-ts">
              <AlertDescription>
                Study finished. Find it in the studies list below.
              </AlertDescription>
            </Alert>
          ) : null}
        </div>
      ) : (
        <div className="space-y-4" data-testid="hyperopt-form">
          {/* strategy — hidden (locked) when the dialog is scoped to one tab */}
          {lockStrategy ? null : (
            <div className="space-y-1.5">
              <Label htmlFor="hp-strategy">Strategy</Label>
              <Select
                id="hp-strategy"
                value={strategy}
                onChange={(e) => {
                  setStrategy(e.target.value as HyperoptStrategy);
                  setLocalError(null);
                }}
                data-testid="hyperopt-strategy"
              >
                {STRATEGY_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </Select>
            </div>
          )}

          {/* objectives (taken verbatim from the spec; both maximized) */}
          <div className="space-y-1.5" data-testid="study-objectives">
            <Label>Objectives</Label>
            <div className="flex flex-wrap gap-2">
              <Badge variant="secondary">sharpe ↑ maximize</Badge>
              <Badge variant="secondary">calmar ↑ maximize</Badge>
            </div>
            <p className="text-xs text-muted-foreground">
              Multi-objective NSGA-II over the aggregate per-fold backtest
              metrics. The Pareto front trades Sharpe against Calmar.
            </p>
          </div>

          {/* search space preset */}
          <div className="space-y-1.5" data-testid="study-search-space">
            <Label>Search space</Label>
            {schemaId == null ? (
              <Alert data-testid="study-search-space-joint">
                <AlertDescription>
                  A joint study tunes SEPA, Sector Rotation and Pairs together —
                  each sub-strategy contributes its own bounded parameters,
                  resolved server-side from the strategy schemas.
                </AlertDescription>
              </Alert>
            ) : schemaLoading ? (
              <p className="text-xs text-muted-foreground">Loading schema…</p>
            ) : tunableParams.length === 0 ? (
              <Alert variant="warning" data-testid="study-search-space-empty">
                <AlertDescription>
                  This strategy exposes no tunable (bounded) parameters in its
                  schema.
                </AlertDescription>
              </Alert>
            ) : (
              <div
                className="cockpit-scroll max-h-40 space-y-1 overflow-y-auto rounded-lg border border-border bg-background/60 p-2"
                data-testid="study-search-space-list"
              >
                {tunableParams.map((p) => (
                  <div
                    key={p.name}
                    className="flex items-center justify-between gap-3 text-xs"
                    data-testid={`study-param-${p.name}`}
                  >
                    <span className="font-mono">{p.name}</span>
                    <span className="text-muted-foreground tabular-nums">
                      [{p.search_low} … {p.search_high}]
                      <span className="ml-1 uppercase">{p.type}</span>
                    </span>
                  </div>
                ))}
              </div>
            )}
            <p className="text-xs text-muted-foreground">
              The search bounds come from the strategy&apos;s param schema (spec
              search-space). The study samples within these ranges.
            </p>
          </div>

          {/* date range + balance */}
          <div className={grid2}>
            <div className="space-y-1.5">
              <Label htmlFor="hp-start">Start (YYYY-MM-DD)</Label>
              <Input
                id="hp-start"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                placeholder="2023-01-02"
                className="font-mono"
                data-testid="hyperopt-start"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hp-end">End (YYYY-MM-DD)</Label>
              <Input
                id="hp-end"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                placeholder="2023-12-29"
                className="font-mono"
                data-testid="hyperopt-end"
              />
            </div>
          </div>

          {/* NSGA-II population / generations / seed / parallelism */}
          <div className={grid2}>
            <div className="space-y-1.5">
              <Label htmlFor="hp-pop">Population</Label>
              <Input
                id="hp-pop"
                value={population}
                onChange={(e) => setPopulation(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="hyperopt-population"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hp-gen">Generations</Label>
              <Input
                id="hp-gen"
                value={generations}
                onChange={(e) => setGenerations(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="hyperopt-generations"
              />
            </div>
          </div>

          <div className={grid2}>
            <div className="space-y-1.5">
              <Label htmlFor="hp-seed">Seed (deterministic)</Label>
              <Input
                id="hp-seed"
                value={seed}
                onChange={(e) => setSeed(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="study-seed"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hp-workers">Parallelism (0 = auto)</Label>
              <Input
                id="hp-workers"
                value={workers}
                onChange={(e) => setWorkers(e.target.value)}
                inputMode="numeric"
                placeholder="auto"
                className="font-mono"
                data-testid="study-workers"
              />
            </div>
          </div>

          {/* walk-forward */}
          <div className="space-y-1.5 rounded-lg border border-border p-3">
            <label
              className="flex items-center gap-2 text-sm font-medium select-none"
              data-testid="study-wf-toggle-label"
            >
              <input
                type="checkbox"
                checked={walkForward}
                onChange={(e) => setWalkForward(e.target.checked)}
                data-testid="study-walk-forward"
                className="size-4 accent-[var(--primary)]"
              />
              Walk-forward validation
            </label>
            {walkForward ? (
              <div className={cn(grid2, "pt-1")}>
                <div className="space-y-1.5">
                  <Label htmlFor="hp-folds">Folds</Label>
                  <Input
                    id="hp-folds"
                    value={folds}
                    onChange={(e) => setFolds(e.target.value)}
                    inputMode="numeric"
                    className="font-mono"
                    data-testid="hyperopt-folds"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="hp-embargo">Embargo (days)</Label>
                  <Input
                    id="hp-embargo"
                    value={embargoDays}
                    onChange={(e) => setEmbargoDays(e.target.value)}
                    inputMode="numeric"
                    className="font-mono"
                    data-testid="study-embargo"
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

          {/* universe for sepa / joint */}
          {needsUniverse(strategy) ? (
            <div className="space-y-1.5" data-testid="study-universe-section">
              <Label htmlFor="hp-universe-mode">Stock universe</Label>
              <Select
                id="hp-universe-mode"
                value={universeMode}
                onChange={(e) =>
                  setUniverseMode(e.target.value as "tickers" | "window")
                }
                data-testid="study-universe-mode"
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
                    data-testid="hyperopt-tickers"
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
                  data-testid="study-universe-table"
                >
                  <option value="SF1">SF1 (common stocks)</option>
                  <option value="SFP">SFP (ETFs / funds)</option>
                </Select>
              )}
              <p className="text-xs text-muted-foreground">
                SEPA requires a stock universe. Sector Rotation / Pairs derive
                instruments from params.
              </p>
            </div>
          ) : (
            <Alert data-testid="study-derived-universe">
              <AlertDescription>
                {strategy === "pairs"
                  ? "The pair legs are resolved from the tuned params — no universe to select."
                  : "The sector ETF set is resolved from the tuned params — no universe to select."}
              </AlertDescription>
            </Alert>
          )}

          {/* starting balance */}
          <div className="space-y-1.5">
            <Label htmlFor="hp-balance">Starting balance (USD)</Label>
            <Input
              id="hp-balance"
              value={balance}
              onChange={(e) => setBalance(e.target.value)}
              inputMode="decimal"
              className="max-w-44 font-mono"
              data-testid="study-balance"
            />
          </div>

          {localError ? (
            <Alert variant="destructive" data-testid="new-study-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Sheet>
  );
}
