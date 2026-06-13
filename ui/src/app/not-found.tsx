import Link from "next/link";
import { buttonVariants } from "@/components/ui/button";

export default function NotFound() {
  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <div
        className="flex max-w-md flex-col items-center gap-3 rounded-xl border border-border bg-card/40 px-8 py-10 text-center"
        data-testid="not-found"
      >
        <span className="text-3xl font-semibold">404</span>
        <p className="text-sm text-muted-foreground">
          This page does not exist in the TMS control plane.
        </p>
        <Link
          href="/data"
          className={buttonVariants({ variant: "default" })}
          data-testid="not-found-home"
        >
          Go to Data
        </Link>
      </div>
    </main>
  );
}
