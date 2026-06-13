package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okHandler(kind string) Handler {
	return HandlerFunc{K: kind, F: func(context.Context, *Job, ProgressFn) (any, error) {
		return nil, nil
	}}
}

func TestRegistryRegisterResolve(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(okHandler("data.refresh")))
	require.NoError(t, r.Register(okHandler("a.b")))

	assert.NotNil(t, r.Resolve("data.refresh"))
	assert.Nil(t, r.Resolve("unknown.kind"))
	assert.Equal(t, []string{"a.b", "data.refresh"}, r.Kinds())
}

func TestRegistryRejectsDuplicatesAndEmptyKind(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(okHandler("x")))
	err := r.Register(okHandler("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate handler")

	err = r.Register(okHandler(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty kind")

	assert.Panics(t, func() { r.MustRegister(okHandler("x")) })
}

func TestWorkerOptionsValidation(t *testing.T) {
	t.Run("defaults fill in", func(t *testing.T) {
		o := WorkerOptions{}
		require.NoError(t, o.applyDefaults())
		assert.Equal(t, DefaultConcurrency, o.Concurrency)
		assert.NotEmpty(t, o.ID)
	})
	t.Run("negative concurrency rejected", func(t *testing.T) {
		o := WorkerOptions{Concurrency: -1}
		require.Error(t, o.applyDefaults())
	})
	t.Run("heartbeat must clear stale TTL", func(t *testing.T) {
		o := WorkerOptions{HeartbeatInterval: DefaultStaleAfter} // == TTL: live jobs would be reaped
		err := o.applyDefaults()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale TTL")
	})
}

func TestStatusTerminal(t *testing.T) {
	assert.False(t, StatusQueued.Terminal())
	assert.False(t, StatusRunning.Terminal())
	assert.True(t, StatusSucceeded.Terminal())
	assert.True(t, StatusFailed.Terminal())
	assert.True(t, StatusCanceled.Terminal())
}
