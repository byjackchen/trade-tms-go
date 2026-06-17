"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "next/navigation";
import { ArrowDownLeft, ArrowUpRight, Send, ShieldAlert } from "lucide-react";
import { useManualOrder } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Checkbox } from "@/components/ui/checkbox";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  MANUAL_LIVE_CONFIRM_PHRASE,
  type ManualOrderRequest,
  type ManualOrderType,
  type ManualSide,
} from "@/lib/api/types";
import { useTradeDesk } from "./trade-desk-context";

/**
 * The manual ORDER TICKET — the operator's discretionary order-entry form, the
 * single client of POST /api/v1/trade/order.
 *
 * SAFETY (paramount — this can place REAL orders):
 *   - The per-order `confirm_token` is ALWAYS required inline before submit is
 *     enabled. In PAPER mode it is the trade password; once the desk is LIVE-armed
 *     (via the guarded mode-live switch) it is the exact phrase
 *     `I CONFIRM THIS REAL MONEY MANUAL ORDER`. Submit is disabled until the token
 *     is present (and, when live, until it matches the phrase exactly).
 *   - The server is the AUTHORITATIVE gate. A missing/wrong token returns 412
 *     (`confirmation_required`) and NO order is placed; a risk-gate violation
 *     returns 422 (`risk_violation`) and the order is rejected unless the operator
 *     checks `override` (an explicit, audited decision) and re-submits. We surface
 *     the server's verdict verbatim inline. There is NO path to a real order
 *     without the full server gate.
 *
 * Idempotency: each ticket carries a fresh `idempotency_key`; re-submitting the
 * SAME ticket (e.g. after checking override) reuses the key, so the broker never
 * double-submits.
 */
export function OrderTicket({ liveArmed }: { liveArmed: boolean }) {
  const place = useManualOrder();
  const desk = useTradeDesk();
  const search = useSearchParams();

  // ---- ticket fields ----
  const [symbol, setSymbol] = useState("");
  const [side, setSide] = useState<ManualSide>("BUY");
  const [qty, setQty] = useState("");
  const [type, setType] = useState<ManualOrderType>("MARKET");
  const [limitPrice, setLimitPrice] = useState("");
  const [override, setOverride] = useState(false);
  const [reason, setReason] = useState("");
  const [confirmToken, setConfirmToken] = useState("");

  // ---- flow ----
  // The latest 422 risk violation (rule + message) shown inline; cleared on a
  // successful submit or any field edit that changes the order.
  const [violation, setViolation] = useState<{ rule: string; message: string } | null>(
    null,
  );
  const [placed, setPlaced] = useState<{ coid: string; submitted: boolean } | null>(
    null,
  );
  const idemRef = useRef<string>(freshKey());
  const [pulse, setPulse] = useState(false);

  const applyPrefill = useCallback(
    (sym: string, sd: ManualSide) => {
      setSymbol(sym.toUpperCase());
      setSide(sd);
      // A fresh ticket: clear the prior key + transient flow so the prefilled
      // order is a clean placement (no stale confirm/override carried over).
      idemRef.current = freshKey();
      setViolation(null);
      setPlaced(null);
      setOverride(false);
      setConfirmToken("");
      place.reset();
      setPulse(true);
      window.setTimeout(() => setPulse(false), 1200);
    },
    [place],
  );

  // Prefill from the in-page bus (positions Trade buttons).
  const prefillSeq = desk.prefill?.seq;
  const prefillSym = desk.prefill?.symbol;
  const prefillSide = desk.prefill?.side;
  useEffect(() => {
    if (!prefillSym || !prefillSide) return;
    // eslint-disable-next-line react-hooks/set-state-in-effect
    applyPrefill(prefillSym, prefillSide);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [prefillSeq]);

  // Prefill from the URL (?symbol=&side=) — the watchlist Trade button links here.
  useEffect(() => {
    const s = search.get("symbol");
    const sd = (search.get("side") ?? "").toUpperCase();
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (s) applyPrefill(s, sd === "SELL" ? "SELL" : "BUY");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function resetTicket() {
    setSymbol("");
    setQty("");
    setLimitPrice("");
    setReason("");
    setOverride(false);
    setConfirmToken("");
    setType("MARKET");
    setSide("BUY");
    idemRef.current = freshKey();
    setViolation(null);
    setPlaced(null);
    place.reset();
  }

  // ---- validation (client-side; the server re-validates) ----
  const qtyNum = Number(qty);
  const limitNum = Number(limitPrice);
  const symbolOk = symbol.trim().length > 0;
  const qtyOk = Number.isFinite(qtyNum) && qtyNum > 0 && Number.isInteger(qtyNum);
  const limitOk = type !== "LIMIT" || (Number.isFinite(limitNum) && limitNum > 0);
  // The confirm token must be present; when LIVE-armed it must match the exact
  // phrase before we even allow a submit (defense in depth on top of the server).
  const tokenPresent = confirmToken.trim().length > 0;
  const tokenOk = liveArmed
    ? confirmToken.trim() === MANUAL_LIVE_CONFIRM_PHRASE
    : tokenPresent;
  const formOk = symbolOk && qtyOk && limitOk && tokenOk && !place.isPending;

  function buildBody(): ManualOrderRequest {
    const body: ManualOrderRequest = {
      idempotency_key: idemRef.current,
      symbol: symbol.trim().toUpperCase(),
      side,
      qty: qtyNum,
      type,
      confirm_token: confirmToken.trim(),
    };
    if (type === "LIMIT") body.limit_price = limitNum;
    if (override) body.override = true;
    if (reason.trim()) body.reason = reason.trim();
    return body;
  }

  function submit() {
    if (!formOk) return;
    setPlaced(null);
    place.mutate(buildBody(), {
      onSuccess: (res) => {
        setViolation(null);
        setPlaced({ coid: res.client_order_id, submitted: res.submitted });
      },
      onError: (err) => {
        // Only a RISK gate violation (422 risk_violation) is overridable — show the
        // inline violation banner with the override affordance. A broker BUSINESS
        // rejection is ALSO 422 (order_rejected: buying power / market closed /
        // unknown symbol) but is NOT overridable; it falls through to the generic
        // server-error alert (no override would help). 412 / 400 / 503 / other also
        // stay in the generic alert.
        if (
          err instanceof ApiError &&
          err.status === 422 &&
          err.code === "risk_violation"
        ) {
          setViolation({ rule: extractRule(err.message), message: err.message });
          return;
        }
        setViolation(null);
      },
    });
  }

  // Any change to the order shape invalidates a prior violation (it referred to a
  // different order) and a prior placement.
  function onOrderFieldChange<T>(setter: (v: T) => void) {
    return (v: T) => {
      setViolation(null);
      setPlaced(null);
      setter(v);
    };
  }

  // Show a generic server-error alert for everything EXCEPT the overridable risk
  // violation (which has its own inline banner). A 422 order_rejected (broker
  // business rejection) is surfaced here — it is not overridable.
  const serverError =
    place.error instanceof ApiError &&
    !(place.error.status === 422 && place.error.code === "risk_violation")
      ? `${place.error.code}: ${place.error.message}`
      : null;

  const SideIcon = side === "BUY" ? ArrowUpRight : ArrowDownLeft;

  return (
    <Card
      data-testid="manual-ticket"
      data-live-armed={liveArmed ? "true" : "false"}
      className={
        pulse
          ? "ring-2 ring-primary/60 transition-shadow"
          : liveArmed
            ? "border-destructive/50"
            : undefined
      }
    >
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          Order ticket
          <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] uppercase text-muted-foreground">
            MANUAL
          </span>
          {liveArmed ? (
            <span className="rounded bg-destructive/15 px-1.5 py-0.5 font-mono text-[10px] uppercase text-destructive">
              LIVE armed
            </span>
          ) : null}
        </CardTitle>
        <span className="text-xs text-muted-foreground">
          Discretionary operator order — attributed to the MANUAL book.
        </span>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Row 1: symbol + side */}
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label htmlFor="manual-symbol">Symbol</Label>
            <Input
              id="manual-symbol"
              value={symbol}
              onChange={(e) =>
                onOrderFieldChange(setSymbol)(e.target.value.toUpperCase())
              }
              placeholder="AAPL"
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              data-testid="manual-ticket-symbol"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="manual-side">Side</Label>
            <Select
              id="manual-side"
              value={side}
              onChange={(e) =>
                onOrderFieldChange(setSide)(e.target.value as ManualSide)
              }
              data-testid="manual-ticket-side"
              data-side={side}
              className={
                side === "BUY"
                  ? "text-emerald-700 dark:text-emerald-400"
                  : "text-red-700 dark:text-red-400"
              }
            >
              <option value="BUY" data-testid="manual-ticket-side-buy">
                BUY
              </option>
              <option value="SELL" data-testid="manual-ticket-side-sell">
                SELL
              </option>
            </Select>
          </div>
        </div>

        {/* Row 2: qty + type */}
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label htmlFor="manual-qty">Quantity (shares)</Label>
            <Input
              id="manual-qty"
              type="number"
              min={1}
              step={1}
              inputMode="numeric"
              value={qty}
              onChange={(e) => onOrderFieldChange(setQty)(e.target.value)}
              placeholder="100"
              className="font-mono"
              data-testid="manual-ticket-qty"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="manual-type">Order type</Label>
            <Select
              id="manual-type"
              value={type}
              onChange={(e) =>
                onOrderFieldChange(setType)(e.target.value as ManualOrderType)
              }
              data-testid="manual-ticket-type"
            >
              <option value="MARKET" data-testid="manual-ticket-type-market">
                MARKET
              </option>
              <option value="LIMIT" data-testid="manual-ticket-type-limit">
                LIMIT
              </option>
            </Select>
          </div>
        </div>

        {/* Row 3: limit price (LIMIT only) */}
        {type === "LIMIT" ? (
          <div className="space-y-1.5">
            <Label htmlFor="manual-limit">Limit price (USD)</Label>
            <Input
              id="manual-limit"
              type="number"
              min={0}
              step="0.01"
              inputMode="decimal"
              value={limitPrice}
              onChange={(e) => onOrderFieldChange(setLimitPrice)(e.target.value)}
              placeholder="0.00"
              className="font-mono"
              data-testid="manual-ticket-limit-price"
            />
            {!limitOk ? (
              <p className="text-[11px] text-destructive">
                A LIMIT order requires a positive limit price.
              </p>
            ) : null}
          </div>
        ) : null}

        {/* Reason (audited) */}
        <div className="space-y-1.5">
          <Label htmlFor="manual-reason">Reason (audited, optional)</Label>
          <Input
            id="manual-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder="e.g. discretionary add ahead of earnings"
            autoComplete="off"
            data-testid="manual-ticket-reason"
          />
        </div>

        {/* Confirm token (always required: paper password OR the live phrase) */}
        <div className="space-y-1.5">
          <Label htmlFor="manual-confirm">
            {liveArmed ? (
              <>
                Confirm phrase —{" "}
                <span className="rounded bg-muted px-1 font-mono text-foreground">
                  {MANUAL_LIVE_CONFIRM_PHRASE}
                </span>
              </>
            ) : (
              "Trade password (confirm_token)"
            )}
          </Label>
          <Input
            id="manual-confirm"
            type={liveArmed ? "text" : "password"}
            value={confirmToken}
            onChange={(e) => setConfirmToken(e.target.value)}
            placeholder={
              liveArmed ? MANUAL_LIVE_CONFIRM_PHRASE : "paper trade password"
            }
            autoComplete="off"
            className={liveArmed ? "border-destructive/50" : undefined}
            data-testid="manual-ticket-confirm"
          />
          <p className="text-[11px] text-muted-foreground">
            {liveArmed
              ? "REAL MONEY. The exact phrase is required; the live desk re-checks it and rejects any mismatch."
              : "The server requires the paper trade password to authorize this order. No real-money order is possible without the full server-side live gate."}
          </p>
        </div>

        {/* Risk override */}
        <label
          className="flex cursor-pointer items-start gap-2 rounded-lg border border-border p-2.5"
          data-testid="manual-ticket-override-row"
        >
          <Checkbox
            checked={override}
            onChange={(e) => {
              setPlaced(null);
              setOverride(e.target.checked);
            }}
            className="mt-0.5"
            data-testid="manual-ticket-override"
          />
          <span className="text-xs">
            <span className="font-medium">Override risk gate</span>
            <span className="block text-muted-foreground">
              Submit even if the order violates an allocator budget / concentration
              / daily-loss limit. Recorded in risk_events + the audit log. Closing
              orders always bypass the budget.
            </span>
          </span>
        </label>

        {/* Server error (any non-overridable error) — 412 confirmation_required,
            503 no desk, or a 422 order_rejected broker business rejection. */}
        {serverError ? (
          <Alert variant="destructive" data-testid="manual-ticket-error">
            <AlertDescription>{serverError}</AlertDescription>
          </Alert>
        ) : null}

        {/* Risk violation (422) — inline; offers override */}
        {violation ? (
          <Alert
            variant="destructive"
            data-testid="manual-ticket-violation"
            data-code="risk_violation"
            data-rule={violation.rule}
          >
            <ShieldAlert className="size-4" />
            <AlertTitle>Risk gate rejected this order</AlertTitle>
            <AlertDescription>
              <p className="mb-2">{violation.message}</p>
              <p className="text-xs">
                Check <span className="font-medium">Override risk gate</span> above
                and submit again to place it as an audited operator override.
              </p>
            </AlertDescription>
          </Alert>
        ) : null}

        {/* Placed confirmation */}
        {placed ? (
          <Alert
            variant={placed.submitted ? "default" : "warning"}
            data-testid="manual-ticket-placed"
            data-client-order-id={placed.coid}
            data-submitted={placed.submitted ? "true" : "false"}
          >
            <Send className="size-4" />
            <AlertDescription>
              {placed.submitted ? (
                <>
                  Order submitted — id{" "}
                  <span className="font-mono">{placed.coid}</span>. Track it in the
                  blotter.
                </>
              ) : (
                <>
                  No order submitted (idempotent no-op) — id{" "}
                  <span className="font-mono">{placed.coid}</span>.
                </>
              )}
              <Button
                variant="link"
                size="sm"
                className="ml-1 h-auto p-0"
                onClick={resetTicket}
                data-testid="manual-ticket-new"
              >
                New ticket
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        {/* Submit (disabled until required fields + confirm token) */}
        <Button
          variant={liveArmed ? "destructive" : "default"}
          disabled={!formOk}
          aria-disabled={!formOk}
          onClick={submit}
          className="w-full"
          data-testid="manual-ticket-submit"
        >
          <SideIcon />
          {place.isPending
            ? "Submitting…"
            : `${override ? "Override & submit" : "Submit"} ${side}${
                symbol ? ` ${symbol}` : ""
              }${liveArmed ? " — REAL" : ""}`}
        </Button>
      </CardContent>
    </Card>
  );
}

function freshKey(): string {
  const rnd =
    typeof crypto !== "undefined" && "randomUUID" in crypto
      ? crypto.randomUUID()
      : Math.random().toString(36).slice(2);
  return `manual-${Date.now().toString(36)}-${rnd}`;
}

/** Pull the gate's rule token out of the 422 message for the data-rule attr.
 * The server formats `... risk gate violation (...): <reason> (<rule>)`. */
function extractRule(message: string): string {
  const m = message.match(/\(([^()]+)\)\s*$/);
  return m?.[1] ?? "risk";
}
