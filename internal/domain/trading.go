package domain

// trading.go defines the two ORTHOGONAL axes the platform's run configuration
// splits into, plus the first-class Account entity — replacing the conflated
// Mode {signal,paper,live} enum (now removed; see docs/concept-alignment.md §1.3).
//
//   - ExecutionPolicy: what happens to a strategy signal (emit-only vs auto-submit).
//   - Account (with BrokerEnv): WHERE orders settle (a specific sim/paper/real
//     broker account). "paper vs live" is a property of the account, not a mode.

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

// RunWord derives the operator-facing convenience word for a run from the two
// orthogonal axes (exec policy + account env). It is NOT a stored type — the
// legacy three-valued Mode {signal,paper,live} enum is gone (docs §1.3); this
// is the read-only label some surfaces still display, ALWAYS derived from
// (exec_policy, account.env), never an independent enum:
//
//	exec=signal            -> "signal" (emit-only; env irrelevant)
//	exec=auto, env=real    -> "live"   (auto-submit against a real-money account)
//	exec=auto, env=*else*  -> "paper"  (auto-submit against a sim/simulate account)
func RunWord(exec ExecutionPolicy, env BrokerEnv) string {
	if exec != ExecAuto {
		return "signal"
	}
	if env == EnvReal {
		return "live"
	}
	return "paper"
}

// AccountKind derives the operator-facing "kind" word for an account from its
// env, following the same rule as RunWord (the env stays the source of truth;
// this is a derived label, never a stored column):
//
//	env=real   -> "live"   (a real-money account)
//	env=*else* -> "paper"  (sim/simulate — no real money)
//
// The unified /trade UI badges each account paper|live from this, and the
// selected account's kind drives the LIVE-red treatment + arm-confirm gating.
func AccountKind(env BrokerEnv) string {
	if env == EnvReal {
		return "live"
	}
	return "paper"
}

// BrokerEnv is the environment an Account lives in.
type BrokerEnv string

const (
	// EnvPaper is a broker PAPER (simulated) account.
	EnvPaper BrokerEnv = "paper"
	// EnvReal is a broker REAL-money account.
	EnvReal BrokerEnv = "real"
)

// IsValid reports whether e is a known BrokerEnv.
func (e BrokerEnv) IsValid() bool {
	switch e {
	case EnvPaper, EnvReal:
		return true
	}
	return false
}

// IsReal reports whether e is the real-money environment.
func (e BrokerEnv) IsReal() bool { return e == EnvReal }

// IsBroker reports whether e is a broker account. Every valid env is now
// broker-backed (the synthetic simu account was removed); kept for call-site clarity.
func (e BrokerEnv) IsBroker() bool { return e == EnvPaper || e == EnvReal }

// String returns the wire value.
func (e BrokerEnv) String() string { return string(e) }

// Account is a first-class trading account: a stable TMS identity over a specific
// broker account (always broker-backed). Orders/positions/fills attribute to an
// Account, so positions can be managed per account.
type Account struct {
	// ID is the stable TMS account id, used as the DB key and in attribution. It is
	// an OPAQUE surrogate (decoupled from venue/env/broker_acc_id so those can be
	// edited without breaking FK history): user-created broker accounts get
	// "acct_<uuid>"; legacy broker rows keep their original
	// "<venue>:<env>:<brokerAccID>" id (still opaque).
	ID string `json:"id"`
	// Venue is the broker/venue: "moomoo".
	Venue string `json:"venue"`
	// Env is the account environment.
	Env BrokerEnv `json:"env"`
	// BrokerAccID is the broker's account id.
	BrokerAccID uint64 `json:"broker_acc_id"`
	// Label is a human-friendly label (e.g. "保证金账户(3063)"); optional.
	Label string `json:"label,omitempty"`
	// IsDefault marks this as THE default account for its (venue, env) — the one a
	// `tms trade run --env paper|real` binds when no explicit account is given.
	IsDefault bool `json:"is_default"`
	// Notes is a free-text operator note; optional.
	Notes string `json:"notes,omitempty"`
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
