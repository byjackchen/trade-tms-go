import * as React from "react";
import { cn } from "@/lib/utils";

/** Styled native checkbox; spreads through `data-testid`, `checked`, etc. */
function Checkbox({ className, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type="checkbox"
      data-slot="checkbox"
      className={cn(
        "size-4 shrink-0 cursor-pointer rounded border-input accent-primary outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50",
        className,
      )}
      {...props}
    />
  );
}

export { Checkbox };
