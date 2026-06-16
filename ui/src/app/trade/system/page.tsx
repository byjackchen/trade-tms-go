import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/trade/live-indicator";
import { TradeTabs } from "@/components/trade/trade-tabs";
import { SystemPanel } from "@/components/trade/system-panel";
import { SessionControls } from "@/components/trade/session-controls";
import { PreflightPanel } from "@/components/trade/preflight-panel";

export default function TradeSystemPage() {
  return (
    <>
      <PageHeader
        title="System"
        subtitle="Connection status and session controls."
        data-testid="live-system-header"
        actions={<LiveIndicator />}
      />
      <TradeTabs />

      <main
        className="mx-auto w-full max-w-5xl flex-1 space-y-4 p-6"
        data-testid="live-system-page"
      >
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <SystemPanel />
          <SessionControls />
        </div>
        <PreflightPanel />
      </main>
    </>
  );
}
