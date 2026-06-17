"use client";

import * as React from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useUiMode } from "@/components/shell/ui-mode-provider";

/**
 * <ResponsiveTable> — a column-definition-driven table that renders the shadcn
 * <Table> on desktop and a stacked CARD LIST on mobile.
 *
 * Decision (LOCKED DECISION 2 — mobile = FULL FEATURE SET): the mobile card
 * carries *every* column, never drops a feature. Primary columns render
 * prominently at the top of each card; secondary columns fold into a
 * collapsible "More" section so the card stays scannable while keeping every
 * feature one tap away.
 *
 * The surface is chosen from `useUiMode().mode` (the explicit `ui-mode` cookie,
 * LOCKED DECISION 4) — NOT a CSS breakpoint — so a desktop user who forces
 * mobile gets the card list too.
 *
 * ── API ──────────────────────────────────────────────────────────────────────
 *   columns: ColumnDef<Row>[]   — one entry per column (see ColumnDef below).
 *   rows:    Row[]              — the data; same array the 19 tables already pass.
 *   rowKey:  (row, i) => string — stable key per row (defaults to the index).
 *   onRowClick?: (row) => void  — optional; whole row/card becomes clickable.
 *   caption?, className?, data-testid? — pass-throughs.
 *
 * Each ColumnDef:
 *   key       — stable id; also the React key for the cell.
 *   header    — column header (desktop <th>) AND the label in the mobile card.
 *   render    — (row, i) => ReactNode for the cell/value. Receives the row so a
 *               column can derive from multiple fields (same as today's tables).
 *   align?    — "left" | "right" | "center"; applied to the desktop cell.
 *   className?, headClassName? — extra classes for the cell / header.
 *   primary?  — when true (desktop unchanged), the column shows in the always-
 *               visible top block of the mobile card. Default: secondary
 *               (collapsed under "More"). If NO column is primary, the first
 *               column is treated as primary so a card is never empty.
 *   mobileHidden? — drop this column from the mobile card entirely. Off by
 *               default (full feature set); reserved for purely decorative columns.
 *   labelMobile? — override the card label (e.g. a short form of `header`).
 *
 * ── Usage ────────────────────────────────────────────────────────────────────
 *   const columns: ColumnDef<Row>[] = [
 *     { key: "name", header: "Table", primary: true,
 *       render: (r) => <span className="font-mono">{r.table}</span> },
 *     { key: "rows", header: "Rows", align: "right",
 *       render: (r) => formatInt(r.rows) },
 *   ];
 *   <ResponsiveTable columns={columns} rows={data.tables}
 *     rowKey={(r) => r.table} data-testid="coverage-table" />
 *
 * Migration note: a table adopts this by lifting its existing <TableHead> +
 * <TableCell> bodies into ColumnDefs verbatim — the `render` is the old cell
 * JSX, the `header` is the old <TableHead> text. No data reshaping required.
 */
export type ColumnAlign = "left" | "right" | "center";

export type ColumnDef<Row> = {
  /** Stable column id; also the React key for header/cell. */
  key: string;
  /** Column header (desktop) and field label (mobile card). */
  header: React.ReactNode;
  /** Cell renderer. Receives the row and its index. */
  render: (row: Row, index: number) => React.ReactNode;
  /** Horizontal alignment of the desktop cell + header. */
  align?: ColumnAlign;
  /** Extra classes for the desktop <td>. */
  className?: string;
  /** Extra classes for the desktop <th>. */
  headClassName?: string;
  /** Show in the always-visible block of the mobile card (vs. under "More"). */
  primary?: boolean;
  /** Omit from the mobile card entirely (decorative columns only). */
  mobileHidden?: boolean;
  /** Override the mobile card label when `header` is too wide. */
  labelMobile?: React.ReactNode;
};

const ALIGN_CELL: Record<ColumnAlign, string> = {
  left: "text-left",
  right: "text-right",
  center: "text-center",
};

export type ResponsiveTableProps<Row> = {
  columns: ColumnDef<Row>[];
  rows: Row[];
  /** Stable key per row; defaults to the row index. */
  rowKey?: (row: Row, index: number) => React.Key;
  /** Optional whole-row / whole-card click handler. */
  onRowClick?: (row: Row, index: number) => void;
  /** Optional per-row test id (desktop <tr> and mobile card). */
  rowTestId?: (row: Row, index: number) => string;
  /**
   * Optional extra attributes (typically `data-*`) applied to BOTH the desktop
   * `<tr>` and the mobile card — lets a table carry its row-level e2e contract
   * (e.g. `data-symbol`, `data-signed-qty`) across both surfaces unchanged.
   */
  rowAttrs?: (row: Row, index: number) => Record<string, string | undefined>;
  /**
   * Optional extra classes applied to BOTH the desktop `<tr>` and the mobile
   * card — e.g. a per-row highlight (buy-zone / stretched) that must survive on
   * both surfaces. The base row styling is preserved; these are appended.
   */
  rowClassName?: (row: Row, index: number) => string | undefined;
  caption?: React.ReactNode;
  className?: string;
  "data-testid"?: string;
};

export function ResponsiveTable<Row>({
  columns,
  rows,
  rowKey,
  onRowClick,
  rowTestId,
  rowAttrs,
  rowClassName,
  caption,
  className,
  "data-testid": testId,
}: ResponsiveTableProps<Row>) {
  const { mode } = useUiMode();
  const keyOf = React.useCallback(
    (row: Row, i: number): React.Key => (rowKey ? rowKey(row, i) : i),
    [rowKey],
  );

  if (mode === "mobile") {
    return (
      <MobileCardList
        columns={columns}
        rows={rows}
        keyOf={keyOf}
        onRowClick={onRowClick}
        rowTestId={rowTestId}
        rowAttrs={rowAttrs}
        rowClassName={rowClassName}
        caption={caption}
        className={className}
        testId={testId}
      />
    );
  }

  return (
    <Table data-testid={testId} className={className}>
      <TableHeader>
        <TableRow>
          {columns.map((col) => (
            <TableHead
              key={col.key}
              className={cn(
                col.align ? ALIGN_CELL[col.align] : undefined,
                col.headClassName,
              )}
            >
              {col.header}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row, i) => (
          <TableRow
            key={keyOf(row, i)}
            data-testid={rowTestId?.(row, i)}
            {...rowAttrs?.(row, i)}
            onClick={onRowClick ? () => onRowClick(row, i) : undefined}
            className={cn(
              onRowClick ? "cursor-pointer" : undefined,
              rowClassName?.(row, i),
            )}
          >
            {columns.map((col) => (
              <TableCell
                key={col.key}
                className={cn(
                  col.align ? ALIGN_CELL[col.align] : undefined,
                  col.className,
                )}
              >
                {col.render(row, i)}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
      {caption ? (
        <caption className="mt-4 caption-bottom text-sm text-muted-foreground">
          {caption}
        </caption>
      ) : null}
    </Table>
  );
}

function MobileCardList<Row>({
  columns,
  rows,
  keyOf,
  onRowClick,
  rowTestId,
  rowAttrs,
  rowClassName,
  caption,
  className,
  testId,
}: {
  columns: ColumnDef<Row>[];
  rows: Row[];
  keyOf: (row: Row, index: number) => React.Key;
  onRowClick?: (row: Row, index: number) => void;
  rowTestId?: (row: Row, index: number) => string;
  rowAttrs?: (row: Row, index: number) => Record<string, string | undefined>;
  rowClassName?: (row: Row, index: number) => string | undefined;
  caption?: React.ReactNode;
  className?: string;
  testId?: string;
}) {
  const visible = columns.filter((c) => !c.mobileHidden);
  let primary = visible.filter((c) => c.primary);
  let secondary = visible.filter((c) => !c.primary);
  // Never render an empty card: if nothing is marked primary, promote the
  // first visible column so the card always has a prominent line.
  if (primary.length === 0 && visible.length > 0) {
    primary = visible.slice(0, 1);
    secondary = visible.slice(1);
  }

  return (
    <div
      data-testid={testId}
      data-slot="responsive-table-cards"
      className={cn("flex flex-col gap-2", className)}
    >
      {rows.map((row, i) => (
        <MobileRowCard
          key={keyOf(row, i)}
          row={row}
          index={i}
          primary={primary}
          secondary={secondary}
          onRowClick={onRowClick}
          testId={rowTestId?.(row, i)}
          attrs={rowAttrs?.(row, i)}
          extraClassName={rowClassName?.(row, i)}
        />
      ))}
      {caption ? (
        <p className="mt-1 text-sm text-muted-foreground">{caption}</p>
      ) : null}
    </div>
  );
}

function MobileRowCard<Row>({
  row,
  index,
  primary,
  secondary,
  onRowClick,
  testId,
  attrs,
  extraClassName,
}: {
  row: Row;
  index: number;
  primary: ColumnDef<Row>[];
  secondary: ColumnDef<Row>[];
  onRowClick?: (row: Row, index: number) => void;
  testId?: string;
  attrs?: Record<string, string | undefined>;
  extraClassName?: string;
}) {
  const [open, setOpen] = React.useState(false);
  const clickable = Boolean(onRowClick);

  return (
    <div
      data-testid={testId}
      {...attrs}
      data-slot="responsive-table-card"
      className={cn(
        "rounded-lg bg-card p-3 text-card-foreground ring-1 ring-foreground/10",
        clickable && "cursor-pointer transition-colors hover:bg-muted/40",
        extraClassName,
      )}
      onClick={onRowClick ? () => onRowClick(row, index) : undefined}
    >
      <dl className="flex flex-col gap-1.5">
        {primary.map((col) => (
          <Field key={col.key} col={col} row={row} index={index} prominent />
        ))}
      </dl>

      {secondary.length > 0 ? (
        <div className="mt-1">
          <button
            type="button"
            // Don't trigger the row click when toggling details.
            onClick={(e) => {
              e.stopPropagation();
              setOpen((v) => !v);
            }}
            aria-expanded={open}
            className="flex items-center gap-1 py-1 text-xs font-medium text-muted-foreground"
            data-testid="responsive-table-card-more"
          >
            <ChevronDown
              className={cn(
                "size-3.5 transition-transform",
                open && "rotate-180",
              )}
            />
            {open ? "Less" : `More (${secondary.length})`}
          </button>
          {open ? (
            <dl className="mt-1 flex flex-col gap-1.5 border-t pt-2">
              {secondary.map((col) => (
                <Field key={col.key} col={col} row={row} index={index} />
              ))}
            </dl>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function Field<Row>({
  col,
  row,
  index,
  prominent,
}: {
  col: ColumnDef<Row>;
  row: Row;
  index: number;
  prominent?: boolean;
}) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="shrink-0 text-xs text-muted-foreground">
        {col.labelMobile ?? col.header}
      </dt>
      <dd
        className={cn(
          "min-w-0 text-right",
          prominent ? "text-sm font-medium" : "text-sm",
        )}
      >
        {col.render(row, index)}
      </dd>
    </div>
  );
}
