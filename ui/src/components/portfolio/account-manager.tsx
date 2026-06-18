"use client";

import { useMemo, useState } from "react";
import { Pencil, Plus, Star, Trash2 } from "lucide-react";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Checkbox } from "@/components/ui/checkbox";
import { Badge } from "@/components/ui/badge";
import { Sheet } from "@/components/ui/sheet";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { ApiError } from "@/lib/api/client";
import {
  useAccounts,
  useCreateAccount,
  useUpdateAccount,
  useDeleteAccount,
} from "@/lib/api/hooks";
import type {
  AccountEnv,
  AccountWriteRequest,
  TradeAccountInfo,
} from "@/lib/api/types";
import { accountEnv } from "./trade-env";

/**
 * `<AccountManager />` — the registry CRUD surface on the Accounts top-level.
 * Lists every registered account (label, env, broker account #, default marker,
 * notes) and exposes the full lifecycle: New / Edit / Delete / Set default. It
 * reads + writes the SAME tms.accounts registry the bound-account selector reads,
 * so creating/editing/deleting here refreshes the selector immediately.
 *
 * This is the persistent registry, NOT session/runtime state — no exec/live/
 * health widgets live here (those belong on /session).
 */
export function AccountManager() {
  const q = useAccounts();
  const accounts = useMemo<TradeAccountInfo[]>(
    () => q.data?.accounts ?? [],
    [q.data],
  );
  const noReader = q.error instanceof ApiError && q.error.status === 503;

  // The form sheet: `null` closed, `{}` create, a row => edit.
  const [editing, setEditing] = useState<TradeAccountInfo | null | "new">(null);
  // The delete-confirm target (a row), plus an inline error (e.g. account_in_use).
  const [deleting, setDeleting] = useState<TradeAccountInfo | null>(null);

  const updateMut = useUpdateAccount();

  // Set-default reuses the update (PATCH) with is_default flipped on. We capture
  // which row is mid-flight so only that button shows the pending state.
  const [defaultingId, setDefaultingId] = useState<string | null>(null);
  const onSetDefault = (a: TradeAccountInfo) => {
    setDefaultingId(a.id);
    updateMut.mutate(
      { id: a.id, body: toWriteRequest({ ...a, is_default: true }) },
      { onSettled: () => setDefaultingId(null) },
    );
  };

  return (
    <Card data-testid="account-manager">
      <CardHeader className="border-b">
        <div className="space-y-1">
          <CardTitle>Manage accounts</CardTitle>
          <CardDescription>
            The persistent account registry. Create, edit, delete, or set the
            default account per environment. Changes update the account selector
            above.
          </CardDescription>
        </div>
        <CardAction>
          <Button
            size="sm"
            onClick={() => setEditing("new")}
            disabled={noReader}
            data-testid="account-new"
          >
            <Plus /> New account
          </Button>
        </CardAction>
      </CardHeader>

      <CardContent>
        {noReader ? (
          <Alert variant="warning" data-testid="account-manager-noreader">
            <AlertDescription>
              The trade reader is unconfigured (signal-only deployment), so the
              account registry is unavailable.
            </AlertDescription>
          </Alert>
        ) : q.isLoading ? (
          <div className="space-y-2" data-testid="account-manager-loading">
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-12 w-full" />
          </div>
        ) : accounts.length === 0 ? (
          <p
            className="py-6 text-center text-sm text-muted-foreground"
            data-testid="account-manager-empty"
          >
            No accounts yet. Create one to bind a Portfolio to a broker or sim
            book.
          </p>
        ) : (
          <ul className="divide-y" data-testid="account-manager-list">
            {accounts.map((a) => (
              <AccountRow
                key={a.id}
                account={a}
                onEdit={() => setEditing(a)}
                onDelete={() => setDeleting(a)}
                onSetDefault={() => onSetDefault(a)}
                settingDefault={defaultingId === a.id}
              />
            ))}
          </ul>
        )}
      </CardContent>

      {editing !== null ? (
        <AccountFormSheet
          account={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
        />
      ) : null}

      {deleting !== null ? (
        <DeleteAccountSheet
          account={deleting}
          onClose={() => setDeleting(null)}
        />
      ) : null}
    </Card>
  );
}

/** One account row: label + env/default badges, broker #, notes, and actions. */
function AccountRow({
  account: a,
  onEdit,
  onDelete,
  onSetDefault,
  settingDefault,
}: {
  account: TradeAccountInfo;
  onEdit: () => void;
  onDelete: () => void;
  onSetDefault: () => void;
  settingDefault: boolean;
}) {
  const kind = accountEnv(a); // "paper" | "live"
  const rawEnv = (a.env || "").toLowerCase();
  return (
    <li
      className="flex flex-col gap-3 py-3 ui-desktop:flex-row ui-desktop:items-center ui-desktop:justify-between"
      data-testid="account-manager-row"
      data-account-id={a.id}
    >
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="truncate font-medium" data-testid="account-row-label">
            {a.label?.trim() || a.id}
          </span>
          <EnvBadge env={rawEnv} kind={kind} />
          {a.is_default ? (
            <Badge variant="success" data-testid="account-row-default">
              <Star className="size-3" /> Default
            </Badge>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
          <span>{a.venue || "—"}</span>
          <span>
            Broker #
            <span className="font-mono text-foreground">{a.broker_acc_id}</span>
          </span>
        </div>
        {a.notes?.trim() ? (
          <p
            className="text-xs text-muted-foreground"
            data-testid="account-row-notes"
          >
            {a.notes}
          </p>
        ) : null}
      </div>

      <div className="flex shrink-0 items-center gap-1.5">
        <Button
          variant="outline"
          size="sm"
          onClick={onSetDefault}
          disabled={a.is_default || settingDefault}
          data-testid="account-set-default"
        >
          <Star /> {settingDefault ? "Setting…" : "Set default"}
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={onEdit}
          data-testid="account-edit"
        >
          <Pencil /> Edit
        </Button>
        <Button
          variant="destructive"
          size="sm"
          onClick={onDelete}
          data-testid="account-delete"
        >
          <Trash2 /> Delete
        </Button>
      </div>
    </li>
  );
}

/**
 * Friendly env labels; `real` is loud-red so REAL money is unmistakable. The
 * synthetic `simu` env was retired backend-side — accounts are broker-only now
 * (paper|real) — so it is no longer offered in the form. The map keeps a label
 * for any legacy `simu` rows still in the registry.
 */
const ENV_LABEL: Record<string, string> = {
  simu: "Sim (legacy)",
  paper: "Paper (broker)",
  real: "Real (LIVE money)",
};

/** The env badge. `real` (kind=live) is destructive-red; paper/legacy are amber. */
function EnvBadge({ env, kind }: { env: string; kind: "paper" | "live" }) {
  const label = ENV_LABEL[env] ?? (env || "—");
  return (
    <Badge
      variant={kind === "live" ? "destructive" : "warning"}
      data-testid="account-env-badge"
      data-kind={kind}
      data-env={env}
      className={kind === "live" ? "font-semibold uppercase tracking-wide" : ""}
    >
      {label}
    </Badge>
  );
}

/** Map a (possibly partial) account onto the write request shape. */
function toWriteRequest(
  a: Partial<TradeAccountInfo> & { is_default?: boolean },
): AccountWriteRequest {
  const env = (a.env || "").toLowerCase();
  // PRESERVE the account's existing env when it is already paper|real (so e.g.
  // "Set default" never rewrites it). Only a truly-unknown legacy value (the
  // retired synthetic `simu`, no longer creatable) falls back to "paper".
  const safeEnv: AccountEnv = env === "real" ? "real" : "paper";
  return {
    venue: a.venue ?? "moomoo",
    env: safeEnv,
    broker_acc_id: a.broker_acc_id ?? 0,
    label: a.label ?? "",
    notes: a.notes ?? "",
    is_default: a.is_default ?? false,
  };
}

type FormState = {
  venue: string;
  env: AccountEnv;
  broker_acc_id: string; // kept as text so the input can be empty mid-edit
  label: string;
  notes: string;
  is_default: boolean;
};

/**
 * Create + edit form, in a Sheet (bottom sheet on mobile, centered modal on
 * desktop). Fields: venue, env (paper|real — `simu` was retired backend-side),
 * broker_acc_id, label, notes, is_default. On success it closes (the hook busts
 * the registry query).
 */
function AccountFormSheet({
  account,
  onClose,
}: {
  account: TradeAccountInfo | null;
  onClose: () => void;
}) {
  const isEdit = account !== null;
  const createMut = useCreateAccount();
  const updateMut = useUpdateAccount();

  const [form, setForm] = useState<FormState>(() => ({
    venue: account?.venue ?? "moomoo",
    env: ((): AccountEnv => {
      const e = (account?.env || "").toLowerCase();
      // `simu` was retired — default a new account to `paper`; map legacy simu
      // rows onto `paper` so the (now broker-only) select has a valid value.
      return e === "real" ? "real" : "paper";
    })(),
    broker_acc_id:
      account != null ? String(account.broker_acc_id ?? 0) : "",
    label: account?.label ?? "",
    notes: account?.notes ?? "",
    is_default: account?.is_default ?? false,
  }));

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) =>
    setForm((f) => ({ ...f, [k]: v }));

  const pending = createMut.isPending || updateMut.isPending;
  const err = (createMut.error ?? updateMut.error) as ApiError | Error | null;

  const brokerNum = Number(form.broker_acc_id);
  const brokerValid =
    form.broker_acc_id.trim() !== "" && Number.isInteger(brokerNum) && brokerNum >= 0;
  // Every account is broker-backed now (paper|real), so it needs a real
  // (non-zero) broker account number to be bindable.
  const brokerOkForEnv = brokerValid && brokerNum > 0;
  const labelOk = form.label.trim().length > 0;
  const canSubmit = brokerValid && brokerOkForEnv && labelOk && !pending;

  const onSubmit = () => {
    const body: AccountWriteRequest = {
      venue: form.venue.trim() || "moomoo",
      env: form.env,
      broker_acc_id: brokerNum,
      label: form.label.trim(),
      notes: form.notes.trim(),
      is_default: form.is_default,
    };
    const opts = { onSuccess: () => onClose() };
    if (isEdit && account) {
      updateMut.mutate({ id: account.id, body }, opts);
    } else {
      createMut.mutate(body, opts);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={isEdit ? "Edit account" : "New account"}
      description={
        isEdit
          ? "Replace this account's details. env is the source of truth."
          : "Register a new account. env is the source of truth (real = LIVE money)."
      }
      data-testid="account-form"
      footer={
        <>
          <Button
            variant="outline"
            onClick={onClose}
            disabled={pending}
            data-testid="account-form-cancel"
          >
            Cancel
          </Button>
          <Button
            variant={form.env === "real" ? "destructive" : "default"}
            onClick={onSubmit}
            disabled={!canSubmit}
            aria-disabled={!canSubmit}
            data-testid="account-form-submit"
          >
            {pending ? "Saving…" : isEdit ? "Save changes" : "Create account"}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="grid gap-4 ui-desktop:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="account-venue">Venue</Label>
            <Input
              id="account-venue"
              value={form.venue}
              onChange={(e) => set("venue", e.target.value)}
              placeholder="moomoo"
              disabled={pending}
              data-testid="account-form-venue"
              autoComplete="off"
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="account-env">Environment</Label>
            <Select
              id="account-env"
              value={form.env}
              onChange={(e) => set("env", e.target.value as AccountEnv)}
              disabled={pending}
              data-testid="account-form-env"
            >
              <option value="paper">{ENV_LABEL.paper}</option>
              <option value="real">{ENV_LABEL.real}</option>
            </Select>
          </div>
        </div>

        {form.env === "real" ? (
          <Alert variant="destructive" data-testid="account-form-real-warning">
            <AlertDescription>
              This is a REAL-money account. Orders bound to it trade live
              capital.
            </AlertDescription>
          </Alert>
        ) : null}

        <div className="grid gap-4 ui-desktop:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="account-broker">Broker account #</Label>
            <Input
              id="account-broker"
              type="number"
              inputMode="numeric"
              value={form.broker_acc_id}
              onChange={(e) => set("broker_acc_id", e.target.value)}
              placeholder="e.g. 3063"
              disabled={pending}
              data-testid="account-form-broker"
              autoComplete="off"
            />
            {!brokerOkForEnv && form.broker_acc_id.trim() !== "" ? (
              <p className="text-xs text-destructive">
                Accounts need a real (non-zero) broker account number.
              </p>
            ) : (
              <p className="text-xs text-muted-foreground">
                The broker&apos;s account number (required to bind).
              </p>
            )}
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="account-label">Label</Label>
            <Input
              id="account-label"
              value={form.label}
              onChange={(e) => set("label", e.target.value)}
              placeholder="e.g. 保证金 paper"
              disabled={pending}
              data-testid="account-form-label"
              autoComplete="off"
            />
          </div>
        </div>

        <div className="space-y-1.5">
          <Label htmlFor="account-notes">Notes</Label>
          <textarea
            id="account-notes"
            value={form.notes}
            onChange={(e) => set("notes", e.target.value)}
            placeholder="Optional operator notes."
            disabled={pending}
            rows={3}
            data-testid="account-form-notes"
            className="flex w-full min-w-0 rounded-lg border border-input bg-background px-3 py-2 text-sm transition-colors outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-input/30"
          />
        </div>

        <Label className="cursor-pointer">
          <Checkbox
            checked={form.is_default}
            onChange={(e) => set("is_default", e.target.checked)}
            disabled={pending}
            data-testid="account-form-default"
          />
          <span>
            Make this the default account for{" "}
            <span className="font-mono">
              {form.venue.trim() || "moomoo"}/{form.env}
            </span>
          </span>
        </Label>
        <p className="text-xs text-muted-foreground">
          At most one default per venue + environment — setting this unsets the
          previous default.
        </p>

        {err ? (
          <Alert variant="destructive" data-testid="account-form-error">
            <AlertDescription>{writeErrorMessage(err)}</AlertDescription>
          </Alert>
        ) : null}
      </div>
    </Sheet>
  );
}

/** Turn a write error into an operator-readable message (codes from the API). */
function writeErrorMessage(err: ApiError | Error): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "invalid_account":
        return `Invalid account: ${err.message}`;
      case "not_found":
        return "This account no longer exists — it may have been deleted.";
      case "unavailable":
        return "The account registry is temporarily unavailable. Try again.";
      default:
        return err.message;
    }
  }
  return err.message;
}

/**
 * Delete confirmation. On a 409 (code="account_in_use") the API rejects the
 * delete because sessions/orders reference the account; we surface that inline
 * and tell the operator to keep it.
 */
function DeleteAccountSheet({
  account,
  onClose,
}: {
  account: TradeAccountInfo;
  onClose: () => void;
}) {
  const delMut = useDeleteAccount();
  const err = delMut.error as ApiError | Error | null;
  const inUse = err instanceof ApiError && err.code === "account_in_use";

  const onConfirm = () => {
    delMut.mutate(account.id, { onSuccess: () => onClose() });
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title="Delete account"
      description={
        <>
          Delete{" "}
          <span className="font-medium text-foreground">
            {account.label?.trim() || account.id}
          </span>
          ? This can&apos;t be undone.
        </>
      }
      data-testid="account-delete-confirm"
      footer={
        <>
          <Button
            variant="outline"
            onClick={onClose}
            disabled={delMut.isPending}
            data-testid="account-delete-cancel"
          >
            Cancel
          </Button>
          <Button
            variant="destructive"
            onClick={onConfirm}
            disabled={delMut.isPending || inUse}
            aria-disabled={delMut.isPending || inUse}
            data-testid="account-delete-submit"
          >
            {delMut.isPending ? "Deleting…" : "Delete account"}
          </Button>
        </>
      }
    >
      {err ? (
        <Alert
          variant={inUse ? "warning" : "destructive"}
          data-testid="account-delete-error"
        >
          <AlertDescription>
            {inUse
              ? "This account is referenced by sessions or orders — it can't be deleted. Keep it (you can edit its label/notes instead)."
              : writeErrorMessage(err)}
          </AlertDescription>
        </Alert>
      ) : (
        <p className="text-sm text-muted-foreground">
          Removing an account does not touch the broker — it only deletes the
          local registry entry.
        </p>
      )}
    </Sheet>
  );
}
