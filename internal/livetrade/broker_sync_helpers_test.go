package livetrade_test

// broker_sync_helpers_test.go holds the shared test harness for the DIRECTION-2
// broker-sync desk (see broker_sync_test.go). It builds a paper-bound
// BrokerSyncController over the in-memory mock venue (no OpenD, no PG) plus a
// reconciler wired to the desk's OWN EXTERNAL book, so SyncFromBroker's read-only
// reflection can be reconciled (broker truth vs the reflected EXTERNAL book).

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
)

const syncPaperAcc = uint64(99001)

// memAudit captures broker-sync audit records.
type memAudit struct {
	mu   sync.Mutex
	recs []livetrade.SyncAuditRecord
}

func (m *memAudit) RecordSyncAction(_ context.Context, a livetrade.SyncAuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, a)
	return nil
}
func (m *memAudit) all() []livetrade.SyncAuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]livetrade.SyncAuditRecord, len(m.recs))
	copy(out, m.recs)
	return out
}
func (m *memAudit) byAction(action string) []livetrade.SyncAuditRecord {
	var out []livetrade.SyncAuditRecord
	for _, r := range m.all() {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// syncHarness bundles a built broker-sync desk + its venue + sinks.
type syncHarness struct {
	venue   *moexec.MockVenue
	exec    *moexec.MoomooExecutor
	account *livetrade.AccountAdapter
	audit   *memAudit
	report  *memReport
	mc      *livetrade.BrokerSyncController
}

// newPaperSync builds a paper-bound broker-sync desk over the mock venue, with a
// reconciler wired to the same venue + the desk's OWN EXTERNAL book.
func newPaperSync(t *testing.T, navUSD float64) *syncHarness {
	t.Helper()
	ctx := context.Background()
	venue := moexec.NewMockVenue(syncPaperAcc)
	acct := accounting.NewAccount(domain.MustMoney(ftoa(navUSD)), nil)
	account := livetrade.NewAccountAdapter(acct)
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, syncPaperAcc, "paper")
	exec, err := moexec.New(ctx, moexec.Config{
		Account:  paperAcct,
		Client:   venue,
		TraderID: "PAPER-TEST-001",
		Sink:     &fillSink{},
		Book:     account,
	})
	require.NoError(t, err)

	audit := &memAudit{}
	report := &memReport{}
	rec, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker: venue,
		Books:  account,
		Sink:   report,
		AccID:  syncPaperAcc,
		Env:    mo.TrdEnvSimulate,
	})
	require.NoError(t, err)
	mc, err := livetrade.NewBrokerSyncController(livetrade.BrokerSyncControllerConfig{
		Acct:       paperAcct,
		Executor:   exec,
		Account:    account,
		Audit:      audit,
		Reconciler: rec,
		NAV:        domain.MustMoney(ftoa(navUSD)),
	})
	require.NoError(t, err)
	return &syncHarness{venue: venue, exec: exec, account: account, audit: audit, report: report, mc: mc}
}
