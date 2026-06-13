"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import { Dialog } from "@/components/ui/dialog";
import { Button, buttonVariants } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { JobProgress } from "@/components/data/job-progress";
import { useCreateBacktest, useCancelJob } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";
import type {
  CreateBacktestRequest,
  FillProfile,
  BacktestIntent,
} from "@/lib/api/types";

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

type IntentSource = "explicit" | "universe";

/**
 * Parse the intents textarea. One intent per line:
 *   YYYY-MM-DD,TICKER,SIDE,QTY   (commas or whitespace separated)
 * Blank lines and `#` comments are ignored. Returns parsed intents or the
 * first error line for surfacing.
 */
function parseIntents(text: string): {
  intents: BacktestIntent[];
  error: string | null;
} {
  const intents: BacktestIntent[] = [];
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const raw = (lines[i] ?? "").trim();
    if (!raw || raw.startsWith("#")) continue;
    const parts = raw.split(/[,\s]+/).filter(Boolean);
    if (parts.length < 4) {
      return { intents: [], error: `Line ${i + 1}: expected "date,ticker,side,qty".` };
    }
    const date = parts[0] ?? "";
    const ticker = parts[1] ?? "";
    const side = (parts[2] ?? "").toUpperCase();
    const qtyRaw = parts[3] ?? "";
    if (!DATE_RE.test(date)) {
      return { intents: [], error: `Line ${i + 1}: bad date "${date}" (YYYY-MM-DD).` };
    }
    if (side !== "LONG" && side !== "SHORT" && side !== "FLAT") {
      return { intents: [], error: `Line ${i + 1}: side must be LONG|SHORT|FLAT.` };
    }
    const qty = Number(qtyRaw);
    if (!Number.isFinite(qty) || qty <= 0) {
      return { intents: [], error: `Line ${i + 1}: qty must be a positive number.` };
    }
    intents.push({ date, ticker: ticker.toUpperCase(), side, qty });
  }
  return { intents, error: null };
}

const PRESET_INTENTS = `# date,ticker,side,qty
2024-01-03,AAPL,LONG,100
2024-06-03,AAPL,FLAT,100`;

export function NewBacktestDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  const [tickers, setTickers] = useState("AAPL");
  const [intentSource, setIntentSource] = useState<IntentSource>("explicit");
  const [universeTable, setUniverseTable] = useState("SF1");
  const [start, setStart] = useState("2024-01-02");
  const [end, setEnd] = useState("2024-12-31");
  const [balance, setBalance] = useState("100000");
  const [fillProfile, setFillProfile] = useState<FillProfile>("nautilus-compat");
  const [kind, setKind] = useState("multi-strategy");
  const [intentsText, setIntentsText] = useState(PRESET_INTENTS);
  const [slippageBps, setSlippageBps] = useState("1");
  const [localError, setLocalError] = useState<string | null>(null);

  const create = useCreateBacktest();
  const cancel = useCancelJob();
  const { tracked, track, reset } = useJobTracker();

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

  // On a succeeded run the job result carries the run_id; route to its detail.
  const rawRunId =
    tracked?.status === "succeeded" ? tracked.result?.run_id : undefined;
  const completedRunId =
    typeof rawRunId === "number"
      ? rawRunId
      : typeof rawRunId === "string" && /^\d+$/.test(rawRunId)
        ? Number(rawRunId)
        : undefined;

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

    const bal = Number(balance);
    if (!Number.isFinite(bal) || bal <= 0) {
      setLocalError("Starting balance must be a positive number.");
      return;
    }

    const body: CreateBacktestRequest = {
      start: start.trim(),
      end: end.trim(),
      starting_balance: bal,
      fill_profile: fillProfile,
      strategy: "scripted",
      kind: kind.trim() || "multi-strategy",
      actor: "ui",
    };

    if (intentSource === "universe") {
      body.universe = {
        start: start.trim(),
        end: end.trim(),
        table: universeTable,
      };
    } else {
      if (tickerList.length === 0) {
        setLocalError("Enter at least one ticker, or use a universe window.");
        return;
      }
      body.tickers = tickerList;
      const { intents, error } = parseIntents(intentsText);
      if (error) {
        setLocalError(error);
        return;
      }
      // Keep only intents whose ticker is in the selected list (a stale preset
      // referencing a different symbol would be rejected by the engine).
      const tickerSet = new Set(tickerList);
      const usable = intents.filter((it) => tickerSet.has(it.ticker));
      // If nothing usable remains (e.g. tickers were changed without editing the
      // preset), synthesize a single LONG intent on the first ticker at the
      // window start so a scripted run always has something to trade.
      const effective =
        usable.length > 0
          ? usable
          : [
              {
                date: start.trim(),
                ticker: tickerList[0] as string,
                side: "LONG" as const,
                qty: 100,
              },
            ];
      body.intents = effective;
    }

    if (fillProfile === "realistic") {
      const slip = Number(slippageBps);
      if (Number.isFinite(slip) && slip >= 0) {
        body.realistic = { slippage_bps: slip };
      }
    }

    try {
      const { job } = await create.mutateAsync(body);
      track(job);
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to enqueue backtest.",
      );
    }
  };

  const submitting = create.isPending;

  return (
    <Dialog
      open={open}
      onClose={close}
      title="New backtest"
      description="Run a scripted backtest against the engine. Progress streams live."
      data-testid="backtest-dialog"
      footer={
        tracked ? (
          <>
            {tracked.done && completedRunId != null ? (
              <Link
                href={`/backtests/${completedRunId}`}
                className={buttonVariants({ variant: "default", size: "default" })}
                onClick={() => onClose()}
                data-testid="backtest-detail-link"
              >
                View results
              </Link>
            ) : null}
            {tracked.done ? (
              <Button
                variant="outline"
                onClick={resetForm}
                data-testid="backtest-run-another"
              >
                Run another
              </Button>
            ) : null}
            <Button
              variant={tracked.done && completedRunId != null ? "ghost" : "default"}
              onClick={close}
              data-testid="backtest-dialog-done"
            >
              {tracked.done ? "Close" : "Run in background"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={close} data-testid="backtest-cancel">
              Cancel
            </Button>
            <Button
              onClick={submit}
              disabled={submitting}
              data-testid="backtest-submit"
            >
              {submitting ? "Enqueuing…" : "Run backtest"}
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
          {tracked.done && tracked.status === "succeeded" && completedRunId == null ? (
            <Alert data-testid="backtest-complete-no-id">
              <AlertDescription>
                Backtest finished. Find it in the runs list below.
              </AlertDescription>
            </Alert>
          ) : null}
        </div>
      ) : (
        <div className="space-y-4" data-testid="backtest-form">
          {/* date range + balance */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="bt-start">Start (YYYY-MM-DD)</Label>
              <Input
                id="bt-start"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                placeholder="2024-01-02"
                className="font-mono"
                data-testid="backtest-start"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="bt-end">End (YYYY-MM-DD)</Label>
              <Input
                id="bt-end"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                placeholder="2024-12-31"
                className="font-mono"
                data-testid="backtest-end"
              />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="bt-balance">Starting balance (USD)</Label>
              <Input
                id="bt-balance"
                value={balance}
                onChange={(e) => setBalance(e.target.value)}
                inputMode="decimal"
                className="font-mono"
                data-testid="backtest-balance"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="bt-fill">Fill profile</Label>
              <Select
                id="bt-fill"
                value={fillProfile}
                onChange={(e) => setFillProfile(e.target.value as FillProfile)}
                data-testid="backtest-fill-profile"
              >
                <option value="nautilus-compat">nautilus-compat (parity)</option>
                <option value="realistic">realistic (slippage + next-open)</option>
              </Select>
            </div>
          </div>

          {fillProfile === "realistic" ? (
            <div className="space-y-1.5">
              <Label htmlFor="bt-slippage">Slippage (bps)</Label>
              <Input
                id="bt-slippage"
                value={slippageBps}
                onChange={(e) => setSlippageBps(e.target.value)}
                inputMode="decimal"
                className="max-w-32 font-mono"
                data-testid="backtest-slippage"
              />
            </div>
          ) : null}

          {/* selection source */}
          <div className="space-y-1.5">
            <Label htmlFor="bt-source">Instruments</Label>
            <Select
              id="bt-source"
              value={intentSource}
              onChange={(e) => setIntentSource(e.target.value as IntentSource)}
              data-testid="backtest-source"
            >
              <option value="explicit">Explicit tickers + scripted intents</option>
              <option value="universe">Survivor-bias-free universe window</option>
            </Select>
          </div>

          {intentSource === "explicit" ? (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="bt-tickers">
                  Tickers{" "}
                  <span className="font-normal text-muted-foreground">
                    (space/comma separated)
                  </span>
                </Label>
                <Input
                  id="bt-tickers"
                  value={tickers}
                  onChange={(e) => setTickers(e.target.value)}
                  placeholder="AAPL KO"
                  className="font-mono uppercase"
                  data-testid="backtest-tickers"
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
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="bt-intents">
                  Scripted intents{" "}
                  <span className="font-normal text-muted-foreground">
                    (one per line: date,ticker,side,qty)
                  </span>
                </Label>
                <textarea
                  id="bt-intents"
                  value={intentsText}
                  onChange={(e) => setIntentsText(e.target.value)}
                  spellCheck={false}
                  rows={5}
                  data-testid="backtest-intents"
                  className="cockpit-scroll w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs outline-none transition-colors focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30"
                />
                <button
                  type="button"
                  onClick={() => setIntentsText(PRESET_INTENTS)}
                  className="text-xs text-primary underline-offset-2 hover:underline"
                  data-testid="backtest-intents-preset"
                >
                  Reset to preset
                </button>
              </div>
            </>
          ) : (
            <div className="space-y-1.5">
              <Label htmlFor="bt-universe-table">Universe table</Label>
              <Select
                id="bt-universe-table"
                value={universeTable}
                onChange={(e) => setUniverseTable(e.target.value)}
                data-testid="backtest-universe-table"
              >
                <option value="SF1">SF1 (common stocks)</option>
                <option value="SFP">SFP (ETFs / funds)</option>
              </Select>
              <p className="text-xs text-muted-foreground">
                The engine resolves a point-in-time universe over the run window
                (no survivor bias).
              </p>
            </div>
          )}

          <div className="space-y-1.5">
            <Label htmlFor="bt-kind">Run kind (badge)</Label>
            <Input
              id="bt-kind"
              value={kind}
              onChange={(e) => setKind(e.target.value)}
              className="max-w-56"
              data-testid="backtest-kind"
            />
          </div>

          {localError ? (
            <Alert variant="destructive" data-testid="new-backtest-error">
              <AlertDescription>{localError}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      )}
    </Dialog>
  );
}
