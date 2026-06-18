import { Construction } from "lucide-react";

/** Placeholder body for nav sections not yet implemented (P2+). */
export function ComingSoon({
  section,
  testid,
}: {
  section: string;
  testid: string;
}) {
  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <div
        data-testid={`${testid}-placeholder`}
        className="flex max-w-md flex-col items-center gap-3 rounded-xl border border-dashed border-border bg-card/40 px-8 py-12 text-center"
      >
        <Construction className="size-8 text-muted-foreground" />
        <h2 className="text-base font-medium">{section} — coming in P2+</h2>
        <p className="text-sm text-muted-foreground">
          This section is reserved. The P1 control plane ships the Data
          workspace; {section.toLowerCase()} lands in a later phase.
        </p>
      </div>
    </main>
  );
}
