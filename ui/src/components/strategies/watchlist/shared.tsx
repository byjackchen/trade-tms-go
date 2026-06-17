"use client";

import { cn } from "@/lib/utils";

// csvCell quotes a value per RFC 4180.
export function csvCell(v: string | number): string {
  const s = String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

/** Trigger a client-side CSV download (no backend call). */
export function downloadCsv(filenameStem: string, lines: string[]) {
  const blob = new Blob([lines.join("\n") + "\n"], {
    type: "text/csv;charset=utf-8",
  });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `${filenameStem}-${new Date().toISOString().replace(/[:.]/g, "-")}.csv`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

/**
 * A clickable, sort-indicating header BUTTON (no <th> wrapper) for use as a
 * <ResponsiveTable> ColumnDef `header`: ResponsiveTable supplies the desktop
 * <th>, and on mobile this same button becomes the card's field label so the
 * column stays sortable on both surfaces. `align` only flips the icon order
 * (the desktop cell alignment is set by the ColumnDef `align`).
 */
export function SortButton<K extends string>({
  k,
  label,
  sortKey,
  sortDir,
  onSort,
  align = "left",
  title,
}: {
  k: K;
  label: string;
  sortKey: K;
  sortDir: "asc" | "desc";
  onSort: (k: K) => void;
  align?: "left" | "right";
  title?: string;
}) {
  return (
    <button
      type="button"
      onClick={() => onSort(k)}
      data-testid={`watchlist-sort-${k}`}
      title={title}
      className={cn(
        "inline-flex items-center gap-1 transition-colors hover:text-foreground",
        align === "right" && "flex-row-reverse",
        sortKey === k ? "text-foreground" : "text-muted-foreground",
      )}
    >
      {label}
      <span className="text-[10px] leading-none">
        {sortKey === k ? (sortDir === "asc" ? "▲" : "▼") : "↕"}
      </span>
    </button>
  );
}

