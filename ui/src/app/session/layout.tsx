// Session is a live runtime surface: every panel reads PG snapshots over the
// bearer-guarded proxy and follows a WS stream, and the view reads the live
// session (useSearchParams indirectly via children). Opt the /session subtree out
// of static prerender so Next renders it per-request.
export const dynamic = "force-dynamic";

export default function SessionLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return children;
}
