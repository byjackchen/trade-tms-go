import type { Metadata, Viewport } from "next";
import { Inter, JetBrains_Mono } from "next/font/google";
import { cookies, headers } from "next/headers";
import { Providers } from "./providers";
import { ShellSwitch } from "@/components/shell/shell-switch";
import { UiModeProvider } from "@/components/shell/ui-mode-provider";
import { UI_MODE_COOKIE, parseUiModePref, resolveServerMode } from "@/lib/ui-mode";
import { ServiceWorkerRegister } from "@/components/pwa/service-worker-register";
import "./globals.css";

const inter = Inter({ variable: "--font-sans", subsets: ["latin"] });
const mono = JetBrains_Mono({ variable: "--font-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  title: "tms — control plane",
  description: "Trade TMS control plane: market data, backtests, live trading.",
  applicationName: "TMS",
  // PWA: app/manifest.ts auto-injects <link rel="manifest">. These add the
  // iOS standalone hints + the touch icon (Android reads the manifest icons).
  appleWebApp: {
    capable: true,
    statusBarStyle: "black-translucent",
    title: "TMS",
  },
  icons: {
    icon: "/icons/icon-192.png",
    apple: "/icons/apple-touch-icon.png",
  },
};

// Mobile parity (LOCKED DECISION 2) needs a real responsive viewport so the
// mobile shell measures device-width, not a zoomed-out desktop canvas.
// themeColor tints the mobile browser/standalone status bar (PWA).
export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  themeColor: "#0f172a",
};

export default async function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  // Resolve the UI mode SSR-side (docs/concept-alignment.md, LOCKED DECISIONS
  // 3 & 4): the explicit `ui-mode` cookie (desktop|mobile|auto) wins; when
  // absent or `auto` we infer desktop|mobile from the User-Agent so the first
  // paint already matches the device. The resolved mode seeds both
  // <html data-ui-mode> and the client provider, so SSR === first CSR render.
  const [cookieStore, headerStore] = await Promise.all([cookies(), headers()]);
  const pref = parseUiModePref(cookieStore.get(UI_MODE_COOKIE)?.value);
  const userAgent = headerStore.get("user-agent");
  const { mode, pref: resolvedPref } = resolveServerMode(pref, userAgent);

  return (
    <html
      lang="en"
      data-ui-mode={mode}
      className={`${inter.variable} ${mono.variable} h-full antialiased`}
      suppressHydrationWarning
    >
      <body className="min-h-full">
        <ServiceWorkerRegister />
        <Providers>
          <UiModeProvider initialMode={mode} initialPref={resolvedPref}>
            <ShellSwitch>{children}</ShellSwitch>
          </UiModeProvider>
        </Providers>
      </body>
    </html>
  );
}
