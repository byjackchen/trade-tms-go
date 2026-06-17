// Trade is a live operator surface (paper AND real-money, account-driven): every
// panel reads PG snapshots over the bearer-guarded proxy and follows a WS stream,
// and the module reads the `?account=` / `?view=` query (useSearchParams). Opt the
// /trade subtree out of static prerender so Next renders it per-request (it is
// always per-request anyway).
export const dynamic = "force-dynamic";

export default function TradeLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return children;
}
