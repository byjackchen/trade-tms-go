package runner

// resolve_account_test.go is the DB-free unit coverage for the account-attribution
// core: (*Live).resolveAccount maps the control-plane mode onto the ONE
// domain.Account a node binds — signal (and any unknown mode) -> the synthetic simu
// account (never real money); paper/live -> the DB-resolved BoundAccount (the CLI
// loads it from tms.accounts by --account id or the (venue, env) default). This is
// the highest-risk surface (real-money account selection), so it is pinned
// independently of any PostgreSQL/integration harness.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func TestResolveAccount(t *testing.T) {
	// The bound account is resolved from tms.accounts by the CLI (surrogate id) and
	// handed to the node via LiveConfig before NewLive — paper/live return it verbatim.
	bound := domain.Account{
		ID:          "acct_3f2b1a90-paper",
		Venue:       "moomoo",
		Env:         domain.EnvPaper,
		BrokerAccID: 111000111,
		Label:       "paper book",
	}
	l := &Live{cfg: LiveConfig{BoundAccount: bound}}

	tests := []struct {
		name string
		mode string
		want domain.Account
	}{
		{"signal -> synthetic simu account (no broker, no real money)", modeSignal, domain.SimAccount("signal")},
		{"paper -> the DB-resolved bound account", modePaper, bound},
		{"live -> the DB-resolved bound account", modeLive, bound},
		{"unknown mode falls back to the synthetic simu account (never real money)", "bogus", domain.SimAccount("signal")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acct := l.resolveAccount(tc.mode)
			assert.Equal(t, tc.want, acct, "resolved account")
			// Every resolved account must be well-formed (the FK target written to
			// tms.accounts before the session/order rows reference it).
			assert.NoError(t, acct.Validate(), "resolved account validates")
		})
	}
}
