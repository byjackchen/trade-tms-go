/**
 * UI-mode plumbing shared between the server (layout) and the client
 * (provider/hook/toggle).
 *
 * Decision (docs/concept-alignment.md, LOCKED DECISION 4): the desktop/mobile
 * surface is driven by an EXPLICIT cookie — NOT pure CSS — so a manual toggle
 * can force mobile on a desktop (or vice versa). The cookie holds the user's
 * *preference* (`pref`); the *resolved* mode (`desktop` | `mobile`) is what the
 * shells actually render against.
 *
 * - `pref = "desktop" | "mobile"` — explicit override, wins everywhere.
 * - `pref = "auto"` — resolve from the device: User-Agent on the server (SSR),
 *   `matchMedia` on the client. This is the default for an unknown device
 *   (LOCKED DECISION 3).
 *
 * Keeping these as plain string literals (no `server-only` import) lets the
 * client provider parse the same cookie shape the server wrote.
 */

/** The resolved surface a shell renders against. */
export type UiMode = "desktop" | "mobile";

/** The user's stored preference; `auto` defers to the device. */
export type UiModePref = UiMode | "auto";

/** The cookie name carrying the preference (desktop | mobile | auto). */
export const UI_MODE_COOKIE = "ui-mode";

/** One year, in seconds — the toggle should be sticky across sessions. */
export const UI_MODE_COOKIE_MAX_AGE = 60 * 60 * 24 * 365;

/**
 * The viewport width below which `auto` resolves to `mobile`. Mirrors Tailwind's
 * `md` breakpoint (768px) so the CSS shells and the JS resolution agree.
 */
export const UI_MODE_MOBILE_MAX_WIDTH = 767;

/** The matchMedia query that resolves `auto` on the client. */
export const UI_MODE_MOBILE_MEDIA = `(max-width: ${UI_MODE_MOBILE_MAX_WIDTH}px)`;

/** Narrow an arbitrary string to a valid preference, or `null` if it isn't one. */
export function parseUiModePref(value: string | undefined | null): UiModePref | null {
  if (value === "desktop" || value === "mobile" || value === "auto") return value;
  return null;
}

/**
 * Infer the resolved mode from a User-Agent string. Used on the server when the
 * preference is `auto` (or absent), so the very first paint already matches the
 * device — no client round-trip, SSR-safe. Conservative: only flips to mobile on
 * the well-known mobile UA tokens, defaulting to desktop otherwise.
 */
export function inferModeFromUserAgent(userAgent: string | undefined | null): UiMode {
  if (!userAgent) return "desktop";
  // iPad (modern Safari reports a desktop UA) is intentionally treated as
  // desktop here; a tablet user who wants the mobile shell can force it.
  return /Android|webOS|iPhone|iPod|BlackBerry|IEMobile|Opera Mini|Mobile/i.test(
    userAgent,
  )
    ? "mobile"
    : "desktop";
}

/**
 * Resolve the SSR mode from the cookie preference + the request User-Agent.
 * Explicit `desktop`/`mobile` win; `auto`/absent fall back to UA inference.
 */
export function resolveServerMode(
  pref: UiModePref | null,
  userAgent: string | undefined | null,
): { mode: UiMode; pref: UiModePref } {
  if (pref === "desktop" || pref === "mobile") return { mode: pref, pref };
  return { mode: inferModeFromUserAgent(userAgent), pref: "auto" };
}
