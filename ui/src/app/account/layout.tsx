// Account is a live operator surface (paper AND real-money, account-driven): every
// panel reads PG snapshots over the bearer-guarded proxy and follows a WS stream,
// and the view reads the `?tab=` query (useSearchParams). Opt the /account
// subtree out of static prerender so Next renders it per-request.
export const dynamic = "force-dynamic";

export default function AccountLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return children;
}
