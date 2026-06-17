import type { MetadataRoute } from "next";

// PWA manifest (Next metadata route -> /manifest.webmanifest + an auto-injected
// <link rel="manifest">). `display: standalone` makes the installed app run
// full-screen with no browser chrome. Icons are placeholder tms wordmark+
// candlestick glyphs (swap for real branding under ui/public/icons).
export const dynamic = "force-static";

export default function manifest(): MetadataRoute.Manifest {
  return {
    name: "TMS — Control Plane",
    short_name: "TMS",
    description:
      "Trade TMS control plane: systems & data, strategies, compositions, paper & live trading.",
    id: "/",
    start_url: "/systems",
    scope: "/",
    display: "standalone",
    orientation: "any",
    background_color: "#0f172a",
    theme_color: "#0f172a",
    icons: [
      { src: "/icons/icon-192.png", sizes: "192x192", type: "image/png", purpose: "any" },
      { src: "/icons/icon-512.png", sizes: "512x512", type: "image/png", purpose: "any" },
      {
        src: "/icons/icon-maskable-512.png",
        sizes: "512x512",
        type: "image/png",
        purpose: "maskable",
      },
    ],
  };
}
