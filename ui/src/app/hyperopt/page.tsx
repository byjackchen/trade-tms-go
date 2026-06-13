"use client";

import { useState } from "react";
import { Sparkles } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { Button } from "@/components/ui/button";
import { StreamIndicator } from "@/components/data/stream-indicator";
import { StudiesTable } from "@/components/hyperopt/studies-table";
import { NewStudyDialog } from "@/components/hyperopt/new-study-dialog";

export default function HyperoptPage() {
  const [newOpen, setNewOpen] = useState(false);
  const [strategy, setStrategy] = useState("");

  return (
    <>
      <PageHeader
        title="Hyperopt"
        subtitle="Seeded NSGA-II walk-forward hyper-parameter studies."
        data-testid="hyperopt-header"
        actions={
          <>
            <StreamIndicator />
            <Button
              size="sm"
              onClick={() => setNewOpen(true)}
              data-testid="open-hyperopt-dialog"
            >
              <Sparkles />
              New study
            </Button>
          </>
        }
      />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="hyperopt-page"
      >
        <StudiesTable strategy={strategy} onStrategyChange={setStrategy} />
      </main>

      {newOpen ? (
        <NewStudyDialog open={newOpen} onClose={() => setNewOpen(false)} />
      ) : null}
    </>
  );
}
