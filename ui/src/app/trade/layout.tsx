// The trade cockpit is a live operator surface: every panel reads PG snapshots
// over the bearer-guarded proxy and follows a WS stream, and the tabs/selector
// read the `?account=` query (useSearchParams). Opt the whole /trade subtree out
// of static prerender so Next never tries to statically generate a search-param-
// dependent page (which would otherwise require a Suspense boundary around every
// useSearchParams caller) — these pages are always rendered per-request anyway.
export const dynamic = "force-dynamic";

export default function TradeLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return children;
}
