package commands

import (
	"fmt"
	"strings"
)

// Target is the component a command is addressed to. The live node consumes
// target == TargetLive.
const TargetLive = "live"

// Name is a control command name.
type Name string

const (
	NameStart   Name = "start"
	NameStop    Name = "stop"
	NameSetMode Name = "set_mode"
	NameHalt    Name = "halt"
	NameResume  Name = "resume"
	NameKill    Name = "kill"
	// NameFlatten closes ALL open positions with FLAT market orders (P6 decision
	// 7). Paper/live only; confirmation-gated. signal mode has no positions.
	NameFlatten Name = "flatten"
	// NameEmergencyKill is the panic button (P6 decision 5): halt + flatten +
	// stop, in that order. Confirmation-gated.
	NameEmergencyKill Name = "emergency_kill"
	// NameReconcile triggers an on-demand reconciliation (P6 decision 5): the
	// live node compares broker positions vs strategy books and writes a report.
	// Read-only (no auto-correct), so no confirmation required.
	NameReconcile Name = "reconcile"
)

// IsValid reports whether n is a known command.
func (n Name) IsValid() bool {
	switch n {
	case NameStart, NameStop, NameSetMode, NameHalt, NameResume, NameKill,
		NameFlatten, NameEmergencyKill, NameReconcile:
		return true
	default:
		return false
	}
}

// RequiresConfirmation reports whether a command needs an operator confirmation
// token at the API boundary. set_mode to paper/live mutates real risk, so it is
// gated; halt/resume/kill/stop/start are always allowed (kill/halt are
// safety actions that must never be blocked — api task requirement).
//
// The argument is the resolved mode for set_mode ("" for other commands). Only
// set_mode->paper|live requires confirmation; set_mode->signal does not.
// flatten + emergency_kill (destructive position close) ALWAYS require
// confirmation (P6 decision 5/7).
func RequiresConfirmation(n Name, mode string) bool {
	switch n {
	case NameFlatten, NameEmergencyKill:
		return true
	case NameSetMode:
		m := strings.ToLower(strings.TrimSpace(mode))
		return m == "paper" || m == "live"
	default:
		return false
	}
}

// Command is one decoded control-plane request from tms.commands.
type Command struct {
	// ID is the tms.commands primary key.
	ID int64
	// Source is api|cli|ui|system.
	Source string
	// Target is the component id (TargetLive for the live node).
	Target string
	// Name is the control command.
	Name Name
	// Args is the decoded args (e.g. {"mode":"signal"} for set_mode,
	// {"reason":"..."} for halt/kill).
	Args CommandArgs
	// RequestedBy is the actor that enqueued the command (audit).
	RequestedBy string
}

// CommandArgs is the typed args of a command (a superset; only the relevant
// fields are populated per command).
type CommandArgs struct {
	// Mode is the target mode for set_mode (signal|paper|live).
	Mode string `json:"mode,omitempty"`
	// Reason is the operator note for halt/kill/stop (audit).
	Reason string `json:"reason,omitempty"`
	// ConfirmToken is the confirmation token for live/paper set_mode.
	ConfirmToken string `json:"confirm_token,omitempty"`
	// TraderID optionally scopes the command to a specific node; empty means the
	// consuming node applies it (single-node-per-target is the common case).
	TraderID string `json:"trader_id,omitempty"`
}

// Validate checks a command's args are well-formed for its name.
func (c Command) Validate() error {
	if !c.Name.IsValid() {
		return fmt.Errorf("commands: unknown command %q", c.Name)
	}
	if c.Name == NameSetMode {
		m := strings.ToLower(strings.TrimSpace(c.Args.Mode))
		switch m {
		case "signal", "paper", "live":
		default:
			return fmt.Errorf("commands: set_mode requires mode in {signal,paper,live}, got %q", c.Args.Mode)
		}
	}
	return nil
}
