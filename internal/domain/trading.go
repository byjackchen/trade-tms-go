package domain

// trading.go defines the two ORTHOGONAL axes the platform's run configuration
// splits into, plus the first-class Account entity — replacing the conflated
// Mode {signal,paper,live} enum (see docs/design/trade-refactor.md).
//
//   - ExecutionPolicy: what happens to a strategy signal (emit-only vs auto-submit).
//   - Account (with BrokerEnv): WHERE orders settle (a specific sim/paper/real
//     broker account). "paper vs live" is a property of the account, not a mode.
//
// Legacy Mode maps onto these via Mode.ExecutionPolicy()/Mode.AccountEnv() during
// the migration; once all call sites carry (ExecutionPolicy, Account) the Mode
// enum is removed.

import "fmt"

// ExecutionPolicy is what the engine does with a strategy's signal.
type ExecutionPolicy string

const (
	// ExecSignal emits signal intents only — NO automated orders. The operator
	// executes manually (the trade desk) or not at all.
	ExecSignal ExecutionPolicy = "signal"
	// ExecAuto auto-submits a strategy's intents as orders against the bound account.
	ExecAuto ExecutionPolicy = "auto"
)

// IsValid reports whether p is a known ExecutionPolicy.
func (p ExecutionPolicy) IsValid() bool { return p == ExecSignal || p == ExecAuto }

// String returns the wire value.
func (p ExecutionPolicy) String() string { return string(p) }

// ParseExecutionPolicy validates and returns the ExecutionPolicy for s.
func ParseExecutionPolicy(s string) (ExecutionPolicy, error) {
	v := ExecutionPolicy(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown ExecutionPolicy %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// BrokerEnv is the environment an Account lives in.
type BrokerEnv string

const (
	// EnvSim is a SYNTHETIC account with no broker — backtest/hyperopt sim fills.
	EnvSim BrokerEnv = "sim"
	// EnvSimulate is a broker PAPER (simulated) account.
	EnvSimulate BrokerEnv = "simulate"
	// EnvReal is a broker REAL-money account.
	EnvReal BrokerEnv = "real"
)

// IsValid reports whether e is a known BrokerEnv.
func (e BrokerEnv) IsValid() bool {
	switch e {
	case EnvSim, EnvSimulate, EnvReal:
		return true
	}
	return false
}

// IsReal reports whether e is the real-money environment.
func (e BrokerEnv) IsReal() bool { return e == EnvReal }

// IsBroker reports whether e is a real broker account (paper or real) vs a
// synthetic sim account.
func (e BrokerEnv) IsBroker() bool { return e == EnvSimulate || e == EnvReal }

// String returns the wire value.
func (e BrokerEnv) String() string { return string(e) }

// Account is a first-class trading account: a stable TMS identity over a specific
// broker account (or a synthetic sim account). Orders/positions/fills attribute
// to an Account, so positions can be managed per account.
type Account struct {
	// ID is the stable TMS account id, used as the DB key and in attribution.
	// Broker accounts: "<venue>:<env>:<brokerAccID>" (e.g. "moomoo:real:283445331237495693").
	// Sim accounts: "sim:<name>" (e.g. "sim:default").
	ID string `json:"id"`
	// Venue is the broker/venue: "moomoo" | "sim".
	Venue string `json:"venue"`
	// Env is the account environment.
	Env BrokerEnv `json:"env"`
	// BrokerAccID is the broker's account id (0 for sim accounts).
	BrokerAccID uint64 `json:"broker_acc_id"`
	// Label is a human-friendly label (e.g. "保证金账户(3063)"); optional.
	Label string `json:"label,omitempty"`
}

// NewBrokerAccount builds a broker-backed Account with a deterministic ID.
func NewBrokerAccount(venue string, env BrokerEnv, brokerAccID uint64, label string) Account {
	return Account{
		ID:          fmt.Sprintf("%s:%s:%d", venue, env, brokerAccID),
		Venue:       venue,
		Env:         env,
		BrokerAccID: brokerAccID,
		Label:       label,
	}
}

// SimAccount builds a synthetic sim Account (backtest/hyperopt) named name.
func SimAccount(name string) Account {
	if name == "" {
		name = "default"
	}
	return Account{ID: "sim:" + name, Venue: "sim", Env: EnvSim}
}

// IsReal reports whether the account is a real-money account.
func (a Account) IsReal() bool { return a.Env.IsReal() }

// IsBroker reports whether the account is broker-backed (paper or real).
func (a Account) IsBroker() bool { return a.Env.IsBroker() }

// Validate checks the Account is well-formed.
func (a Account) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("%w: account id required", ErrInvalidArgument)
	}
	if !a.Env.IsValid() {
		return fmt.Errorf("%w: invalid account env %q", ErrInvalidArgument, a.Env)
	}
	if a.Env.IsBroker() && a.BrokerAccID == 0 {
		return fmt.Errorf("%w: broker account %q requires a broker_acc_id", ErrInvalidArgument, a.ID)
	}
	return nil
}

// ---- legacy Mode bridge (removed once all call sites carry the two axes) ----

// ExecutionPolicy maps the legacy Mode onto the execution axis: signal → emit-only,
// paper/live → auto-submit.
func (m Mode) ExecutionPolicy() ExecutionPolicy {
	if m == ModeSignal {
		return ExecSignal
	}
	return ExecAuto
}

// AccountEnv maps the legacy Mode onto the account-environment axis: paper →
// simulate, live → real, signal → sim (informational; signal holds no account).
func (m Mode) AccountEnv() BrokerEnv {
	switch m {
	case ModePaper:
		return EnvSimulate
	case ModeLive:
		return EnvReal
	default:
		return EnvSim
	}
}
