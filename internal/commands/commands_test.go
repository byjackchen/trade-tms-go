package commands

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHaltStateGate(t *testing.T) {
	h := NewHaltState(func() time.Time { return time.Unix(0, 0).UTC() })
	assert.True(t, h.Emitting(), "fresh state emits")
	assert.False(t, h.IsHalted())
	assert.False(t, h.IsStopped())

	h.Halt(HaltManual, "operator")
	assert.False(t, h.Emitting(), "halted stops emission")
	assert.True(t, h.IsHalted())

	snap := h.Snapshot()
	assert.True(t, snap.Halted)
	assert.Equal(t, HaltManual, snap.Kind)
	assert.Equal(t, "operator", snap.Reason)

	// Re-halt keeps the first reason (idempotent).
	h.Halt(HaltDailyLoss, "other")
	assert.Equal(t, "operator", h.Snapshot().Reason)

	h.Resume()
	assert.True(t, h.Emitting(), "resume restores emission")
	assert.False(t, h.IsHalted())

	h.Stop()
	assert.False(t, h.Emitting(), "stopped stops emission")
	assert.True(t, h.IsStopped())
	// Resume does NOT clear a stop.
	h.Resume()
	assert.False(t, h.Emitting(), "resume cannot un-stop")
}

func TestCommandValidate(t *testing.T) {
	assert.NoError(t, Command{Name: NameHalt}.Validate())
	assert.NoError(t, Command{Name: NameSetMode, Args: CommandArgs{Mode: "signal"}}.Validate())
	assert.Error(t, Command{Name: NameSetMode, Args: CommandArgs{Mode: "bogus"}}.Validate())
	assert.Error(t, Command{Name: "frobnicate"}.Validate())
}

func TestRequiresConfirmation(t *testing.T) {
	assert.False(t, RequiresConfirmation(NameHalt, ""))
	assert.False(t, RequiresConfirmation(NameKill, ""))
	assert.False(t, RequiresConfirmation(NameSetMode, "signal"))
	assert.True(t, RequiresConfirmation(NameSetMode, "paper"))
	assert.True(t, RequiresConfirmation(NameSetMode, "live"))
	assert.True(t, RequiresConfirmation(NameSetMode, "LIVE"))
}

func TestNameIsValid(t *testing.T) {
	for _, n := range []Name{NameStart, NameStop, NameSetMode, NameHalt, NameResume, NameKill} {
		assert.True(t, n.IsValid(), string(n))
	}
	assert.False(t, Name("nope").IsValid())
}
