"use client";

import Link from "next/link";
import { CircleDot } from "lucide-react";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { useNavItems, navLinkProps } from "@/components/shell/nav-item";

export function Sidebar() {
  const items = useNavItems();
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
        {items.map((item) => {
          const { section: s, active } = item;
          const Icon = s.icon;
          return (
            <Link
              key={s.href}
              {...navLinkProps(item)}
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
