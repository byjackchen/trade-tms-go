/** Locale-stable integer formatting (e.g. 9_000_000 -> "9,000,000"). */
export function formatInt(value: number | null | undefined): string {
  if (value == null || Number.isNaN(value)) return "—";
  return new Intl.NumberFormat("en-US").format(value);
}

/** Compact large counts for dense cells (e.g. 9_000_000 -> "9.0M"). */
export function formatCompact(value: number | null | undefined): string {
  if (value == null || Number.isNaN(value)) return "—";
  return new Intl.NumberFormat("en-US", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(value);
}

/** Render an RFC3339 UTC timestamp as a short, locale-stable wall-clock label. */
export function formatTs(ts: string | null | undefined): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString("en-US", {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** A YYYY-MM-DD date passed through unchanged (already an NYSE trading date). */
export function formatDate(d: string | null | undefined): string {
  if (!d) return "—";
  return d;
}

/**
 * Human-readable relative time from an RFC3339 timestamp (e.g. "5m ago").
 * Returns "never" for null/empty. Computed against a caller-supplied `now` so
 * it stays deterministic across re-renders when desired.
 */
export function formatRelative(
  ts: string | null | undefined,
  now: number = Date.now(),
): string {
  if (!ts) return "never";
  const then = new Date(ts).getTime();
  if (Number.isNaN(then)) return "—";
  const diffSec = Math.floor((now - then) / 1000);
  if (diffSec < 0) return "just now";
  if (diffSec < 60) return `${diffSec}s ago`;
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHr = Math.floor(diffMin / 60);
  if (diffHr < 24) return `${diffHr}h ago`;
  const diffDay = Math.floor(diffHr / 24);
  return `${diffDay}d ago`;
}

/** Format a fractional duration in milliseconds between two RFC3339 timestamps. */
export function formatDuration(
  startTs: string | null | undefined,
  endTs: string | null | undefined,
): string {
  if (!startTs || !endTs) return "—";
  const start = new Date(startTs).getTime();
  const end = new Date(endTs).getTime();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return "—";
  const sec = Math.round((end - start) / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  const rem = sec % 60;
  if (min < 60) return rem ? `${min}m ${rem}s` : `${min}m`;
  const hr = Math.floor(min / 60);
  return `${hr}h ${min % 60}m`;
}
