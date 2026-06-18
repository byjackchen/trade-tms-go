import { redirect } from "next/navigation";

/**
 * Legacy `/trade` route. The unified "Trade" top-level was SPLIT into "Session"
 * (runtime control) and "Account" (the persistent book). Redirect to /session so
 * any bookmarked /trade link lands on the runtime surface.
 */
export default function TradeRedirect() {
  redirect("/session");
}
