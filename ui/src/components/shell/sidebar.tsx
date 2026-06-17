"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { CircleDot } from "lucide-react";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { NAV_SECTIONS, isSectionActive } from "@/components/shell/nav";

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
        {NAV_SECTIONS.map((s) => {
          const active = isSectionActive(pathname, s.href);
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
    </aside>
  );
}
