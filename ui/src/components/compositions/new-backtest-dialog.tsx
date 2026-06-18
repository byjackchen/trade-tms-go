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
import { useCreateBacktest, useCancelJob, useCompositions } from "@/lib/api/hooks";
import { useJobTracker } from "@/lib/api/use-job-tracker";
import { ApiError } from "@/lib/api/client";
import type {
  CreateBacktestRequest,
  FillProfile,
  BacktestIntent,
  BacktestStrategy,
  Composition,
} from "@/lib/api/types";

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

type IntentSource = "explicit" | "universe";

/** The scripted (composition-less) option value used in the composition selector. */
const SCRIPTED = "__scripted__";

/**
 * The strategy ids that drive the per-member universe controls. A Composition with a
 * SEPA member can take an explicit stock universe; one with an ORB member needs
 * a single intraday instrument.
 */
const STRATEGY_SEPA = "sepa";
const STRATEGY_ORB = "intraday_breakout";

/**
 * Legacy strategy → seed Composition id (docs/concept-alignment.md seeds). The
 * Strategies module still launches a single-strategy backtest by passing
 * `prefillStrategy`; we resolve it onto the corresponding single-member seed
 * Composition so the request always carries a `composition_id`. "scripted" stays composition-less.
 */
const LEGACY_COMPOSITION_BY_STRATEGY: Record<string, string> = {
  sepa: "sepa-only",
  sector_rotation: "sector-only",
  pairs: "pairs-only",
  orb: "orb-only",
  multi: "default-multi",
};

/**
 * Parse the intents textarea. One intent per line:
 *   YYYY-MM-DD,TICKER,SIDE,QTY   (commas or whitespace separated)
 * Blank lines and `#` comments are ignored.
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
  prefillStrategy,
  prefillCompositionId,
  onView,
}: {
  open: boolean;
  onClose: () => void;
  /**
   * LEGACY: the Strategies module passes a strategy token (sepa|…|multi) to
   * launch a single-strategy backtest. It is resolved onto the matching seed
   * Composition id (LEGACY_COMPOSITION_BY_STRATEGY). A backtest's object is always a Composition.
   */
  prefillStrategy?: BacktestStrategy;
  /** Preselect a specific Composition id (the Compositions module passes the open Composition). */
  prefillCompositionId?: string;
  /** Open the freshly-completed run in the inline backtest panel. */
  onView?: (id: number) => void;
}) {
  const grid2 = "grid gap-3 grid-cols-2 ui-mobile:grid-cols-1";

  const { data: compositionsData } = useCompositions();
  const compositions = useMemo<Composition[]>(() => compositionsData?.compositions ?? [], [compositionsData]);

  // Resolve the initial Composition selection: an explicit prefillCompositionId wins, else a
  // legacy strategy token maps to its seed Composition, else scripted.
  const initialCompositionId = useMemo(() => {
    if (prefillCompositionId) return prefillCompositionId;
    if (prefillStrategy === "scripted") return SCRIPTED;
    if (prefillStrategy) return LEGACY_COMPOSITION_BY_STRATEGY[prefillStrategy] ?? SCRIPTED;
    return SCRIPTED;
  }, [prefillCompositionId, prefillStrategy]);

  const [compositionId, setCompositionId] = useState<string>(initialCompositionId);
  // The universe is the FULL survivor-bias-free SF1 set by default — a SEPA
  // backtest scans thousands of names to find the few that break out, so a single
  // hand-picked ticker is a degenerate run. An explicit ticker list is OPTIONAL
  // (blank by default); scripted runs still default to explicit (they need their
  // own intents).
  const [tickers, setTickers] = useState("");
  const [orbSymbol, setOrbSymbol] = useState("SPY");
  const [intentSource, setIntentSource] = useState<IntentSource>(
    initialCompositionId === SCRIPTED ? "explicit" : "universe",
  );
  const [universeTable, setUniverseTable] = useState("SF1");
  const [start, setStart] = useState("2024-01-02");
  const [end, setEnd] = useState("2024-12-31");
  const [balance, setBalance] = useState("100000");
  const [fillProfile, setFillProfile] = useState<FillProfile>("realistic");
  const [kind, setKind] = useState("composition-backtest");
  const [intentsText, setIntentsText] = useState(PRESET_INTENTS);
  const [slippageBps, setSlippageBps] = useState("1");
  const [localError, setLocalError] = useState<string | null>(null);

  const create = useCreateBacktest();
  const cancel = useCancelJob();
  const { tracked, track, reset } = useJobTracker();

  const selectedComposition = useMemo(
    () => compositions.find((m) => m.id === compositionId) ?? null,
    [compositions, compositionId],
  );
  const isScripted = compositionId === SCRIPTED;

  const hasSEPA = useMemo(
    () => selectedComposition?.members.some((m) => m.strategy_id === STRATEGY_SEPA) ?? false,
    [selectedComposition],
  );
  const hasORB = useMemo(
    () => selectedComposition?.members.some((m) => m.strategy_id === STRATEGY_ORB) ?? false,
    [selectedComposition],
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

  // On a succeeded run the job result carries the run_id.
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

    // A backtest's object is always a Composition (concept-alignment §3.3, A3): the
    // request carries composition_id. The only exception is the scripted-intents path,
    // which bypasses strategy assembly entirely.
    const body: CreateBacktestRequest = {
      start: start.trim(),
      end: end.trim(),
      starting_balance: bal,
      fill_profile: fillProfile,
      kind: kind.trim() || "composition-backtest",
      actor: "ui",
    };
    if (!isScripted) {
      body.composition_id = compositionId;
    }

    if (isScripted) {
      // Scripted runs need an explicit universe + (optionally) intents.
      if (intentSource === "universe") {
        body.universe = { start: start.trim(), end: end.trim(), table: universeTable };
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
        const tickerSet = new Set(tickerList);
        const usable = intents.filter((it) => tickerSet.has(it.ticker));
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
    } else if (hasORB) {
      // ORB trades a single intraday instrument; engine accepts orb_symbol.
      const sym = orbSymbol.trim().toUpperCase();
      if (!sym) {
        setLocalError("This Composition has an ORB member — supply an instrument symbol (e.g. SPY).");
        return;
      }
      body.orb_symbol = sym;
    } else if (hasSEPA) {
      // A SEPA member optionally takes a stock universe (ticker list or window);
      // omitting both lets the engine resolve its own point-in-time universe.
      if (intentSource === "universe") {
        body.universe = { start: start.trim(), end: end.trim(), table: universeTable };
      } else if (tickerList.length > 0) {
        body.tickers = tickerList;
      }
    }
    // sector_rotation / pairs derive their instruments from params: nothing to add.

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
    <Sheet
      open={open}
      onClose={close}
      title="New backtest"
      description="Backtest a Composition against the engine. A run's object is always a Composition; progress streams live."
      data-testid="backtest-dialog"
      footer={
        tracked ? (
          <>
            {tracked.done && completedRunId != null ? (
              <Button
                onClick={() => {
                  onView?.(completedRunId);
                  onClose();
                }}
                data-testid="backtest-detail-link"
              >
                View results
              </Button>
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
          {/* Composition selector */}
          <div className="space-y-1.5">
            <Label htmlFor="bt-composition">Composition</Label>
            <Select
              id="bt-composition"
              value={compositionId}
              onChange={(e) => {
                setCompositionId(e.target.value);
                setLocalError(null);
              }}
              data-testid="backtest-composition"
            >
              {compositions.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name} ({m.id})
                </option>
              ))}
              <option value={SCRIPTED}>Scripted (manual intents — no Composition)</option>
            </Select>
            <p className="text-xs text-muted-foreground" data-testid="backtest-composition-hint">
              {isScripted
                ? "Replays manual trade intents — no signal logic, no Composition."
                : selectedComposition
                  ? `${selectedComposition.members.filter((m) => m.active).length} active member(s); the engine drops in this blueprint.`
                  : "Pick a Composition to backtest, or the scripted path."}
            </p>
          </div>

          {/* date range + balance */}
          <div className={grid2}>
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

          <div className={grid2}>
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
                <option value="realistic">realistic (slippage + next-open)</option>
                <option value="close-fill">close-fill (same-bar close, zero cost)</option>
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

          {/* ORB member: single intraday instrument */}
          {!isScripted && hasORB ? (
            <div className="space-y-1.5" data-testid="backtest-orb-section">
              <Label htmlFor="bt-orb-symbol">ORB instrument symbol</Label>
              <Input
                id="bt-orb-symbol"
                value={orbSymbol}
                onChange={(e) => setOrbSymbol(e.target.value)}
                placeholder="SPY"
                className="max-w-40 font-mono uppercase"
                data-testid="backtest-orb-symbol"
              />
              <p className="text-xs text-muted-foreground">
                This Composition has an ORB member — it trades one instrument intraday
                and is flat by EOD.
              </p>
            </div>
          ) : null}

          {/* sector_rotation / pairs only: params-derived universe note */}
          {!isScripted && !hasSEPA && !hasORB && selectedComposition ? (
            <Alert data-testid="backtest-derived-universe">
              <AlertDescription>
                This Composition&apos;s members resolve their instruments from the active
                params — there is no universe to select.
              </AlertDescription>
            </Alert>
          ) : null}

          {/* scripted / SEPA: explicit equity universe */}
          {isScripted || hasSEPA ? (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="bt-source">Instruments</Label>
                <Select
                  id="bt-source"
                  value={intentSource}
                  onChange={(e) => setIntentSource(e.target.value as IntentSource)}
                  data-testid="backtest-source"
                >
                  <option value="explicit">
                    {isScripted ? "Explicit tickers + scripted intents" : "Explicit tickers"}
                  </option>
                  <option value="universe">Survivor-bias-free universe window</option>
                </Select>
                {!isScripted ? (
                  <p className="text-xs text-muted-foreground">
                    {intentSource === "universe"
                      ? "Default: SEPA scans the FULL survivor-bias-free SF1 universe over the window — the few names that break out become trades. This is the representative SEPA backtest."
                      : "Optional: restrict SEPA to a hand-picked ticker list. A single name is usually too narrow — SEPA finds setups by scanning the whole universe, so explicit tickers are for targeted studies only."}
                  </p>
                ) : null}
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

                  {isScripted ? (
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
                  ) : null}
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
                    The engine resolves a point-in-time universe over the run
                    window (no survivor bias).
                  </p>
                </div>
              )}
            </>
          ) : null}

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
    </Sheet>
  );
}
