package runner

// export_test.go exposes internal seams to the runner_test package for tests
// that must drive otherwise-unexported lifecycle steps (e.g. halt rehydration)
// without standing up a full live session + moomoo mock.

import (
	"context"

	"github.com/byjackchen/trade-tms-go/internal/commands"
)

// SetSessionIDForTest sets the open session id used to scope halt rows.
func (l *Live) SetSessionIDForTest(id int64) {
	l.mu.Lock()
	l.sessionID = id
	l.mu.Unlock()
}

// RecordHaltForTest persists an active halt row scoped to the test session.
func (l *Live) RecordHaltForTest(ctx context.Context, kind, reason string) {
	l.recordHalt(ctx, commands.HaltKind(kind), reason)
}

// ClearHaltForTest clears the active halt rows for the test session.
func (l *Live) ClearHaltForTest(ctx context.Context, by string) { l.clearHalt(ctx, by) }

// RehydrateHaltForTest runs the restart-time halt rehydration.
func (l *Live) RehydrateHaltForTest(ctx context.Context) { l.rehydrateHalt(ctx) }
