package db

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSleep records requested delays without actually sleeping.
type fakeSleep struct {
	delays []time.Duration
	err    error // returned from every sleep call when non-nil
}

func (f *fakeSleep) sleep(_ context.Context, d time.Duration) error {
	f.delays = append(f.delays, d)
	return f.err
}

func testPolicy(fs *fakeSleep) BackoffPolicy {
	p := DefaultBackoff()
	p.JitterFrac = 0 // deterministic delays for assertions
	p.sleep = fs.sleep
	return p
}

func TestRetrySucceedsFirstAttempt(t *testing.T) {
	fs := &fakeSleep{}
	calls := 0
	err := Retry(context.Background(), testPolicy(fs), func(context.Context) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Empty(t, fs.delays, "no backoff on immediate success")
}

func TestRetryTransientThenSuccess(t *testing.T) {
	fs := &fakeSleep{}
	calls := 0
	err := Retry(context.Background(), testPolicy(fs), func(context.Context) error {
		calls++
		if calls < 3 {
			return syscall.ECONNREFUSED
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	// 100ms then 200ms: capped exponential, multiplier 2, no jitter.
	assert.Equal(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, fs.delays)
}

func TestRetryNonTransientReturnsImmediatelyAndUnwrapped(t *testing.T) {
	fs := &fakeSleep{}
	calls := 0
	syntaxErr := &pgconn.PgError{Code: pgerrcode.SyntaxError, Message: "syntax error at or near"}
	err := Retry(context.Background(), testPolicy(fs), func(context.Context) error {
		calls++
		return syntaxErr
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls, "non-transient errors must not be retried")
	assert.Empty(t, fs.delays)
	assert.Same(t, syntaxErr, err, "non-transient error must be returned as-is")
}

func TestRetryExhaustsAttempts(t *testing.T) {
	fs := &fakeSleep{}
	calls := 0
	cause := &pgconn.PgError{Code: pgerrcode.SerializationFailure}
	err := Retry(context.Background(), testPolicy(fs), func(context.Context) error {
		calls++
		return cause
	})
	require.Error(t, err)
	assert.Equal(t, 4, calls, "DefaultBackoff allows 4 total attempts")
	assert.Len(t, fs.delays, 3, "3 waits between 4 attempts")
	assert.Contains(t, err.Error(), "gave up after 4 attempts")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "cause must survive wrapping")
	assert.Equal(t, pgerrcode.SerializationFailure, pgErr.Code)
}

func TestRetryContextCancelledDuringBackoff(t *testing.T) {
	fs := &fakeSleep{err: context.Canceled}
	calls := 0
	err := Retry(context.Background(), testPolicy(fs), func(context.Context) error {
		calls++
		return io.ErrUnexpectedEOF
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.True(t, errors.Is(err, context.Canceled), "cancellation must be matchable")
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF), "last transient cause must be matchable")
}

func TestRetryContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Retry(ctx, testPolicy(&fakeSleep{}), func(context.Context) error {
		calls++
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, calls, "op must not run on a dead context")
}

func TestRetryRejectsInvalidPolicy(t *testing.T) {
	err := Retry(context.Background(), BackoffPolicy{MaxAttempts: 0}, func(context.Context) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MaxAttempts")
}

func TestRetryCustomClassifier(t *testing.T) {
	fs := &fakeSleep{}
	p := testPolicy(fs)
	sentinel := errors.New("flaky-but-known")
	p.IsTransient = func(err error) bool { return errors.Is(err, sentinel) }
	calls := 0
	err := Retry(context.Background(), p, func(context.Context) error {
		calls++
		if calls == 1 {
			return sentinel
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestDelaySchedule(t *testing.T) {
	p := BackoffPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second, Multiplier: 2}
	want := []time.Duration{
		100 * time.Millisecond, // attempt 1
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		2 * time.Second, // capped
		2 * time.Second, // stays capped
	}
	for i, w := range want {
		assert.Equal(t, w, p.Delay(i+1), "attempt %d", i+1)
	}
	assert.Equal(t, time.Duration(0), p.Delay(0), "attempt < 1 yields no delay")

	// Very large attempt numbers must not overflow into negatives.
	assert.Equal(t, 2*time.Second, p.Delay(10_000))
}

func TestDelayJitterBounds(t *testing.T) {
	p := BackoffPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second, Multiplier: 2, JitterFrac: 0.2}
	for range 200 {
		d := p.Delay(2) // nominal 200ms, jittered ±20%
		assert.GreaterOrEqual(t, d, 160*time.Millisecond)
		assert.LessOrEqual(t, d, 240*time.Millisecond)
	}
}

func TestSleepCtx(t *testing.T) {
	// Zero/negative duration returns immediately with the context status.
	require.NoError(t, sleepCtx(context.Background(), 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sleepCtx(ctx, time.Hour), context.Canceled)

	// A short real sleep completes without error.
	require.NoError(t, sleepCtx(context.Background(), time.Millisecond))
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped context canceled", context.Canceled, false},
		{"pg connection exception (08006)", &pgconn.PgError{Code: pgerrcode.ConnectionFailure}, true},
		{"pg serialization failure (40001)", &pgconn.PgError{Code: pgerrcode.SerializationFailure}, true},
		{"pg deadlock (40P01)", &pgconn.PgError{Code: pgerrcode.DeadlockDetected}, true},
		{"pg cannot connect now (57P03)", &pgconn.PgError{Code: pgerrcode.CannotConnectNow}, true},
		{"pg admin shutdown (57P01)", &pgconn.PgError{Code: pgerrcode.AdminShutdown}, true},
		{"pg too many connections (53300)", &pgconn.PgError{Code: pgerrcode.TooManyConnections}, true},
		{"pg syntax error (42601)", &pgconn.PgError{Code: pgerrcode.SyntaxError}, false},
		{"pg unique violation (23505)", &pgconn.PgError{Code: pgerrcode.UniqueViolation}, false},
		{"net op error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, true},
		{"econnrefused", syscall.ECONNREFUSED, true},
		{"econnreset", syscall.ECONNRESET, true},
		{"epipe", syscall.EPIPE, true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsTransient(tc.err))
		})
	}
}
