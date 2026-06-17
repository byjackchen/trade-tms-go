"use client";

import { useMemo, useState } from "react";
import { Sheet } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { cn } from "@/lib/utils";
import { JobProgress } from "@/components/systems/job-progress";
import { useCompositionHyperopt, useCancelJob } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";
import {
  DEFAULT_COMPOSITION_HYPEROPT_RANGES,
  type Composition,
  type CompositionHyperoptRequest,
  type HyperoptRange,
} from "@/lib/api/types";

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

/** A range as edit-friendly strings. */
type RangeDraft = { low: string; high: string };

function toDraft(r: HyperoptRange): RangeDraft {
  return { low: String(r.low), high: String(r.high) };
}

function parseRange(d: RangeDraft): HyperoptRange | null {
  const low = Number(d.low);
  const high = Number(d.high);
  if (!Number.isFinite(low) || !Number.isFinite(high)) return null;
  if (low < 0 || high <= low) return null;
  return { low, high };
}

/** A labelled low/high range editor row. */
function RangeRow({
  label,
  testid,
  value,
  onChange,
  mobile,
}: {
  label: string;
  testid: string;
  value: RangeDraft;
  onChange: (next: RangeDraft) => void;
  mobile?: boolean;
}) {
  const inputCls = cn("w-20 font-mono", mobile ? "h-11" : "h-8");
  return (
    <div
      className="flex items-center justify-between gap-3"
      data-testid={`composition-hp-range-${testid}`}
    >
      <span className="min-w-0 font-mono text-xs">{label}</span>
      <div className="flex shrink-0 items-center gap-1.5">
        <Input
          value={value.low}
          onChange={(e) => onChange({ ...value, low: e.target.value })}
          inputMode="decimal"
          className={inputCls}
          data-testid={`composition-hp-${testid}-low`}
          aria-label={`${label} low`}
        />
        <span className="text-muted-foreground">…</span>
        <Input
          value={value.high}
          onChange={(e) => onChange({ ...value, high: e.target.value })}
          inputMode="decimal"
          className={inputCls}
          data-testid={`composition-hp-${testid}-high`}
          aria-label={`${label} high`}
        />
      </div>
    </div>
  );
}

/**
 * The Composition Hyperopt launch dialog (weights & risk). DISTINCT from the
 * strategy NewStudyDialog (which tunes signal params): here the member strategy
 * params are FIXED at their active set (LOCKED DECISION 4); the search is over each
 * ACTIVE member's raw weight + raw cash (normalized to a simplex, LOCKED DECISION
 * 1a) and the three portfolio-level risk caps. The ranges prefill from the LOCKED
 * global DEFAULTS (LOCKED DECISION 6) and are EDITABLE before launch (LOCKED
 * DECISION 2). Reuses the SAME NSGA-II + walk-forward machinery (LOCKED DECISION 6).
 */
export function CompositionHyperoptDialog({
  open,
  onClose,
  composition,
  onView,
}: {
  open: boolean;
  onClose: () => void;
  composition: Composition;
  /** Open the freshly-completed study in the inline panel. */
  onView?: (ts: string) => void;
}) {
  const { mode } = useUiMode();
  const mobile = mode === "mobile";
  const grid2 = cn("grid gap-3", mobile ? "grid-cols-1" : "grid-cols-2");

  // Only ACTIVE members get a weight dim (LOCKED DECISION 1a / 4).
  const activeMembers = useMemo(
    () => composition.members.filter((m) => m.active),
    [composition.members],
  );

  const [start, setStart] = useState("2023-01-02");
  const [end, setEnd] = useState("2023-12-29");
  const [population, setPopulation] = useState("50");
  const [generations, setGenerations] = useState("5");
  const [seed, setSeed] = useState("42");
  const [workers, setWorkers] = useState("");
  const [walkForward, setWalkForward] = useState(true);
  const [folds, setFolds] = useState("5");
  const [embargoDays, setEmbargoDays] = useState("5");
  const [balance, setBalance] = useState("100000");

  // Editable search ranges, prefilled from the LOCKED defaults. The server applies a
  // SINGLE shared raw-weight range to every active member's weight dim, so there is
  // one weight row (not one per member).
  const [weight, setWeight] = useState<RangeDraft>(
    toDraft(DEFAULT_COMPOSITION_HYPEROPT_RANGES.member_weight),
  );
  const [cash, setCash] = useState<RangeDraft>(
    toDraft(DEFAULT_COMPOSITION_HYPEROPT_RANGES.cash),
  );
  const [singleName, setSingleName] = useState<RangeDraft>(
    toDraft(DEFAULT_COMPOSITION_HYPEROPT_RANGES.single_name_pct),
  );
  const [concentration, setConcentration] = useState<RangeDraft>(
    toDraft(DEFAULT_COMPOSITION_HYPEROPT_RANGES.concentration_pct),
  );
  const [dailyLossHalt, setDailyLossHalt] = useState<RangeDraft>(
    toDraft(DEFAULT_COMPOSITION_HYPEROPT_RANGES.daily_loss_halt_pct),
  );

  const [localError, setLocalError] = useState<string | null>(null);

  const create = useCompositionHyperopt(composition.id);
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

  const rawTs =
    tracked?.status === "succeeded" ? tracked.result?.study_ts : undefined;
  const completedTs = typeof rawTs === "string" && rawTs ? rawTs : undefined;

  const submit = async () => {
    setLocalError(null);

    if (!DATE_RE.test(start.trim()) || !DATE_RE.test(end.trim())) {
      setLocalError("Start and end dates must be YYYY-MM-DD.");
      return;
    }
    if (end.trim() < start.trim()) {
      setLocalError("End date must be on or after start date.");
      return;
    }
    if (activeMembers.length === 0) {
      setLocalError(
        "This Composition has no active members — activate at least one before tuning.",
      );
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

    // Parse + validate every range (low ≥ 0, high > low).
    const weightR = parseRange(weight);
    const cashR = parseRange(cash);
    const singleR = parseRange(singleName);
    const concR = parseRange(concentration);
    const haltR = parseRange(dailyLossHalt);
    if (!weightR) {
      setLocalError("Raw weight range must be 0 ≤ low < high.");
      return;
    }
    if (!cashR) {
      setLocalError("Cash range must be 0 ≤ low < high.");
      return;
    }
    if (!singleR || !concR || !haltR) {
      setLocalError("Each risk-cap range must be 0 ≤ low < high.");
      return;
    }

    // Emit the server's FLAT range overrides ([low, high] tuples).
    const toPair = (r: HyperoptRange): [number, number] => [r.low, r.high];

    const body: CompositionHyperoptRequest = {
      start: start.trim(),
      end: end.trim(),
      population: pop,
      generations: gen,
      seed: seedNum,
      walk_forward: walkForward,
      starting_balance: bal,
      weight: toPair(weightR),
      cash: toPair(cashR),
      single_name: toPair(singleR),
      concentration: toPair(concR),
      daily_loss: toPair(haltR),
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

    try {
      const { job } = await create.mutateAsync(body);
      track(job);
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to enqueue hyperopt.",
      );
    }
  };

  const submitting = create.isPending;

  return (
    <Sheet
      open={open}
      onClose={close}
      title="Composition Hyperopt — weights & risk"
      description="Search this Composition's member weights + cash + risk caps with NSGA-II walk-forward. Strategy params stay FIXED at each member's active set."
      data-testid="composition-hyperopt-dialog"
      className={mobile ? undefined : "w-[min(48rem,calc(100vw-2rem))]"}
      footer={
        tracked ? (
          <>
            {tracked.done && completedTs != null && onView ? (
              <Button
                onClick={() => {
                  onView(completedTs);
                  onClose();
                }}
                data-testid="composition-hyperopt-detail-link"
              >
                View study
              </Button>
            ) : null}
            {tracked.done ? (
              <Button
                variant="outline"
                onClick={resetForm}
                data-testid="composition-hyperopt-run-another"
              >
                Run another
              </Button>
            ) : null}
            <Button
              variant={tracked.done && completedTs != null ? "ghost" : "default"}
              onClick={close}
              data-testid="composition-hyperopt-done"
            >
              {tracked.done ? "Close" : "Run in background"}
            </Button>
          </>
        ) : (
          <>
            <Button
              variant="ghost"
              onClick={close}
              data-testid="composition-hyperopt-cancel"
            >
              Cancel
            </Button>
            <Button
              onClick={submit}
              disabled={submitting}
              data-testid="composition-hyperopt-submit"
            >
              {submitting ? "Enqueuing…" : "Launch hyperopt"}
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
          {tracked.done &&
          tracked.status === "succeeded" &&
          completedTs == null ? (
            <Alert data-testid="composition-hyperopt-no-ts">
              <AlertDescription>
                Hyperopt finished. Find it in the studies list below.
              </AlertDescription>
            </Alert>
          ) : null}
        </div>
      ) : (
        <div className="space-y-4" data-testid="composition-hyperopt-form">
          {/* what this tunes — the unmistakable distinction */}
          <Alert data-testid="composition-hyperopt-scope">
            <AlertDescription>
              Tunes <strong>weights &amp; risk</strong> only. Each member&apos;s
              strategy signal params are FIXED at its active set — to retune those,
              use the strategy&apos;s <strong>Strategy Hyperopt — signal params</strong>{" "}
              panel.
            </AlertDescription>
          </Alert>

          {/* objectives */}
          <div className="space-y-1.5" data-testid="composition-hyperopt-objectives">
            <Label>Objectives</Label>
            <div className="flex flex-wrap gap-2">
              <Badge variant="secondary">sharpe ↑ maximize</Badge>
              <Badge variant="secondary">calmar ↑ maximize</Badge>
            </div>
          </div>

          {/* weight + cash ranges */}
          <div className="space-y-1.5" data-testid="composition-hyperopt-weights">
            <Label>Raw weight + cash ranges</Label>
            {activeMembers.length === 0 ? (
              <Alert variant="warning" data-testid="composition-hyperopt-no-active">
                <AlertDescription>
                  No active members — activate at least one member in the Composer
                  before tuning weights.
                </AlertDescription>
              </Alert>
            ) : (
              <div className="space-y-1 rounded-lg border border-border bg-background/60 p-2">
                <RangeRow
                  label="weight (raw, per active member)"
                  testid="weight"
                  value={weight}
                  mobile={mobile}
                  onChange={(next) => {
                    setWeight(next);
                    setLocalError(null);
                  }}
                />
                <RangeRow
                  label="cash (raw)"
                  testid="cash"
                  value={cash}
                  mobile={mobile}
                  onChange={(next) => {
                    setCash(next);
                    setLocalError(null);
                  }}
                />
              </div>
            )}
            <p className="text-xs text-muted-foreground">
              One shared raw-weight range applies to every active member. Raw weights +
              cash are normalized to a simplex so Σ(weights) + cash = 1 (always
              feasible).
            </p>
          </div>

          {/* risk-cap ranges */}
          <div className="space-y-1.5" data-testid="composition-hyperopt-risk">
            <Label>Risk-cap ranges (fractions)</Label>
            <div className="space-y-1 rounded-lg border border-border bg-background/60 p-2">
              <RangeRow
                label="single_name_pct"
                testid="single-name"
                value={singleName}
                mobile={mobile}
                onChange={(next) => {
                  setSingleName(next);
                  setLocalError(null);
                }}
              />
              <RangeRow
                label="concentration_pct"
                testid="concentration"
                value={concentration}
                mobile={mobile}
                onChange={(next) => {
                  setConcentration(next);
                  setLocalError(null);
                }}
              />
              <RangeRow
                label="daily_loss_halt_pct"
                testid="daily-loss-halt"
                value={dailyLossHalt}
                mobile={mobile}
                onChange={(next) => {
                  setDailyLossHalt(next);
                  setLocalError(null);
                }}
              />
            </div>
            <p className="text-xs text-muted-foreground">
              Default ranges (LOCKED) — editable before launch.
            </p>
          </div>

          {/* date range + balance */}
          <div className={grid2}>
            <div className="space-y-1.5">
              <Label htmlFor="ch-start">Start (YYYY-MM-DD)</Label>
              <Input
                id="ch-start"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                className="font-mono"
                data-testid="composition-hyperopt-start"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ch-end">End (YYYY-MM-DD)</Label>
              <Input
                id="ch-end"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                className="font-mono"
                data-testid="composition-hyperopt-end"
              />
            </div>
          </div>

          {/* NSGA-II population / generations / seed / parallelism */}
          <div className={grid2}>
            <div className="space-y-1.5">
              <Label htmlFor="ch-pop">Population</Label>
              <Input
                id="ch-pop"
                value={population}
                onChange={(e) => setPopulation(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="composition-hyperopt-population"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ch-gen">Generations</Label>
              <Input
                id="ch-gen"
                value={generations}
                onChange={(e) => setGenerations(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="composition-hyperopt-generations"
              />
            </div>
          </div>

          <div className={grid2}>
            <div className="space-y-1.5">
              <Label htmlFor="ch-seed">Seed (deterministic)</Label>
              <Input
                id="ch-seed"
                value={seed}
                onChange={(e) => setSeed(e.target.value)}
                inputMode="numeric"
                className="font-mono"
                data-testid="composition-hyperopt-seed"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ch-workers">Parallelism (0 = auto)</Label>
              <Input
                id="ch-workers"
                value={workers}
                onChange={(e) => setWorkers(e.target.value)}
                inputMode="numeric"
                placeholder="auto"
                className="font-mono"
                data-testid="composition-hyperopt-workers"
              />
            </div>
          </div>

          {/* walk-forward */}
          <div className="space-y-1.5 rounded-lg border border-border p-3">
            <label className="flex items-center gap-2 text-sm font-medium select-none">
              <input
                type="checkbox"
                checked={walkForward}
                onChange={(e) => setWalkForward(e.target.checked)}
                data-testid="composition-hyperopt-walk-forward"
                className="size-4 accent-[var(--primary)]"
              />
              Walk-forward validation
            </label>
            {walkForward ? (
              <div className={cn(grid2, "pt-1")}>
                <div className="space-y-1.5">
                  <Label htmlFor="ch-folds">Folds</Label>
                  <Input
                    id="ch-folds"
                    value={folds}
                    onChange={(e) => setFolds(e.target.value)}
                    inputMode="numeric"
                    className="font-mono"
                    data-testid="composition-hyperopt-folds"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="ch-embargo">Embargo (days)</Label>
                  <Input
                    id="ch-embargo"
                    value={embargoDays}
                    onChange={(e) => setEmbargoDays(e.target.value)}
                    inputMode="numeric"
                    className="font-mono"
                    data-testid="composition-hyperopt-embargo"
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

          {/* starting balance */}
          <div className="space-y-1.5">
            <Label htmlFor="ch-balance">Starting balance (USD)</Label>
            <Input
              id="ch-balance"
              value={balance}
              onChange={(e) => setBalance(e.target.value)}
              inputMode="decimal"
              className="max-w-44 font-mono"
              data-testid="composition-hyperopt-balance"
            />
          </div>

          {localError ? (
            <Alert variant="destructive" data-testid="composition-hyperopt-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Sheet>
  );
}
