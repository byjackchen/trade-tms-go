/**
 * Mirror of the UI's number formatting so DB-truth assertions compare apples to
 * apples. The UI renders integer cells via `formatInt` in
 * ui/src/lib/format.ts, which is exactly `Intl.NumberFormat("en-US")`
 * (e.g. 9000000 -> "9,000,000"). Replicated here verbatim.
 */
export function formatIntLikeUi(value: number): string {
  if (Number.isNaN(value)) return "—";
  return new Intl.NumberFormat("en-US").format(value);
}
