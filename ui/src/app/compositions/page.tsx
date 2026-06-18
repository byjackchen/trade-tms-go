"use client";

import { Suspense, useCallback, useMemo, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Layers } from "lucide-react";
import { StreamIndicator } from "@/components/systems/stream-indicator";
import { cn } from "@/lib/utils";
import { CompositionsList } from "@/components/compositions/compositions-list";
import { CompositionComposer } from "@/components/compositions/composition-composer";
import { NewBacktestDialog } from "@/components/compositions/new-backtest-dialog";
import { RunsTable } from "@/components/compositions/runs-table";
import { BacktestPanel } from "@/components/compositions/backtest-panel";
import { CompositionHyperoptPanel } from "@/components/compositions/composition-hyperopt-panel";
import type { Composition } from "@/lib/api/types";

type Tab = "compositions" | "backtests";

/**
 * The Compositions module (docs/concept-alignment.md §3.4 ③, the COMPOSE + VALIDATE
 * stage). A Composition is a named portfolio blueprint that COMPOSES already-tuned
 * strategies + weights + risk; this page is its control plane:
 *
 *  - Compositions tab: the list + Composer (create/edit), and the per-Composition action
 *    Backtest (POST /backtests composition_id). A Composition never re-tunes params — that
 *    lives in the Strategies module's per-strategy Hyperopt. Backtest results
 *    render INLINE in the BacktestPanel, driven by `?backtest=`.
 *  - Backtests tab: the runs list; selecting a run opens the same inline panel.
 *
 * The retired `/backtests` + `/backtests/[id]` routes 301 here (next.config),
 * preserving the `?backtest=:id` deep-link.
 */
export default function CompositionsPage() {
  return (
    <Suspense fallback={null}>
      <CompositionsBody />
    </Suspense>
  );
}

function CompositionsBody() {
  const router = useRouter();
  const params = useSearchParams();

  const [tab, setTab] = useState<Tab>("compositions");
  const [status, setStatus] = useState("");

  // Composer state (create vs edit).
  const [composerOpen, setComposerOpen] = useState(false);
  const [editComposition, setEditComposition] = useState<Composition | null>(null);

  // Action-dialog state (the Composition the dialog targets).
  const [backtestComposition, setBacktestComposition] = useState<Composition | null>(null);
  // Composition Hyperopt (weights & risk) — the inline panel's target Composition.
  const [hyperoptComposition, setHyperoptComposition] = useState<Composition | null>(null);

  // Inline panels are URL-driven so the redirects' deep-links survive.
  const backtestId = useMemo(() => {
    const raw = params?.get("backtest");
    const n = Number(raw);
    return raw && Number.isInteger(n) && n > 0 ? n : null;
  }, [params]);

  const setParam = useCallback(
    (key: string, value: string | null) => {
      const next = new URLSearchParams(params?.toString() ?? "");
      if (value == null) next.delete(key);
      else next.set(key, value);
      const qs = next.toString();
      router.replace(qs ? `/compositions?${qs}` : "/compositions", { scroll: false });
    },
    [params, router],
  );

  const openBacktest = useCallback(
    (id: number) => setParam("backtest", String(id)),
    [setParam],
  );
  const closeBacktest = useCallback(() => setParam("backtest", null), [setParam]);

  const newComposition = useCallback(() => {
    setEditComposition(null);
    setComposerOpen(true);
  }, []);
  const onEdit = useCallback((m: Composition) => {
    setEditComposition(m);
    setComposerOpen(true);
  }, []);

  return (
    <>
      {/* tab switcher — horizontally scrollable on mobile, with >=44px tap
          targets, so additional tabs never overflow off-screen. The live stream
          indicator (relocated from the retired page-header bar) sits right-aligned
          in the same row. */}
      <nav
        className="flex items-center gap-1 overflow-x-auto border-b border-border px-4 sm:px-6"
        data-testid="compositions-tabs"
        role="tablist"
      >
        {(["compositions", "backtests"] as Tab[]).map((t) => {
          const active = t === tab;
          return (
            <button
              key={t}
              type="button"
              role="tab"
              aria-selected={active}
              data-testid={`compositions-tab-${t}`}
              data-active={active ? "true" : "false"}
              onClick={() => setTab(t)}
              className={cn(
                "min-h-11 shrink-0 border-b-2 px-3 py-2.5 text-sm font-medium capitalize transition-colors sm:min-h-0",
                active
                  ? "border-primary text-foreground"
                  : "border-transparent text-muted-foreground hover:text-foreground",
              )}
            >
              {t === "compositions" ? "Compositions" : "Backtests"}
            </button>
          );
        })}
        <div className="ml-auto shrink-0 pl-2">
          <StreamIndicator />
        </div>
      </nav>

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-6 p-4 sm:p-6"
        data-testid="compositions-page"
        data-active-tab={tab}
      >
        {tab === "compositions" ? (
          <CompositionsList
            onNew={newComposition}
            onEdit={onEdit}
            onBacktest={(m) => setBacktestComposition(m)}
            onHyperopt={(m) => setHyperoptComposition(m)}
          />
        ) : (
          <RunsTable
            status={status}
            onStatusChange={setStatus}
            selectedId={backtestId}
            onSelect={openBacktest}
          />
        )}

        {/* Inline Composition Hyperopt (weights & risk). Distinct from Backtest:
            it TUNES the blueprint's weights + risk (member signal params stay
            fixed), reusing the shared study/trials views. */}
        {hyperoptComposition != null ? (
          <section data-testid="compositions-hyperopt-section">
            <div className="mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              <Layers className="size-3.5" />
              Composition Hyperopt — weights &amp; risk
            </div>
            <CompositionHyperoptPanel
              composition={hyperoptComposition}
              onClose={() => setHyperoptComposition(null)}
            />
          </section>
        ) : null}

        {/* Inline backtest results (deep-linkable via ?backtest=). */}
        {backtestId != null ? (
          <section data-testid="compositions-backtest-panel">
            <div className="mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              <Layers className="size-3.5" />
              Backtest results
            </div>
            <BacktestPanel id={backtestId} onClose={closeBacktest} />
          </section>
        ) : null}
      </main>

      {/* Composer (create/edit). */}
      {composerOpen ? (
        <CompositionComposer
          open
          onClose={() => setComposerOpen(false)}
          composition={editComposition}
        />
      ) : null}

      {/* Backtest a Composition. */}
      {backtestComposition ? (
        <NewBacktestDialog
          open
          onClose={() => setBacktestComposition(null)}
          prefillCompositionId={backtestComposition.id}
          onView={openBacktest}
        />
      ) : null}
    </>
  );
}
