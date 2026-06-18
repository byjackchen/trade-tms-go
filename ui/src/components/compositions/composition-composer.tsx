"use client";

import { useMemo, useState } from "react";
import { Sheet } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { cn } from "@/lib/utils";
import { useCreateComposition, useUpdateComposition } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import {
  COMPOSITION_STRATEGY_IDS,
  type Composition,
  type CompositionMember,
  type CompositionRequest,
  type CompositionRisk,
} from "@/lib/api/types";

/** Human labels for the four canonical strategy ids a member may reference. */
const STRATEGY_LABEL: Record<string, string> = {
  sepa: "SEPA",
  sector_rotation: "Sector Rotation",
  pairs: "Pairs",
  intraday_breakout: "Intraday Breakout (ORB)",
};

/** Default composite risk for a brand-new Composition (the default-multi seed caps). */
const DEFAULT_RISK: CompositionRisk = {
  single_name_pct: 0.5,
  concentration_pct: 0.4,
  daily_loss_halt_pct: 0.1,
  max_gross_pct: null,
  max_positions: null,
};

const SLUG_RE = /^[a-z0-9][a-z0-9-]*$/;

/** A composer-local member row (weight/param_set as edit-friendly strings). */
type MemberDraft = {
  strategy_id: string;
  enabled: boolean; // present in the Composition at all
  active: boolean; // counts toward Σ weights
  weight: string; // percent string, e.g. "40"
  paramSetId: string; // "" ⇒ active params
};

function blankMembers(composition?: Composition): MemberDraft[] {
  return COMPOSITION_STRATEGY_IDS.map((sid) => {
    const existing = composition?.members.find((m) => m.strategy_id === sid);
    return {
      strategy_id: sid,
      enabled: existing != null,
      active: existing?.active ?? true,
      weight:
        existing != null ? String(Math.round(existing.weight * 1000) / 10) : "",
      paramSetId:
        existing?.param_set_id != null ? String(existing.param_set_id) : "",
    };
  });
}

function pctToFraction(s: string): number | null {
  const n = Number(s);
  if (!Number.isFinite(n)) return null;
  return n / 100;
}

/**
 * The Composition Composer — create or edit a named portfolio blueprint
 * (docs/concept-alignment.md §3.4 ③). Pick strategies, set each member's capital
 * weight with a live cash remainder, bind a param_set (default = the strategy's
 * active params), and set Composition-level risk (single_name / concentration /
 * daily_loss_halt). Σ(active weights) + cash ≤ 1 is validated client-side before
 * the create/update mutation fires.
 */
export function CompositionComposer({
  open,
  onClose,
  composition,
  onSaved,
}: {
  open: boolean;
  onClose: () => void;
  /** When set, the composer edits this Composition (PUT); otherwise it creates (POST). */
  composition?: Composition | null;
  /** Called with the saved Composition id after a successful create/update. */
  onSaved?: (id: string) => void;
}) {
  const editing = composition != null;

  const [id, setId] = useState(composition?.id ?? "");
  const [name, setName] = useState(composition?.name ?? "");
  const [description, setDescription] = useState(composition?.description ?? "");
  const [cashPct, setCashPct] = useState(
    composition != null ? String(Math.round(composition.cash_pct * 1000) / 10) : "10",
  );
  const [members, setMembers] = useState<MemberDraft[]>(blankMembers(composition ?? undefined));
  const [risk, setRisk] = useState<CompositionRisk>(composition?.risk ?? DEFAULT_RISK);
  const [localError, setLocalError] = useState<string | null>(null);

  const create = useCreateComposition();
  const update = useUpdateComposition();
  const submitting = create.isPending || update.isPending;

  // Live allocation accounting: Σ(active member weights) + cash. The remainder
  // is the unallocated capital; the form is invalid when it goes negative.
  const cashFrac = pctToFraction(cashPct) ?? 0;
  const activeWeightFrac = useMemo(
    () =>
      members.reduce((sum, m) => {
        if (!m.enabled || !m.active) return sum;
        return sum + (pctToFraction(m.weight) ?? 0);
      }, 0),
    [members],
  );
  const allocated = activeWeightFrac + cashFrac;
  const remainder = 1 - allocated;
  const overAllocated = remainder < -1e-9;

  const setMember = (sid: string, patch: Partial<MemberDraft>) => {
    setMembers((prev) =>
      prev.map((m) => (m.strategy_id === sid ? { ...m, ...patch } : m)),
    );
    setLocalError(null);
  };

  const setRiskField = (key: keyof CompositionRisk, value: string) => {
    const n = value.trim() === "" ? null : Number(value);
    setRisk((prev) => ({ ...prev, [key]: n }));
    setLocalError(null);
  };

  const close = () => {
    setLocalError(null);
    onClose();
  };

  const submit = async () => {
    setLocalError(null);

    const slug = id.trim();
    if (!slug) {
      setLocalError("Id (slug) is required.");
      return;
    }
    if (!SLUG_RE.test(slug)) {
      setLocalError("Id must be a slug: lowercase letters, digits and dashes.");
      return;
    }
    if (!name.trim()) {
      setLocalError("Name is required.");
      return;
    }

    if (cashFrac < 0 || cashFrac >= 1) {
      setLocalError("Cash reserve must be in [0, 100).");
      return;
    }

    const enabled = members.filter((m) => m.enabled);
    if (enabled.length === 0) {
      setLocalError("Add at least one strategy member.");
      return;
    }

    const wireMembers: CompositionMember[] = [];
    for (const m of enabled) {
      const w = pctToFraction(m.weight);
      if (w == null || w <= 0 || w > 1) {
        setLocalError(
          `${STRATEGY_LABEL[m.strategy_id] ?? m.strategy_id}: weight must be in (0, 100].`,
        );
        return;
      }
      let paramSetId: number | null = null;
      if (m.paramSetId.trim() !== "") {
        const n = Number(m.paramSetId);
        if (!Number.isInteger(n) || n <= 0) {
          setLocalError(
            `${STRATEGY_LABEL[m.strategy_id] ?? m.strategy_id}: param_set id must be a positive integer (or blank for active params).`,
          );
          return;
        }
        paramSetId = n;
      }
      wireMembers.push({
        strategy_id: m.strategy_id,
        weight: w,
        active: m.active,
        param_set_id: paramSetId,
      });
    }

    // Client-side guard mirroring the server's Composition.Validate: Σ(active weights)
    // + cash ≤ 1 (docs/concept-alignment.md §1.2, §3.1).
    if (overAllocated) {
      setLocalError(
        `Σ(active weights) + cash = ${(allocated * 100).toFixed(1)}% > 100%. Reduce weights or cash.`,
      );
      return;
    }

    // Validate the three required risk caps are in (0, 1].
    const reqCaps: [keyof CompositionRisk, string][] = [
      ["single_name_pct", "Single-name cap"],
      ["concentration_pct", "Concentration cap"],
      ["daily_loss_halt_pct", "Daily-loss halt"],
    ];
    for (const [key, label] of reqCaps) {
      const v = risk[key];
      if (typeof v !== "number" || !Number.isFinite(v) || v <= 0 || v > 1) {
        setLocalError(`${label} must be in (0, 1] (e.g. 0.5).`);
        return;
      }
    }
    if (
      risk.max_gross_pct != null &&
      (!Number.isFinite(risk.max_gross_pct) || risk.max_gross_pct <= 0)
    ) {
      setLocalError("Max gross, when set, must be > 0.");
      return;
    }
    if (
      risk.max_positions != null &&
      (!Number.isInteger(risk.max_positions) || risk.max_positions <= 0)
    ) {
      setLocalError("Max positions, when set, must be a positive integer.");
      return;
    }

    const body: CompositionRequest = {
      id: slug,
      name: name.trim(),
      description: description.trim(),
      cash_pct: cashFrac,
      risk: {
        single_name_pct: risk.single_name_pct,
        concentration_pct: risk.concentration_pct,
        daily_loss_halt_pct: risk.daily_loss_halt_pct,
        max_gross_pct: risk.max_gross_pct ?? null,
        max_positions: risk.max_positions ?? null,
      },
      members: wireMembers,
      actor: "ui",
    };
    if (editing && composition) {
      body.version = composition.version;
    }

    try {
      if (editing) {
        await update.mutateAsync({ id: slug, body });
      } else {
        await create.mutateAsync(body);
      }
      onSaved?.(slug);
      onClose();
    } catch (err) {
      setLocalError(
        err instanceof ApiError ? err.message : "Failed to save Composition.",
      );
    }
  };

  return (
    <Sheet
      open={open}
      onClose={close}
      title={editing ? `Edit Composition — ${composition?.name}` : "New Composition"}
      description="A named portfolio blueprint: strategies + weights + param refs + composite risk. Σ(active weights) + cash ≤ 100%."
      data-testid="composition-composer"
      className="ui-desktop:w-[min(52rem,calc(100vw-2rem))]"
      footer={
        <>
          <Button variant="ghost" onClick={close} data-testid="composer-cancel">
            Cancel
          </Button>
          <Button onClick={submit} disabled={submitting} data-testid="composer-submit">
            {submitting ? "Saving…" : editing ? "Save Composition" : "Create Composition"}
          </Button>
        </>
      }
    >
      <div className="space-y-5" data-testid="composer-form">
        {/* identity */}
        <div className="grid gap-3 grid-cols-2 ui-mobile:grid-cols-1">
          <div className="space-y-1.5">
            <Label htmlFor="mc-id">Id (slug)</Label>
            <Input
              id="mc-id"
              value={id}
              onChange={(e) => {
                setId(e.target.value);
                setLocalError(null);
              }}
              placeholder="sepa-pairs-7030"
              className="font-mono"
              disabled={editing}
              data-testid="composer-id"
            />
            {editing ? (
              <p className="text-xs text-muted-foreground">
                The id is immutable; create a new Composition to fork.
              </p>
            ) : null}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="mc-name">Name</Label>
            <Input
              id="mc-name"
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                setLocalError(null);
              }}
              placeholder="SEPA + Pairs 70/30"
              data-testid="composer-name"
            />
          </div>
        </div>

        <div className="space-y-1.5">
          <Label htmlFor="mc-desc">Description</Label>
          <Input
            id="mc-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="What this blueprint is for"
            data-testid="composer-description"
          />
        </div>

        {/* members */}
        <div className="space-y-2" data-testid="composer-members">
          <div className="flex items-center justify-between">
            <Label>Members</Label>
            <span
              className="text-xs text-muted-foreground"
              data-testid="composer-allocation-hint"
            >
              Per-member capital weight; toggle active to count it.
            </span>
          </div>
          <div className="space-y-2">
            {members.map((m) => (
              <div
                key={m.strategy_id}
                data-testid={`composer-member-${m.strategy_id}`}
                data-enabled={m.enabled ? "true" : "false"}
                className={cn(
                  "rounded-lg border border-border p-3",
                  !m.enabled && "opacity-60",
                )}
              >
                <div className="flex flex-wrap items-center gap-3">
                  <label className="flex items-center gap-2 text-sm font-medium select-none">
                    <Checkbox
                      checked={m.enabled}
                      onChange={(e) =>
                        setMember(m.strategy_id, { enabled: e.target.checked })
                      }
                      data-testid={`member-enabled-${m.strategy_id}`}
                    />
                    {STRATEGY_LABEL[m.strategy_id] ?? m.strategy_id}
                  </label>

                  {m.enabled ? (
                    <>
                      <div className="flex items-center gap-1.5">
                        <Label
                          htmlFor={`mc-weight-${m.strategy_id}`}
                          className="text-xs text-muted-foreground"
                        >
                          Weight %
                        </Label>
                        <Input
                          id={`mc-weight-${m.strategy_id}`}
                          value={m.weight}
                          onChange={(e) =>
                            setMember(m.strategy_id, { weight: e.target.value })
                          }
                          inputMode="decimal"
                          placeholder="40"
                          className="w-20 font-mono h-8 ui-mobile:h-11"
                          data-testid={`member-weight-${m.strategy_id}`}
                        />
                      </div>

                      <label className="flex items-center gap-1.5 text-xs text-muted-foreground select-none">
                        <Checkbox
                          checked={m.active}
                          onChange={(e) =>
                            setMember(m.strategy_id, { active: e.target.checked })
                          }
                          data-testid={`member-active-${m.strategy_id}`}
                        />
                        Active
                      </label>

                      <div className="flex items-center gap-1.5">
                        <Label
                          htmlFor={`mc-param-${m.strategy_id}`}
                          className="text-xs text-muted-foreground"
                        >
                          param_set id
                        </Label>
                        <Input
                          id={`mc-param-${m.strategy_id}`}
                          value={m.paramSetId}
                          onChange={(e) =>
                            setMember(m.strategy_id, { paramSetId: e.target.value })
                          }
                          inputMode="numeric"
                          placeholder="active"
                          className="w-24 font-mono h-8 ui-mobile:h-11"
                          data-testid={`member-paramset-${m.strategy_id}`}
                        />
                      </div>
                      {m.paramSetId.trim() === "" ? (
                        <Badge variant="muted" className="text-[10px]">
                          active params
                        </Badge>
                      ) : null}
                    </>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        </div>

        {/* cash + live remainder */}
        <div
          className="grid items-end gap-4 grid-cols-[10rem_1fr] ui-mobile:grid-cols-1"
        >
          <div className="space-y-1.5">
            <Label htmlFor="mc-cash">Cash reserve %</Label>
            <Input
              id="mc-cash"
              value={cashPct}
              onChange={(e) => {
                setCashPct(e.target.value);
                setLocalError(null);
              }}
              inputMode="decimal"
              className="font-mono"
              data-testid="composer-cash"
            />
          </div>
          <div
            className={cn(
              "rounded-lg border p-3 text-sm",
              overAllocated
                ? "border-destructive/40 bg-destructive/10"
                : "border-border bg-muted/40",
            )}
            data-testid="composer-remainder"
            data-over={overAllocated ? "true" : "false"}
            data-remainder={remainder}
          >
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground">
                Allocated (active weights + cash)
              </span>
              <span className="font-mono tabular-nums">
                {(allocated * 100).toFixed(1)}%
              </span>
            </div>
            <div className="mt-1 flex items-center justify-between">
              <span className="text-muted-foreground">Cash remainder</span>
              <span
                className={cn(
                  "font-mono font-medium tabular-nums",
                  overAllocated && "text-destructive",
                )}
                data-testid="composer-remainder-value"
              >
                {(remainder * 100).toFixed(1)}%
              </span>
            </div>
            {overAllocated ? (
              <p className="mt-1 text-xs text-destructive">
                Over-allocated — Σ(active weights) + cash must be ≤ 100%.
              </p>
            ) : null}
          </div>
        </div>

        {/* risk */}
        <div className="space-y-2" data-testid="composer-risk">
          <Label>Composition-level risk (fractions in (0, 1])</Label>
          <div className="grid gap-3 grid-cols-3 ui-mobile:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="mc-risk-single" className="text-xs text-muted-foreground">
                Single name
              </Label>
              <Input
                id="mc-risk-single"
                value={risk.single_name_pct ?? ""}
                onChange={(e) => setRiskField("single_name_pct", e.target.value)}
                inputMode="decimal"
                placeholder="0.5"
                className="font-mono"
                data-testid="risk-single-name"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mc-risk-conc" className="text-xs text-muted-foreground">
                Concentration
              </Label>
              <Input
                id="mc-risk-conc"
                value={risk.concentration_pct ?? ""}
                onChange={(e) => setRiskField("concentration_pct", e.target.value)}
                inputMode="decimal"
                placeholder="0.4"
                className="font-mono"
                data-testid="risk-concentration"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mc-risk-halt" className="text-xs text-muted-foreground">
                Daily-loss halt
              </Label>
              <Input
                id="mc-risk-halt"
                value={risk.daily_loss_halt_pct ?? ""}
                onChange={(e) => setRiskField("daily_loss_halt_pct", e.target.value)}
                inputMode="decimal"
                placeholder="0.1"
                className="font-mono"
                data-testid="risk-daily-loss-halt"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mc-risk-gross" className="text-xs text-muted-foreground">
                Max gross (optional)
              </Label>
              <Input
                id="mc-risk-gross"
                value={risk.max_gross_pct ?? ""}
                onChange={(e) => setRiskField("max_gross_pct", e.target.value)}
                inputMode="decimal"
                placeholder="—"
                className="font-mono"
                data-testid="risk-max-gross"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mc-risk-pos" className="text-xs text-muted-foreground">
                Max positions (optional)
              </Label>
              <Input
                id="mc-risk-pos"
                value={risk.max_positions ?? ""}
                onChange={(e) => setRiskField("max_positions", e.target.value)}
                inputMode="numeric"
                placeholder="—"
                className="font-mono"
                data-testid="risk-max-positions"
              />
            </div>
          </div>
        </div>

        {localError ? (
          <Alert variant="destructive" data-testid="composer-error">
            <AlertDescription>{localError}</AlertDescription>
          </Alert>
        ) : null}
      </div>
    </Sheet>
  );
}
