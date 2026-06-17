package runner

// resolve_account_test.go is the DB-free unit coverage for the account-attribution
// core of phase 3: (*Live).resolveAccount maps the control-plane mode onto the ONE
// domain.Account a node binds (signal -> synthetic sim, paper -> moomoo SIMULATE
// @ PaperAccID, live -> moomoo REAL @ LiveAccID). This is the single highest-risk
// surface (real-money account selection), so it is pinned independently of any
// PostgreSQL/integration harness.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func TestResolveAccount(t *testing.T) {
	const (
		paperAccID uint64 = 111000111
		liveAccID  uint64 = 283445331237495693
	)
	l := &Live{cfg: LiveConfig{PaperAccID: paperAccID, LiveAccID: liveAccID}}

	tests := []struct {
		name       string
		mode       string
		wantID     string
		wantVenue  string
		wantEnv    domain.BrokerEnv
		wantBroker uint64
		wantIsReal bool
	}{
		{
			name:       "signal -> synthetic sim account (no broker, no real money)",
			mode:       modeSignal,
			wantID:     "sim:signal",
			wantVenue:  "sim",
			wantEnv:    domain.EnvSim,
			wantBroker: 0,
			wantIsReal: false,
		},
		{
			name:       "paper -> moomoo SIMULATE @ PaperAccID",
			mode:       modePaper,
			wantID:     domain.NewBrokerAccount("moomoo", domain.EnvSimulate, paperAccID, "").ID,
			wantVenue:  "moomoo",
			wantEnv:    domain.EnvSimulate,
			wantBroker: paperAccID,
			wantIsReal: false,
		},
		{
			name:       "live -> moomoo REAL @ LiveAccID (the real-money mapping)",
			mode:       modeLive,
			wantID:     domain.NewBrokerAccount("moomoo", domain.EnvReal, liveAccID, "").ID,
			wantVenue:  "moomoo",
			wantEnv:    domain.EnvReal,
			wantBroker: liveAccID,
			wantIsReal: true,
		},
		{
			name:       "unknown mode falls back to the synthetic sim account (never real money)",
			mode:       "bogus",
			wantID:     "sim:signal",
			wantVenue:  "sim",
			wantEnv:    domain.EnvSim,
			wantBroker: 0,
			wantIsReal: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acct := l.resolveAccount(tc.mode)
			assert.Equal(t, tc.wantID, acct.ID, "account id")
			assert.Equal(t, tc.wantVenue, acct.Venue, "venue")
			assert.Equal(t, tc.wantEnv, acct.Env, "account env")
			assert.Equal(t, tc.wantBroker, acct.BrokerAccID, "broker acc id")
			assert.Equal(t, tc.wantIsReal, acct.IsReal(), "IsReal()")
			// Every resolved account must be well-formed (it is the FK target written
			// to tms.accounts before the session/order rows reference it).
			assert.NoError(t, acct.Validate(), "resolved account validates")
		})
	}
}
