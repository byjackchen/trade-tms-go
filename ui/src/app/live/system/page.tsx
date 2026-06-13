import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/live/live-indicator";
import { LiveTabs } from "@/components/live/live-tabs";
import { SystemPanel } from "@/components/live/system-panel";
import { SessionControls } from "@/components/live/session-controls";

export default function LiveSystemPage() {
  return (
    <>
      <PageHeader
        title="System"
        subtitle="Connection status and session controls."
        data-testid="live-system-header"
        actions={<LiveIndicator />}
      />
      <LiveTabs />

      <main
        className="mx-auto w-full max-w-5xl flex-1 space-y-4 p-6"
        data-testid="live-system-page"
      >
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <SystemPanel />
          <SessionControls />
        </div>
      </main>
    </>
  );
}
