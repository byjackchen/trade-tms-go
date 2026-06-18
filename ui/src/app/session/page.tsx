"use client";

import { SessionView } from "@/components/portfolio/session-view";

/**
 * Session (#4) — the RUNTIME top-level. The session lifecycle + exec policy, the
 * running Composition, the session's bound account (read-only link to /account),
 * and the live BAR tape. Account selection + the persistent book live in the
 * Account top-level (docs/concept-alignment.md §3.4).
 */
export default function SessionPage() {
  return <SessionView />;
}
