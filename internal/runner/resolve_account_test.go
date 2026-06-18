package runner

// resolve_account_test.go is the DB-free unit coverage for the account-attribution
// core: (*Live).resolveAccount returns the ONE domain.Account a node binds for
// EVERY mode — the CLI-resolved BoundAccount (loaded from tms.accounts by --account
// id or the (venue, env) default; signal gets the (moomoo, paper) default as a
// nominal placeholder, or the zero Account when none exists). This is the
// highest-risk surface (real-money account selection), so it is pinned
// independently of any PostgreSQL/integration harness.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func TestResolveAccount(t *testing.T) {
	// With a bound paper account set on LiveConfig, EVERY mode (signal included)
	// resolves to that exact account.
	bound := domain.Account{
		ID:          "acct_3f2b1a90-paper",
		Venue:       "moomoo",
		Env:         domain.EnvPaper,
		BrokerAccID: 111000111,
		Label:       "paper book",
	}
	l := &Live{cfg: LiveConfig{BoundAccount: bound}}
	for _, mode := range []string{modeSignal, modePaper, modeLive} {
		t.Run("bound/"+mode, func(t *testing.T) {
			acct := l.resolveAccount(mode)
			assert.Equal(t, bound, acct, "resolved account")
			// A well-formed bound account is the FK target written to tms.accounts
			// before the session/order rows reference it.
			assert.NoError(t, acct.Validate(), "resolved account validates")
		})
	}

	// With NO bound account (no paper default existed at CLI time), every mode
	// resolves to the zero Account — an empty id maps to a NULL account_id downstream.
	empty := &Live{cfg: LiveConfig{}}
	for _, mode := range []string{modeSignal, modePaper, modeLive} {
		t.Run("empty/"+mode, func(t *testing.T) {
			assert.Equal(t, domain.Account{}, empty.resolveAccount(mode), "resolved account")
		})
	}
}
