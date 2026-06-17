"use client";

import { Suspense, useCallback, useMemo, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Layers } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { StreamIndicator } from "@/components/systems/stream-indicator";
import { cn } from "@/lib/utils";
import { ModelsList } from "@/components/models/models-list";
import { ModelComposer } from "@/components/models/model-composer";
import { NewBacktestDialog } from "@/components/models/new-backtest-dialog";
import { RunsTable } from "@/components/models/runs-table";
import { BacktestPanel } from "@/components/models/backtest-panel";
import type { Model } from "@/lib/api/types";

type Tab = "models" | "backtests";

/**
 * The Models module (docs/concept-alignment.md §3.4 ③, the COMPOSE + VALIDATE
 * stage). A Model is a named portfolio blueprint that COMPOSES already-tuned
 * strategies + weights + risk; this page is its control plane:
 *
 *  - Models tab: the list + Composer (create/edit), and the per-Model action
 *    Backtest (POST /backtests model_id). A Model never re-tunes params — that
 *    lives in the Strategies module's per-strategy Hyperopt. Backtest results
 *    render INLINE in the BacktestPanel, driven by `?backtest=`.
 *  - Backtests tab: the runs list; selecting a run opens the same inline panel.
 *
 * The retired `/backtests` + `/backtests/[id]` routes 301 here (next.config),
 * preserving the `?backtest=:id` deep-link.
 */
export default function ModelsPage() {
  return (
    <Suspense fallback={null}>
      <ModelsBody />
    </Suspense>
  );
}

function ModelsBody() {
  const router = useRouter();
  const params = useSearchParams();

  const [tab, setTab] = useState<Tab>("models");
  const [status, setStatus] = useState("");

  // Composer state (create vs edit).
  const [composerOpen, setComposerOpen] = useState(false);
  const [editModel, setEditModel] = useState<Model | null>(null);

  // Action-dialog state (the Model the dialog targets).
  const [backtestModel, setBacktestModel] = useState<Model | null>(null);

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
      router.replace(qs ? `/models?${qs}` : "/models", { scroll: false });
    },
    [params, router],
  );

  const openBacktest = useCallback(
    (id: number) => setParam("backtest", String(id)),
    [setParam],
  );
  const closeBacktest = useCallback(() => setParam("backtest", null), [setParam]);

  const newModel = useCallback(() => {
    setEditModel(null);
    setComposerOpen(true);
  }, []);
  const onEdit = useCallback((m: Model) => {
    setEditModel(m);
    setComposerOpen(true);
  }, []);

  return (
    <>
      <PageHeader
        title="Models"
        subtitle="Compose named portfolio blueprints from tuned strategies, then validate them by backtest."
        data-testid="models-header"
        actions={<StreamIndicator />}
      />

      {/* tab switcher */}
      <nav
        className="flex items-center gap-1 border-b border-border px-6"
        data-testid="models-tabs"
        role="tablist"
      >
        {(["models", "backtests"] as Tab[]).map((t) => {
          const active = t === tab;
          return (
            <button
              key={t}
              type="button"
              role="tab"
              aria-selected={active}
              data-testid={`models-tab-${t}`}
              data-active={active ? "true" : "false"}
              onClick={() => setTab(t)}
              className={cn(
                "border-b-2 px-3 py-2.5 text-sm font-medium capitalize transition-colors",
                active
                  ? "border-primary text-foreground"
                  : "border-transparent text-muted-foreground hover:text-foreground",
              )}
            >
              {t === "models" ? "Models" : "Backtests"}
            </button>
          );
        })}
      </nav>

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-6 p-6"
        data-testid="models-page"
        data-active-tab={tab}
      >
        {tab === "models" ? (
          <ModelsList
            onNew={newModel}
            onEdit={onEdit}
            onBacktest={(m) => setBacktestModel(m)}
          />
        ) : (
          <RunsTable
            status={status}
            onStatusChange={setStatus}
            selectedId={backtestId}
            onSelect={openBacktest}
          />
        )}

        {/* Inline backtest results (deep-linkable via ?backtest=). */}
        {backtestId != null ? (
          <section data-testid="models-backtest-panel">
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
        <ModelComposer
          open
          onClose={() => setComposerOpen(false)}
          model={editModel}
        />
      ) : null}

      {/* Backtest a Model. */}
      {backtestModel ? (
        <NewBacktestDialog
          open
          onClose={() => setBacktestModel(null)}
          prefillModelId={backtestModel.id}
          onView={openBacktest}
        />
      ) : null}
    </>
  );
}
