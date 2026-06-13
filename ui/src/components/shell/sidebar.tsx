"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useTheme } from "next-themes";
import { useEffect, useState } from "react";
import {
  Database,
  FlaskConical,
  Sparkles,
  Activity,
  Wrench,
  Moon,
  Sun,
  CircleDot,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";

type Section = {
  href: string;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  testid: string;
  /** Implemented now (P1) vs. placeholder (P2+). */
  ready: boolean;
};

const SECTIONS: Section[] = [
  { href: "/data", label: "Data", icon: Database, testid: "nav-data", ready: true },
  { href: "/backtests", label: "Backtests", icon: FlaskConical, testid: "nav-backtests", ready: true },
  { href: "/hyperopt", label: "Hyperopt", icon: Sparkles, testid: "nav-hyperopt", ready: false },
  { href: "/live", label: "Live", icon: Activity, testid: "nav-live", ready: false },
  { href: "/ops", label: "Ops", icon: Wrench, testid: "nav-ops", ready: false },
];

function ThemeToggle() {
  const { resolvedTheme, setTheme } = useTheme();
  const [mounted, setMounted] = useState(false);
  // Hydration guard: theme is only known client-side, so defer icon/label until
  // mounted to avoid an SSR/CSR mismatch. This one-shot setState is the
  // canonical next-themes pattern.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => setMounted(true), []);
  const isDark = resolvedTheme === "dark";
  return (
    <button
      type="button"
      data-testid="theme-toggle"
      aria-label="Toggle theme"
      onClick={() => setTheme(isDark ? "light" : "dark")}
      className="flex h-8 w-full items-center gap-2 rounded-lg px-3 text-sm text-sidebar-foreground/70 transition-colors hover:bg-sidebar-accent hover:text-sidebar-foreground"
    >
      {mounted ? (
        isDark ? <Moon className="size-4" /> : <Sun className="size-4" />
      ) : (
        <Moon className="size-4" />
      )}
      <span>{mounted && !isDark ? "Light" : "Dark"} theme</span>
    </button>
  );
}

export function Sidebar() {
  const pathname = usePathname();
  return (
    <aside
      data-testid="sidebar"
      className="flex w-56 shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground"
    >
      <div className="flex h-14 items-center gap-2 border-b border-sidebar-border px-4">
        <CircleDot className="size-5 text-sidebar-primary" />
        <span className="font-semibold tracking-tight">tms</span>
        <span className="ml-auto text-[10px] uppercase tracking-widest text-muted-foreground">
          control plane
        </span>
      </div>

      <nav className="flex flex-1 flex-col gap-0.5 p-2" data-testid="nav">
        {SECTIONS.map((s) => {
          const active =
            pathname === s.href || pathname.startsWith(`${s.href}/`);
          const Icon = s.icon;
          return (
            <Link
              key={s.href}
              href={s.href}
              data-testid={s.testid}
              data-active={active ? "true" : "false"}
              aria-current={active ? "page" : undefined}
              className={cn(
                "group flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm font-medium transition-colors",
                active
                  ? "bg-sidebar-accent text-sidebar-accent-foreground"
                  : "text-sidebar-foreground/70 hover:bg-sidebar-accent/60 hover:text-sidebar-foreground",
              )}
            >
              <Icon className="size-4 shrink-0" />
              <span className="flex-1">{s.label}</span>
              {!s.ready ? (
                <Badge variant="muted" className="h-4 px-1.5 text-[10px]">
                  P2+
                </Badge>
              ) : null}
            </Link>
          );
        })}
      </nav>

      <div className="border-t border-sidebar-border p-2">
        <ThemeToggle />
      </div>
    </aside>
  );
}
