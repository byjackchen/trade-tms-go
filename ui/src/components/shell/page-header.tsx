import * as React from "react";

/** Sticky page header with title, optional subtitle and right-aligned actions. */
export function PageHeader({
  title,
  subtitle,
  actions,
  "data-testid": testId,
}: {
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  "data-testid"?: string;
}) {
  return (
    <header
      data-testid={testId}
      className="sticky top-0 z-10 flex h-14 items-center gap-4 border-b border-border bg-background/80 px-6 backdrop-blur"
    >
      <div className="min-w-0">
        <h1 className="truncate text-sm font-semibold">{title}</h1>
        {subtitle ? (
          <p className="truncate text-xs text-muted-foreground">{subtitle}</p>
        ) : null}
      </div>
      {actions ? <div className="ml-auto flex items-center gap-2">{actions}</div> : null}
    </header>
  );
}
